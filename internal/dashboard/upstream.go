// 本文件是与 Devin 上游通信的部分：/panel/api/status 聚合端点、
// 模型目录/供应商/模型状态的拉取与缓存、Connect metadata 构造、鉴权 transport。
package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/upstream"
)

const (
	clientName    = "windsurf"
	clientVersion = "1.48.2"
	// seatUserStatusPath 与 Windsurf 官方 / WindsurfAPI 一致的 JSON Connect 路径。
	// 生成的 connect 包名 ExaSeatManagementPb_SeatManagementService 在上游会 404。
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

func (h *Handler) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	// 面板聚合多个上游调用，给足时间避免单个慢接口拖垮整体；
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()

	result := map[string]any{}
	var resultMu sync.Mutex

	// 正确路径：JSON Connect SeatManagement GetUserStatus（Bearer + metadata.api_key）
	if user, plan, planInfo, err := h.fetchUserStatus(ctx); err != nil {
		resultMu.Lock()
		result["user_status_error"] = err.Error()
		resultMu.Unlock()
	} else {
		resultMu.Lock()
		if user != nil {
			result["user"] = user
		}
		if plan != nil {
			result["plan_status"] = plan
		}
		if planInfo != nil {
			result["plan_info"] = planInfo
		}
		resultMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		capResp, err := h.apiClient.CheckChatCapacity(ctx, connect.NewRequest(&devinproto.CheckChatCapacityRequest{
			Metadata: upstream.BuildMetadata(h.token, clientName, clientVersion, "win", 32),
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
		statusResp, err := h.apiClient.GetStatus(ctx, connect.NewRequest(&devinproto.GetStatusRequest{
			Metadata: upstream.BuildMetadata(h.token, clientName, clientVersion, "win", 32),
		}))
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["status_error"] = err.Error()
			return
		}
		st := statusResp.Msg.GetStatus()
		result["ide_status"] = map[string]any{
			"level":   shortEnum(st.GetLevel().String(), "STATUS_LEVEL_"),
			"message": st.GetMessage(),
		}
		result["show_review_prompt"] = statusResp.Msg.GetShowReviewPrompt()
	}()

	go func() {
		defer wg.Done()
		statuses := h.cachedModelStatuses(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if statuses != nil {
			result["model_statuses"] = statuses
		}
	}()

	go func() {
		defer wg.Done()
		providers := h.cachedProviders(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if providers != nil {
			result["providers"] = providers
		}
	}()

	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// fetchUserStatus 调用官方 seat_management JSON Connect 路径。
func (h *Handler) fetchUserStatus(ctx context.Context) (user, plan, planInfo map[string]any, err error) {
	bodyObj := map[string]any{
		"metadata": map[string]any{
			"api_key":           h.token,
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
	url := h.baseURL + seatUserStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+h.token)

	// 使用不带 Basic 改写的 client，避免 authTransport 覆盖 Bearer；但复用代理 transport。
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	client := &http.Client{Timeout: 610 * time.Second, Transport: h.baseTransport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
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
		"teams_tier":          shortEnum(strAny(us["teamsTier"], us["teams_tier"]), "TEAMS_TIER_"),
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
		}
		pi, _ := ps["planInfo"].(map[string]any)
		if pi == nil {
			pi, _ = ps["plan_info"].(map[string]any)
		}
		if pi != nil {
			plan["plan_name"] = strAny(pi["planName"], pi["plan_name"])
			plan["monthly_prompt_credits"] = numAny(pi["monthlyPromptCredits"], pi["monthly_prompt_credits"])
			plan["monthly_flow_credits"] = numAny(pi["monthlyFlowCredits"], pi["monthly_flow_credits"])
			plan["billing_strategy"] = shortEnum(strAny(pi["billingStrategy"], pi["billing_strategy"]), "BILLING_STRATEGY_")
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
			"billing_strategy":          shortEnum(strAny(top["billingStrategy"], top["billing_strategy"]), "BILLING_STRATEGY_"),
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

func (h *Handler) apiModels(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	// 模型目录可能较大，给足时间并复用缓存；与 ResponseHeaderTimeout 对齐。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()

	models, err := h.cachedModels(ctx)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"%s"}`, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func (h *Handler) cachedModels(ctx context.Context) ([]map[string]any, error) {
	h.cacheMu.RLock()
	if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
		cached := h.modelsCache
		h.cacheMu.RUnlock()
		return cached, nil
	}
	h.cacheMu.RUnlock()

	// 写锁内复查后再拉取：TTL 过期瞬间的并发 miss 收敛为单次上游调用。
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
		return h.modelsCache, nil
	}

	// CLI 版响应与 Cascade 版模型表一致，并多出 subagent_default_model_uid 等字段。
	resp, err := h.apiClient.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: upstream.BuildMetadata(h.token, clientName, clientVersion, "win", 32),
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
		pricingType := shortEnum(c.GetPricingType().String(), "MODEL_PRICING_TYPE_")
		provider := shortEnum(c.GetProvider().String(), "MODEL_PROVIDER_")
		apiProvider := shortEnum(c.GetApiProvider().String(), "API_PROVIDER_")

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
				"kind":        shortEnum(d.GetKind().String(), "MODEL_DIMENSION_KIND_"),
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

	h.modelsCache = models
	h.modelsExpiry = time.Now().Add(h.cacheTTL)
	return models, nil
}

func (h *Handler) cachedProviders(ctx context.Context) []map[string]any {
	h.cacheMu.RLock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		cached := h.providersCache
		h.cacheMu.RUnlock()
		return cached
	}
	h.cacheMu.RUnlock()

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		return h.providersCache
	}

	providerResp, err := h.apiClient.GetModelProviders(ctx, connect.NewRequest(&devinproto.GetModelProvidersRequest{}))
	if err != nil {
		return nil
	}
	var providers []map[string]any
	for _, p := range providerResp.Msg.GetModelProviders() {
		providers = append(providers, map[string]any{
			"provider":     shortEnum(p.GetProvider().String(), "MODEL_PROVIDER_"),
			"display_name": p.GetDisplayName(),
		})
	}

	h.providersCache = providers
	h.providersExpiry = time.Now().Add(h.cacheTTL)
	return providers
}

func (h *Handler) cachedModelStatuses(ctx context.Context) []map[string]any {
	h.cacheMu.RLock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		cached := h.modelStatusesCache
		h.cacheMu.RUnlock()
		return cached
	}
	h.cacheMu.RUnlock()

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		return h.modelStatusesCache
	}

	modelStatusResp, err := h.apiClient.GetModelStatuses(ctx, connect.NewRequest(&devinproto.GetModelStatusesRequest{
		Metadata: upstream.BuildMetadata(h.token, clientName, clientVersion, "win", 32),
	}))
	if err != nil {
		return nil
	}
	var statuses []map[string]any
	for _, s := range modelStatusResp.Msg.GetModelStatusInfos() {
		statuses = append(statuses, map[string]any{
			"model":  shortEnum(s.GetModel().String(), "MODEL_"),
			"status": shortEnum(s.GetStatus().String(), "MODEL_STATUS_"),
		})
	}

	h.modelStatusesCache = statuses
	h.modelStatusesExpiry = time.Now().Add(h.cacheTTL)
	return statuses
}
