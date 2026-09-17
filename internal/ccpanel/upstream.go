// 本文件是与 Devin 上游通信的部分：/admin/status 聚合端点、
// 模型目录/供应商/模型状态的拉取与缓存、Connect metadata 构造、鉴权 transport。
package ccpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/upstream"
)

const (
	clientName    = "windsurf"
	clientVersion = "1.48.2"
	// seatUserStatusPath 与 Windsurf 官方 / WindsurfAPI 一致的 JSON Connect 路径。
	// 生成的 connect 包名 ExaSeatManagementPb_SeatManagementService 在上游会 404。
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// panelUpstream 是一次「上游端点」的固化产物：面板自身的 connect client
// 与裸 transport 绑在同一份 base_url/proxy/force_http1 上。endpoint 配置
// 热应用时 SetUpstream 整体重建、原子换指针；在途调用持旧引用跑完。
type panelUpstream struct {
	// baseURL 已归一化去尾随斜杠（尾随斜杠会让 Connect 调用路径出 "//"）。
	baseURL   string
	apiClient devinprotoconnect.ApiServerServiceClient
	// baseTransport 供 seat 类裸 POST 复用代理拨号（它们自带 Bearer，
	// 不能走 apiClient 的 Basic 改写）；退役时 CloseIdleConnections 收
	// idle 池，在途请求持引用跑完。
	baseTransport *http.Transport
}

// newPanelUpstream 按端点参数构建面板的上游调用束：transport 经 tokenFunc
// 每次请求取凭据（adapter 自愈换 token 后面板跟随新值）。面板调用可能
// 遇上游长时思考/排队，610s 超时与 ResponseHeaderTimeout 对齐。
// proxy 串非法等构建失败返回 error，调用方整体不提交。
func newPanelUpstream(baseURL, proxy string, forceHTTP1 bool, tokenFunc func() string) (*panelUpstream, error) {
	base, err := httpproxy.NewTransport(proxy, forceHTTP1)
	if err != nil {
		return nil, fmt.Errorf("proxy transport: %w", err)
	}
	transport := upstream.NewBasicAuthTransportFunc(base, tokenFunc)
	trimmed := strings.TrimRight(baseURL, "/")
	return &panelUpstream{
		baseURL: trimmed,
		apiClient: devinprotoconnect.NewApiServerServiceClient(
			&http.Client{Transport: transport, Timeout: 610 * time.Second},
			trimmed, connect.WithSendGzip()),
		baseTransport: base,
	}, nil
}

// SetUpstream 热换面板上游端点（配置 reload 热路径）：整体重建调用束
// 并原子换指针；构建失败整体不提交，旧端点继续服役。
func (h *Handler) SetUpstream(baseURL, proxy string, forceHTTP1 bool) error {
	up, err := newPanelUpstream(baseURL, proxy, forceHTTP1, h.tokenFunc)
	if err != nil {
		return err
	}
	old := h.upstreamPtr.Swap(up)
	if old != nil {
		old.baseTransport.CloseIdleConnections()
	}
	return nil
}

// currentUpstream 返回当前生效的上游调用束快照；New 之后恒非 nil。
func (h *Handler) currentUpstream() *panelUpstream {
	return h.upstreamPtr.Load()
}

// BaseURL 返回当前生效的上游地址（归一化后），供移植面板投到日志行与
// 调试响应——与面板自身上游调用同一来源，endpoint 热应用后跟随新值。
func (h *Handler) BaseURL() string {
	return h.currentUpstream().baseURL
}

// StatusReport 六路并行聚合上游状态：账户/plan/容量/IDE 状态/模型状态/
// 供应商/别名缺席校验。单路失败只落 *_error 键，不拖垮整体。
func (h *Handler) StatusReport(ctx context.Context) map[string]any {
	result := map[string]any{}
	var resultMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(6)

	// 正确路径：JSON Connect SeatManagement GetUserStatus（Bearer + metadata.api_key）
	go func() {
		defer wg.Done()
		user, plan, planInfo, err := h.fetchUserStatus(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["user_status_error"] = err.Error()
			return
		}
		if user != nil {
			result["user"] = user
		}
		if plan != nil {
			result["plan_status"] = plan
		}
		if planInfo != nil {
			result["plan_info"] = planInfo
		}
	}()

	go func() {
		defer wg.Done()
		capResp, err := h.currentUpstream().apiClient.CheckChatCapacity(ctx, connect.NewRequest(&devinproto.CheckChatCapacityRequest{
			Metadata: upstream.BuildMetadata(h.tokenFunc(), clientName, clientVersion, "win", 32),
		}))
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["capacity_error"] = err.Error()
			return
		}
		result["capacity"] = map[string]any{
			"has_capacity":    capResp.Msg.GetHasCapacity(),
			"message":         capResp.Msg.GetMessage(),
			"active_sessions": capResp.Msg.GetActiveSessions(),
		}
	}()

	go func() {
		defer wg.Done()
		statusResp, err := h.currentUpstream().apiClient.GetStatus(ctx, connect.NewRequest(&devinproto.GetStatusRequest{
			Metadata: upstream.BuildMetadata(h.tokenFunc(), clientName, clientVersion, "win", 32),
		}))
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["status_error"] = err.Error()
			return
		}
		st := statusResp.Msg.GetStatus()
		result["ide_status"] = map[string]any{
			"level":   shortEnum(st.GetLevel().String()),
			"message": st.GetMessage(),
		}
		result["show_review_prompt"] = statusResp.Msg.GetShowReviewPrompt()
	}()

	go func() {
		defer wg.Done()
		statuses, err := h.cachedModelStatuses(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["model_status_error"] = err.Error()
		} else if statuses != nil {
			result["model_statuses"] = statuses
		}
	}()

	go func() {
		defer wg.Done()
		providers, err := h.cachedProviders(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["providers_error"] = err.Error()
		} else if providers != nil {
			result["providers"] = providers
		}
	}()

	// 别名目标缺席校验：devin.aliases 指向的 uid 不在上游目录时，请求
	// 会以模糊的 permission_denied 失败——这个告警此前只在 stderr 里
	// 按请求打一行，抬到 status 让 agent 程序化可得。
	go func() {
		defer wg.Done()
		if h.aliasesFunc == nil {
			return
		}
		// 与其余五路同序：先拉取与计算、末段一次 resultMu 写结果——
		// 慢目录拉取不该把整扇聚合互斥到底，也避免 resultMu→modelsMu
		// 的嵌套锁序日后长成真死锁。
		models, err := h.cachedModels(ctx)
		var absent []string
		var shadowed []string
		if err == nil {
			uids := make(map[string]struct{}, len(models))
			for _, m := range models {
				if uid, ok := m["uid"].(string); ok && uid != "" {
					uids[uid] = struct{}{}
				}
			}
			for name, target := range h.aliasesFunc() {
				target = strings.TrimSpace(target)
				if target == "" {
					continue
				}
				if _, ok := uids[target]; !ok {
					absent = append(absent, name+"→"+target)
				}
				// 别名名本身是目录里的真模型：请求全部被改写，原模型变得
				// 不可达——多半是借用官方名过客户端校验（如 claude-* 名单），
				// 但值得显式留痕，免得日后查"为什么模型行为对不上目录"。
				if _, ok := uids[name]; ok {
					shadowed = append(shadowed, name+"→"+target)
				}
			}
			slices.Sort(shadowed)
		}
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["alias_check_error"] = err.Error()
			return
		}
		if len(absent) > 0 {
			result["alias_targets_absent"] = absent
		}
		if len(shadowed) > 0 {
			result["alias_shadows_catalog"] = shadowed
		}
	}()

	wg.Wait()

	return result
}

// fetchUserStatus 调用官方 seat_management JSON Connect 路径（面板首号身份）。
func (h *Handler) fetchUserStatus(ctx context.Context) (user, plan, planInfo map[string]any, err error) {
	return h.fetchUserStatusAs(ctx, h.tokenFunc())
}

// fetchUserStatusAs 以指定凭据调用 GetUserStatus：号池的逐账号配额采样
// 与面板自身的状态查询共用这一条路径，只是凭据来源不同。
func (h *Handler) fetchUserStatusAs(ctx context.Context, token string) (user, plan, planInfo map[string]any, err error) {
	bodyObj := map[string]any{
		"metadata": map[string]any{
			"api_key":           token,
			"extension_name":    clientName,
			"extension_version": clientVersion,
			"ide_name":          clientName,
			"ide_version":       clientVersion,
			"locale":            "en",
			"os":                "windows",
		},
	}
	payload, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, nil, nil, err
	}
	up := h.currentUpstream()
	url := up.baseURL + seatUserStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+token)

	// 使用不带 Basic 改写的 client，避免 authTransport 覆盖 Bearer；但复用代理 transport。
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	client := &http.Client{Timeout: 610 * time.Second, Transport: up.baseTransport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("GetUserStatus HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("decode GetUserStatus: %w", err)
	}
	us, _ := root["userStatus"].(map[string]any)
	if us == nil {
		us, _ = root["user_status"].(map[string]any)
	}
	if us == nil {
		return nil, nil, nil, fmt.Errorf("GetUserStatus: empty userStatus")
	}

	user = map[string]any{
		"name":                strAny(us["name"]),
		"email":               strAny(us["email"]),
		"pro":                 boolAny(us["pro"]),
		"user_id":             strAny(us["userId"], us["user_id"]),
		"team_id":             strAny(us["teamId"], us["team_id"]),
		"teams_tier":          shortEnum(strAny(us["teamsTier"], us["teams_tier"])),
		"used_prompt_credits": numAny(us["userUsedPromptCredits"], us["user_used_prompt_credits"]),
		"used_flow_credits":   numAny(us["userUsedFlowCredits"], us["user_used_flow_credits"]),
		"max_premium_chat":    numAny(us["maxNumPremiumChatMessages"], us["max_num_premium_chat_messages"]),
	}

	ps, _ := us["planStatus"].(map[string]any)
	if ps == nil {
		ps, _ = us["plan_status"].(map[string]any)
	}
	if ps != nil {
		plan = map[string]any{
			"available_prompt_credits": numAny(ps["availablePromptCredits"], ps["available_prompt_credits"]),
			"available_flow_credits":   numAny(ps["availableFlowCredits"], ps["available_flow_credits"]),
			"available_flex_credits":   numAny(ps["availableFlexCredits"], ps["available_flex_credits"]),
			"used_flex_credits":        numAny(ps["usedFlexCredits"], ps["used_flex_credits"]),
			"used_flow_credits":        numAny(ps["usedFlowCredits"], ps["used_flow_credits"]),
			"used_prompt_credits":      numAny(ps["usedPromptCredits"], ps["used_prompt_credits"]),
			"daily_quota_remaining":    numAny(ps["dailyQuotaRemainingPercent"], ps["daily_quota_remaining_percent"]),
			"weekly_quota_remaining":   numAny(ps["weeklyQuotaRemainingPercent"], ps["weekly_quota_remaining_percent"]),
			"daily_quota_reset":        numAny(ps["dailyQuotaResetAtUnix"], ps["daily_quota_reset_at_unix"]),
			"weekly_quota_reset":       numAny(ps["weeklyQuotaResetAtUnix"], ps["weekly_quota_reset_at_unix"]),
			"acu_consumed":             numAny(ps["acuConsumed"], ps["acu_consumed"]),
			"acu_limit":                numAny(ps["acuLimit"], ps["acu_limit"]),
			"overage_balance_micros":   numAny(ps["overageBalanceMicros"], ps["overage_balance_micros"]),
			"plan_start":               strAny(ps["planStart"], ps["plan_start"]),
			"plan_end":                 strAny(ps["planEnd"], ps["plan_end"]),
			// 超额使用后的宽限与充值状态：配额烧穿不是立即断供，先进
			// grace period（grace_period_end 是 Connect-JSON Timestamp =
			// RFC3339 字符串）；top_up_status 记录自动加额是否生效。
			"was_reduced_by_orphaned_usage": boolAny(ps["wasReducedByOrphanedUsage"], ps["was_reduced_by_orphaned_usage"]),
			"grace_period_status":           shortEnum(strAny(ps["gracePeriodStatus"], ps["grace_period_status"])),
			"grace_period_end":              rfc3339Any(ps["gracePeriodEnd"], ps["grace_period_end"]),
		}
		tu, _ := ps["topUpStatus"].(map[string]any)
		if tu == nil {
			tu, _ = ps["top_up_status"].(map[string]any)
		}
		if tu != nil {
			plan["top_up_status"] = map[string]any{
				"enabled":            boolAny(tu["topUpEnabled"], tu["top_up_enabled"]),
				"transaction_status": shortEnum(strAny(tu["topUpTransactionStatus"], tu["top_up_transaction_status"])),
				"monthly_amount":     numAny(tu["monthlyTopUpAmount"], tu["monthly_top_up_amount"]),
				"spent":              numAny(tu["topUpSpent"], tu["top_up_spent"]),
				"increment":          numAny(tu["topUpIncrement"], tu["top_up_increment"]),
				"criteria_met":       boolAny(tu["topUpCriteriaMet"], tu["top_up_criteria_met"]),
			}
		}
		pi, _ := ps["planInfo"].(map[string]any)
		if pi == nil {
			pi, _ = ps["plan_info"].(map[string]any)
		}
		if pi != nil {
			plan["plan_name"] = strAny(pi["planName"], pi["plan_name"])
			plan["monthly_prompt_credits"] = numAny(pi["monthlyPromptCredits"], pi["monthly_prompt_credits"])
			plan["monthly_flow_credits"] = numAny(pi["monthlyFlowCredits"], pi["monthly_flow_credits"])
			plan["billing_strategy"] = shortEnum(strAny(pi["billingStrategy"], pi["billing_strategy"]))
			plan["is_teams"] = boolAny(pi["isTeams"], pi["is_teams"])
			plan["is_enterprise"] = boolAny(pi["isEnterprise"], pi["is_enterprise"])
			plan["can_buy_more"] = boolAny(pi["canBuyMoreCredits"], pi["can_buy_more_credits"])
			plan["has_paid_features"] = boolAny(pi["hasPaidFeatures"], pi["has_paid_features"])
		}
	}

	// 顶层 planInfo（部分响应会挂在 root）
	if top, ok := root["planInfo"].(map[string]any); ok {
		planInfo = map[string]any{
			"plan_name":                 strAny(top["planName"], top["plan_name"]),
			"monthly_prompt_credits":    numAny(top["monthlyPromptCredits"], top["monthly_prompt_credits"]),
			"monthly_flow_credits":      numAny(top["monthlyFlowCredits"], top["monthly_flow_credits"]),
			"billing_strategy":          shortEnum(strAny(top["billingStrategy"], top["billing_strategy"])),
			"is_teams":                  boolAny(top["isTeams"], top["is_teams"]),
			"is_enterprise":             boolAny(top["isEnterprise"], top["is_enterprise"]),
			"has_paid_features":         boolAny(top["hasPaidFeatures"], top["has_paid_features"]),
			"max_premium_chat_messages": numAny(top["maxNumPremiumChatMessages"], top["max_num_premium_chat_messages"]),
		}
	} else if plan != nil {
		if name, ok := plan["plan_name"]; ok {
			planInfo = map[string]any{
				"plan_name":              name,
				"monthly_prompt_credits": plan["monthly_prompt_credits"],
				"monthly_flow_credits":   plan["monthly_flow_credits"],
				"billing_strategy":       plan["billing_strategy"],
				"is_teams":               plan["is_teams"],
				"is_enterprise":          plan["is_enterprise"],
				"has_paid_features":      plan["has_paid_features"],
			}
		}
	}
	return user, plan, planInfo, nil
}

// cachedModels 返回 TTL 内的模型目录缓存；过期时经 singleflight 收敛为
// 单次上游拉取：等待方挂 done channel 而非写锁排队——RPC 最坏 610s，
// 锁内等待不吃 ctx，断连的调用方会永远卡在队列里。
func (h *Handler) cachedModels(ctx context.Context) ([]map[string]any, error) {
	for {
		h.modelsMu.RLock()
		if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
			cached := h.modelsCache
			h.modelsMu.RUnlock()
			return cached, nil
		}
		h.modelsMu.RUnlock()

		h.modelsMu.Lock()
		if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
			cached := h.modelsCache
			h.modelsMu.Unlock()
			return cached, nil
		}
		if h.modelsFetch != nil {
			done := h.modelsFetch
			h.modelsMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		h.modelsFetch = make(chan struct{})
		h.modelsMu.Unlock()

		// 目录是 handler 级共享缓存：单个调用方断连不应掐死其他等待者
		// 共用的拉取——脱离调用方 ctx，apiClient 的 610s 上限仍兜底。
		models, err := h.fetchModels(context.WithoutCancel(ctx))

		h.modelsMu.Lock()
		if err == nil {
			h.modelsCache = models
			h.modelsExpiry = time.Now().Add(h.cacheTTL)
		}
		// 先写缓存再 close：被唤醒的等待方回到循环立刻读到新值，
		// 失败时缓存保持旧值，下一个醒来的等待方顺位成为新的拉取者。
		close(h.modelsFetch)
		h.modelsFetch = nil
		h.modelsMu.Unlock()
		return models, err
	}
}

// fetchModels 拉取并投影上游模型目录；CLI 版响应与 Cascade 版模型表一致，
// 并多出 subagent_default_model_uid 等字段。
func (h *Handler) fetchModels(ctx context.Context) ([]map[string]any, error) {
	resp, err := h.currentUpstream().apiClient.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: upstream.BuildMetadata(h.tokenFunc(), clientName, clientVersion, "win", 32),
	}))
	if err != nil {
		return nil, err
	}

	var models []map[string]any
	for _, c := range resp.Msg.GetClientModelConfigs() {
		uid := c.GetModelUid()
		if uid == "" && c.GetModelOrAlias() != nil {
			uid = c.GetModelOrAlias().GetModelUid()
		}
		costTier := "unspecified"
		switch c.GetModelCostTier() {
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_FREE:
			costTier = "free"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_LOW:
			costTier = "low"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_MEDIUM:
			costTier = "medium"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_HIGH:
			costTier = "high"
		}

		mult := c.GetCreditMultiplier()
		multKnown := mult != 0 || costTier == "free"
		pricingType := shortEnum(c.GetPricingType().String())
		provider := shortEnum(c.GetProvider().String())
		apiProvider := shortEnum(c.GetApiProvider().String())

		m := map[string]any{
			"uid":                 uid,
			"label":               c.GetLabel(),
			"description":         c.GetDescription(),
			"cost_tier":           costTier,
			"credit_multiplier":   mult,
			"multiplier_known":    multKnown,
			"pricing_type":        pricingType,
			"provider":            provider,
			"api_provider":        apiProvider,
			"max_tokens":          c.GetMaxTokens(),
			"disabled":            c.GetDisabled(),
			"is_premium":          c.GetIsPremium(),
			"is_beta":             c.GetIsBeta(),
			"is_new":              c.GetIsNew(),
			"is_recommended":      c.GetIsRecommended(),
			"supports_images":     c.GetSupportsImages(),
			"is_capacity_limited": c.GetIsCapacityLimited(),
			"supports_legacy":     c.GetSupportsLegacy(),
		}
		if modelInfo := c.GetModelInfo(); modelInfo != nil {
			m["context_tokens"] = modelInfo.GetMaxTokens()
			m["max_output_tokens"] = modelInfo.GetMaxOutputTokens()
			m["is_model_router"] = modelInfo.GetIsModelRouter()
			if features := modelInfo.GetModelFeatures(); features != nil {
				m["supports_tool_calls"] = features.GetSupportsToolCalls()
				m["supports_parallel_tool_calls"] = features.GetSupportsParallelToolCalls()
				m["supports_thinking"] = features.GetSupportsThinking()
				m["preserve_thinking"] = features.GetPreserveThinking()
				m["interleave_thinking"] = features.GetInterleaveThinking()
			}
		}

		var dims []map[string]any
		var inputPrice, cachedPrice, outputPrice float64
		var hasInput, hasCached, hasOutput bool
		for _, d := range c.GetModelDimensions() {
			dim := map[string]any{
				"label":       d.GetLabel(),
				"value":       d.GetValue(),
				"min":         d.GetMinRange(),
				"max":         d.GetMaxRange(),
				"denominator": d.GetDenominator(),
				"kind":        shortEnum(d.GetKind().String()),
				"info":        d.GetInfo(),
			}
			dims = append(dims, dim)
			switch strings.ToLower(d.GetLabel()) {
			case "input":
				inputPrice, hasInput = float64(d.GetValue()), true
			case "cached input", "cached_input", "cache read", "cache_read":
				cachedPrice, hasCached = float64(d.GetValue()), true
			case "output":
				outputPrice, hasOutput = float64(d.GetValue()), true
			}
		}
		if len(dims) > 0 {
			m["dimensions"] = dims
		}
		if hasInput {
			m["price_input"] = inputPrice
		}
		if hasCached {
			m["price_cached"] = cachedPrice
		}
		if hasOutput {
			m["price_output"] = outputPrice
		}

		if ps := c.GetPromoStatus(); ps != nil && ps.GetIsActive() {
			promo := map[string]any{"active": true, "label": ps.GetLabel()}
			if ed := ps.GetEndDate(); ed != nil && ed.GetSeconds() != 0 {
				promo["end_date"] = time.Unix(ed.GetSeconds(), int64(ed.GetNanos())).UTC().Format(time.RFC3339)
			}
			m["promo"] = promo
		}
		if fs := c.GetFastStatus(); fs != nil && fs.GetIsActive() {
			m["fast"] = map[string]any{"active": true, "tooltip": fs.GetTooltip()}
		}
		if fm := c.GetModelFamilyMetadata(); fm != nil {
			m["family"] = fm.GetModelFamilyLabel()
			m["is_default_in_family"] = fm.GetIsDefaultModelInFamily() || c.GetIsDefaultModelInFamily()
		}
		if dr := c.GetDisabledReason(); dr != nil {
			m["disabled_reason"] = dr.GetShortReason()
			m["disabled_description"] = dr.GetDescription()
		}
		if c.GetBetaWarningMessage() != "" {
			m["beta_warning"] = c.GetBetaWarningMessage()
		}
		models = append(models, m)
	}

	return models, nil
}

func (h *Handler) cachedProviders(ctx context.Context) ([]map[string]any, error) {
	h.providersMu.RLock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		cached := h.providersCache
		h.providersMu.RUnlock()
		return cached, nil
	}
	h.providersMu.RUnlock()

	h.providersMu.Lock()
	defer h.providersMu.Unlock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		return h.providersCache, nil
	}

	providerResp, err := h.currentUpstream().apiClient.GetModelProviders(ctx, connect.NewRequest(&devinproto.GetModelProvidersRequest{}))
	if err != nil {
		return nil, err
	}
	var providers []map[string]any
	for _, p := range providerResp.Msg.GetModelProviders() {
		providers = append(providers, map[string]any{
			"provider":     shortEnum(p.GetProvider().String()),
			"display_name": p.GetDisplayName(),
		})
	}

	h.providersCache = providers
	h.providersExpiry = time.Now().Add(h.cacheTTL)
	return providers, nil
}

func (h *Handler) cachedModelStatuses(ctx context.Context) ([]map[string]any, error) {
	h.modelStatusesMu.RLock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		cached := h.modelStatusesCache
		h.modelStatusesMu.RUnlock()
		return cached, nil
	}
	h.modelStatusesMu.RUnlock()

	h.modelStatusesMu.Lock()
	defer h.modelStatusesMu.Unlock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		return h.modelStatusesCache, nil
	}

	modelStatusResp, err := h.currentUpstream().apiClient.GetModelStatuses(ctx, connect.NewRequest(&devinproto.GetModelStatusesRequest{
		Metadata: upstream.BuildMetadata(h.tokenFunc(), clientName, clientVersion, "win", 32),
	}))
	if err != nil {
		return nil, err
	}
	var statuses []map[string]any
	for _, s := range modelStatusResp.Msg.GetModelStatusInfos() {
		statuses = append(statuses, map[string]any{
			"model":     shortEnum(s.GetModel().String()),
			"model_uid": s.GetModelUid(),
			"status":    shortEnum(s.GetStatus().String()),
			"message":   s.GetMessage(),
		})
	}

	h.modelStatusesCache = statuses
	h.modelStatusesExpiry = time.Now().Add(h.cacheTTL)
	return statuses, nil
}
