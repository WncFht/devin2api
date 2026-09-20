// 本文件是与 Devin 上游通信的部分：/admin/status 聚合端点、
// 模型目录/供应商/模型状态的拉取与缓存、Connect metadata 构造、鉴权 transport。
package ccpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/upstream"
)

const (
	clientName    = "windsurf"
	clientVersion = "1.48.2"
	// seatUserStatusPath 与 Windsurf 官方 / WindsurfAPI 一致的 JSON Connect 路径。
	// 生成的 connect 包名 ExaSeatManagementPb_SeatManagementService 在上游会 404。
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
	// seatMintPath 是 durable api_key 铸 session token 的 seat 端点，
	// 与 adapter mintSessionToken 同 wire（X-Api-Key + metadata.api_key）。
	seatMintPath = "/exa.seat_management_pb.SeatManagementService/GetSelfDevinSessionToken"
	// chatCapacityPath 是 lane 真实鉴权面（ApiServerService）的轻量探针：
	// Basic token-token 认证与 lane 完全同形，答「凭据能不能干活」。
	chatCapacityPath = "/exa.api_server_pb.ApiServerService/CheckChatCapacity"
)

// upstreamHTTPError 是上游非 200 的结构化错误：endpoint/status/body
// 供调用方按语义分拣（seat plan-gate 判别需要 status 与正文，裸字符
// 串断言做不到）。Error() 与原 fmt.Errorf 文案同形，调用面无感。
type upstreamHTTPError struct {
	endpoint string
	status   int
	body     string
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.endpoint, e.status, truncate(e.body, 300))
}

// isSeatPlanGate 判定错误是不是「individual plan 无 seat 面权限」：
// SeatManagementService 对非 team 号整面 403 permission_denied，错误
// 文案带 individual plan 标记——凭据本身有效，只是该服务面对其不开放。
func isSeatPlanGate(err error) bool {
	var ue *upstreamHTTPError
	return errors.As(err, &ue) && ue.status == http.StatusForbidden &&
		strings.Contains(ue.body, "individual plan")
}

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
		ps, ok := h.poolSnapshot()
		if !ok {
			return
		}
		// 与其余五路同序：先拉取与计算、末段一次 resultMu 写结果——
		// 慢目录拉取不该把整扇聚合互斥到底，也避免 resultMu→缓存锁
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
			for name, target := range ps.Aliases {
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
			slices.Sort(absent)
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
		return nil, nil, nil, &upstreamHTTPError{endpoint: "GetUserStatus", status: resp.StatusCode, body: string(raw)}
	}

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, nil, nil, fmt.Errorf("decode GetUserStatus: %w", err)
	}
	// 上游同一字段在 camelCase/snake_case 间漂移——键树归一（去 _ 全小写）
	// 后两种拼写收敛到同一键，wire struct 的 tag 按归一键书写。
	normalizeWireKeys(root)
	norm, err := json.Marshal(root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode GetUserStatus: %w", err)
	}
	var wire seatUserStatusResponse
	dec = json.NewDecoder(bytes.NewReader(norm))
	dec.UseNumber()
	if err := dec.Decode(&wire); err != nil {
		return nil, nil, nil, fmt.Errorf("decode GetUserStatus: %w", err)
	}
	us := wire.UserStatus
	if us == nil {
		return nil, nil, nil, fmt.Errorf("GetUserStatus: empty userStatus")
	}

	user = map[string]any{
		"name":                string(us.Name),
		"email":               string(us.Email),
		"pro":                 bool(us.Pro),
		"user_id":             string(us.UserID),
		"team_id":             string(us.TeamID),
		"teams_tier":          shortEnum(string(us.TeamsTier)),
		"used_prompt_credits": numOrNil(us.UsedPromptCredits),
		"used_flow_credits":   numOrNil(us.UsedFlowCredits),
		"max_premium_chat":    numOrNil(us.MaxPremiumChatMessages),
	}

	if ps := us.PlanStatus; ps != nil {
		plan = map[string]any{
			"available_prompt_credits": numOrNil(ps.AvailablePromptCredits),
			"available_flow_credits":   numOrNil(ps.AvailableFlowCredits),
			"available_flex_credits":   numOrNil(ps.AvailableFlexCredits),
			"used_flex_credits":        numOrNil(ps.UsedFlexCredits),
			"used_flow_credits":        numOrNil(ps.UsedFlowCredits),
			"used_prompt_credits":      numOrNil(ps.UsedPromptCredits),
			"daily_quota_remaining":    numOrNil(ps.DailyQuotaRemaining),
			"weekly_quota_remaining":   numOrNil(ps.WeeklyQuotaRemaining),
			"daily_quota_reset":        numOrNil(ps.DailyQuotaReset),
			"weekly_quota_reset":       numOrNil(ps.WeeklyQuotaReset),
			"acu_consumed":             numOrNil(ps.ACUConsumed),
			"acu_limit":                numOrNil(ps.ACULimit),
			"overage_balance_micros":   numOrNil(ps.OverageBalanceMicros),
			"plan_start":               string(ps.PlanStart),
			"plan_end":                 string(ps.PlanEnd),
			// 超额使用后的宽限与充值状态：配额烧穿不是立即断供，先进
			// grace period（grace_period_end 是 Connect-JSON Timestamp =
			// RFC3339 字符串）；top_up_status 记录自动加额是否生效。
			"was_reduced_by_orphaned_usage": bool(ps.WasReducedByOrphanedUsage),
			"grace_period_status":           shortEnum(string(ps.GracePeriodStatus)),
			"grace_period_end":              normRFC3339(string(ps.GracePeriodEnd)),
		}
		if tu := ps.TopUpStatus; tu != nil {
			plan["top_up_status"] = map[string]any{
				"enabled":            bool(tu.Enabled),
				"transaction_status": shortEnum(string(tu.TransactionStatus)),
				"monthly_amount":     numOrNil(tu.MonthlyAmount),
				"spent":              numOrNil(tu.Spent),
				"increment":          numOrNil(tu.Increment),
				"criteria_met":       bool(tu.CriteriaMet),
			}
		}
		if pi := ps.PlanInfo; pi != nil {
			plan["plan_name"] = string(pi.PlanName)
			plan["monthly_prompt_credits"] = numOrNil(pi.MonthlyPromptCredits)
			plan["monthly_flow_credits"] = numOrNil(pi.MonthlyFlowCredits)
			plan["billing_strategy"] = shortEnum(string(pi.BillingStrategy))
			plan["is_teams"] = bool(pi.IsTeams)
			plan["is_enterprise"] = bool(pi.IsEnterprise)
			plan["can_buy_more"] = bool(pi.CanBuyMoreCredits)
			plan["has_paid_features"] = bool(pi.HasPaidFeatures)
		}
	}

	// 顶层 planInfo（部分响应会挂在 root；键归一后 snake 拼写同样命中——
	// 比原先只读 camelCase 略宽，漂移方向上是更宽容的一侧）。
	if top := wire.PlanInfo; top != nil {
		planInfo = map[string]any{
			"plan_name":                 string(top.PlanName),
			"monthly_prompt_credits":    numOrNil(top.MonthlyPromptCredits),
			"monthly_flow_credits":      numOrNil(top.MonthlyFlowCredits),
			"billing_strategy":          shortEnum(string(top.BillingStrategy)),
			"is_teams":                  bool(top.IsTeams),
			"is_enterprise":             bool(top.IsEnterprise),
			"has_paid_features":         bool(top.HasPaidFeatures),
			"max_premium_chat_messages": numOrNil(top.MaxPremiumChatMessages),
		}
	} else if us.PlanStatus != nil && us.PlanStatus.PlanInfo != nil {
		pi := us.PlanStatus.PlanInfo
		planInfo = map[string]any{
			"plan_name":              string(pi.PlanName),
			"monthly_prompt_credits": numOrNil(pi.MonthlyPromptCredits),
			"monthly_flow_credits":   numOrNil(pi.MonthlyFlowCredits),
			"billing_strategy":       shortEnum(string(pi.BillingStrategy)),
			"is_teams":               bool(pi.IsTeams),
			"is_enterprise":          bool(pi.IsEnterprise),
			"has_paid_features":      bool(pi.HasPaidFeatures),
		}
	}
	return user, plan, planInfo, nil
}

// seatProbeBody 构造 seat/ApiServer 裸 JSON 探针的公共请求体——
// metadata 形状与 fetchUserStatusAs/adapter mint 完全同口径。
func seatProbeBody(token string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"api_key":           token,
			"extension_name":    clientName,
			"extension_version": clientVersion,
			"ide_name":          clientName,
			"ide_version":       clientVersion,
			"locale":            "en",
			"os":                "windows",
		},
	})
	return payload
}

// checkChatCapacityAs 以指定凭据探测 ApiServerService/CheckChatCapacity：
// lane 的真实鉴权面（Basic token-token 认证与 lane wire 完全同形）。
// verify/test 用它回答「这枚凭据能不能干活」——seat 端点答不了这个
// 问题（对 individual plan 整面 plan-gated，结果互不预测）。
// 返回 has_capacity；非 200 回 *upstreamHTTPError。
func (h *Handler) checkChatCapacityAs(ctx context.Context, token string) (bool, error) {
	up := h.currentUpstream()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		up.baseURL+chatCapacityPath, bytes.NewReader(seatProbeBody(token)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Basic "+token+"-"+token)
	client := &http.Client{Timeout: 60 * time.Second, Transport: up.baseTransport}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, &upstreamHTTPError{endpoint: "CheckChatCapacity", status: resp.StatusCode, body: string(raw)}
	}
	var parsed struct {
		HasCapacity bool `json:"hasCapacity"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, fmt.Errorf("decode CheckChatCapacity: %w", err)
	}
	return parsed.HasCapacity, nil
}

// mintSessionTokenProbe 以 durable api_key 试铸一枚 session token
// （adapter mintSessionToken 的探测版，同 wire：metadata.api_key 与
// X-Api-Key 头放同一枚 key）。api_key-only 账号的 lane 服役凭据就是
// 铸出的 token——mint 成功即服役链路的端到端验证。错误体先擦掉
// durable key 再外透（adapter 同款防回显）。
func (h *Handler) mintSessionTokenProbe(ctx context.Context, apiKey string) (string, error) {
	up := h.currentUpstream()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		up.baseURL+seatMintPath, bytes.NewReader(seatProbeBody(apiKey)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Api-Key", apiKey)
	client := &http.Client{Timeout: 30 * time.Second, Transport: up.baseTransport}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.ReplaceAll(string(raw), apiKey, "[redacted]")
		return "", &upstreamHTTPError{endpoint: "GetSelfDevinSessionToken", status: resp.StatusCode, body: detail}
	}
	var parsed struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode GetSelfDevinSessionToken: %w", err)
	}
	token := strings.TrimSpace(parsed.SessionToken)
	if token == "" {
		return "", errors.New("GetSelfDevinSessionToken: empty sessionToken")
	}
	return token, nil
}

// verifyCredential 探测一次写输入将生效的凭据，回结构化能力位；cred 是
// 调用方已按 ops 口径解出的服役凭据（api_key-only 输入解出 api_key 本身）。
//   - api_key-only（lane 将以铸出的 session token 服役）：mint 即端到端
//     验证，铸出的 token 再过 chat 面拿 has_capacity；
//   - token/credentials_file/credentials_content：cred 直接过 chat 面；
//     附带 api_key 时顺带测 mint——不通只记能力位不拦建行（服役凭据
//     本身已证可用）；
//   - seat GetUserStatus 尽力回填：200 带 user/plan/teams_tier；
//     individual-plan 403 → seat=false+seat_gated 受限标记；其余失败
//     仅留 seat_error——chat 已过即放行。
//
// error 非 nil = 拒绝建号（chat 面或 mint 段失败，含上游真实原因）。
func (h *Handler) verifyCredential(ctx context.Context, in accounts.AccountWrite, cred string) (map[string]any, error) {
	ver := map[string]any{}
	var servingToken string
	if in.Token == "" && in.CredentialsFile == "" && in.CredentialsContent == "" && strings.TrimSpace(in.APIKey) != "" {
		minted, err := h.mintSessionTokenProbe(ctx, strings.TrimSpace(in.APIKey))
		if err != nil {
			return nil, fmt.Errorf("api_key mint: %w", err)
		}
		ver["mint"] = true
		servingToken = minted
	} else {
		servingToken = cred
		if k := strings.TrimSpace(in.APIKey); k != "" {
			if _, err := h.mintSessionTokenProbe(ctx, k); err != nil {
				ver["mint"] = false
				ver["mint_error"] = err.Error()
			} else {
				ver["mint"] = true
			}
		}
	}
	hasCap, err := h.checkChatCapacityAs(ctx, servingToken)
	if err != nil {
		return nil, err
	}
	ver["chat"] = true
	ver["has_capacity"] = hasCap
	user, plan, _, err := h.fetchUserStatusAs(ctx, servingToken)
	if err != nil {
		ver["seat"] = false
		ver["seat_error"] = err.Error()
		if isSeatPlanGate(err) {
			ver["seat_gated"] = true
		}
	} else {
		ver["seat"] = true
		ver["user"] = user
		if plan != nil {
			ver["plan"] = plan["plan_name"]
			if user != nil {
				ver["teams_tier"] = user["teams_tier"]
			}
		}
	}
	return ver, nil
}

// tag 写的是 normalizeWireKeys 归一后的键（去 _ 全小写）：上游同一字段
// 在 camelCase/snake_case 间漂移，归一后两种拼写打到同一字段。
// 数值字段用 any 原样透传——上游数字与数字串两种形态都发，下游
// floatAny 两种都吃，透传保住 wire 原文（含 >2^53 的精度）。
type seatUserStatusResponse struct {
	UserStatus *seatUserStatus `json:"userstatus"`
	PlanInfo   *seatPlanInfo   `json:"planinfo"`
}

// seatUserStatus 是 userStatus 段的 wire 形状；PlanStatus/TopUpStatus/
// PlanInfo 嵌套对象缺席时指针为 nil，与原 map 断言同口径。
type seatUserStatus struct {
	Name                   wireString      `json:"name"`
	Email                  wireString      `json:"email"`
	Pro                    wireBool        `json:"pro"`
	UserID                 wireString      `json:"userid"`
	TeamID                 wireString      `json:"teamid"`
	TeamsTier              wireString      `json:"teamstier"`
	UsedPromptCredits      any             `json:"userusedpromptcredits"`
	UsedFlowCredits        any             `json:"userusedflowcredits"`
	MaxPremiumChatMessages any             `json:"maxnumpremiumchatmessages"`
	PlanStatus             *seatPlanStatus `json:"planstatus"`
}

// seatPlanStatus 是 planStatus 段的 wire 形状（配额/宽限/充值状态）。
type seatPlanStatus struct {
	AvailablePromptCredits    any           `json:"availablepromptcredits"`
	AvailableFlowCredits      any           `json:"availableflowcredits"`
	AvailableFlexCredits      any           `json:"availableflexcredits"`
	UsedFlexCredits           any           `json:"usedflexcredits"`
	UsedFlowCredits           any           `json:"usedflowcredits"`
	UsedPromptCredits         any           `json:"usedpromptcredits"`
	DailyQuotaRemaining       any           `json:"dailyquotaremainingpercent"`
	WeeklyQuotaRemaining      any           `json:"weeklyquotaremainingpercent"`
	DailyQuotaReset           any           `json:"dailyquotaresetatunix"`
	WeeklyQuotaReset          any           `json:"weeklyquotaresetatunix"`
	ACUConsumed               any           `json:"acuconsumed"`
	ACULimit                  any           `json:"aculimit"`
	OverageBalanceMicros      any           `json:"overagebalancemicros"`
	PlanStart                 wireString    `json:"planstart"`
	PlanEnd                   wireString    `json:"planend"`
	WasReducedByOrphanedUsage wireBool      `json:"wasreducedbyorphanedusage"`
	GracePeriodStatus         wireString    `json:"graceperiodstatus"`
	GracePeriodEnd            wireString    `json:"graceperiodend"`
	TopUpStatus               *seatTopUp    `json:"topupstatus"`
	PlanInfo                  *seatPlanInfo `json:"planinfo"`
}

// seatTopUp 是 topUpStatus 段的 wire 形状（自动加额配置与当月执行账）。
type seatTopUp struct {
	Enabled           wireBool   `json:"topupenabled"`
	TransactionStatus wireString `json:"topuptransactionstatus"`
	MonthlyAmount     any        `json:"monthlytopupamount"`
	Spent             any        `json:"topupspent"`
	Increment         any        `json:"topupincrement"`
	CriteriaMet       wireBool   `json:"topupcriteriamet"`
}

// seatPlanInfo 是 planInfo 段的 wire 形状；同一形状挂在 planStatus 内与
// 响应 root 两处，字段集取两边投影的并集。
type seatPlanInfo struct {
	PlanName               wireString `json:"planname"`
	MonthlyPromptCredits   any        `json:"monthlypromptcredits"`
	MonthlyFlowCredits     any        `json:"monthlyflowcredits"`
	BillingStrategy        wireString `json:"billingstrategy"`
	IsTeams                wireBool   `json:"isteams"`
	IsEnterprise           wireBool   `json:"isenterprise"`
	CanBuyMoreCredits      wireBool   `json:"canbuymorecredits"`
	HasPaidFeatures        wireBool   `json:"haspaidfeatures"`
	MaxPremiumChatMessages any        `json:"maxnumpremiumchatmessages"`
}

// wireString 接受字符串或原始数字/布尔字面量的宽松字符串字段
// （strAny 同口径：字符串原样、数字取原始字面量、布尔取 true/false 文本）。
type wireString string

// UnmarshalJSON 把 JSON 值宽松解成字符串：字符串正常解，其余标量保留
// 原始字面量，null 留空。
func (s *wireString) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] != '"' {
		*s = wireString(string(raw))
		return nil
	}
	return json.Unmarshal(raw, (*string)(s))
}

// wireBool 接受 bool 或字符串型 bool 的宽松布尔字段（boolAny 同口径：
// bool 原样、"true"/"1" 字符串判真，其余一律 false）。
type wireBool bool

// UnmarshalJSON 把 JSON 值宽松解成布尔：true 字面量与 "true"/"1" 字符串
// 为真，其余（false/其它字符串/数字/对象/null）一律 false。
func (b *wireBool) UnmarshalJSON(raw []byte) error {
	switch string(raw) {
	case "true", `"true"`, `"1"`:
		*b = true
	}
	return nil
}

// normalizeWireKeys 把宽松解码的 JSON 树键名原地归一成「去 _ 全小写」：
// userId/user_id/userID 收敛到 userid，wire struct 只写一种 tag。
// 键按序处理且先写赢——排序使 camelCase（无 _ 拼写）先于 snake 落位，
// 归一撞键时保留 camel 值，与原双读 camel 优先的口径一致。
func normalizeWireKeys(m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := m[k]
		delete(m, k)
		nk := strings.ToLower(strings.ReplaceAll(k, "_", ""))
		if _, clash := m[nk]; !clash {
			m[nk] = v
		}
		switch child := v.(type) {
		case map[string]any:
			normalizeWireKeys(child)
		case []any:
			for _, item := range child {
				if mm, ok := item.(map[string]any); ok {
					normalizeWireKeys(mm)
				}
			}
		}
	}
}

// numOrNil 与 numAny 同口径：空字符串按缺席计（nil），其余值原样透传。
func numOrNil(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

// normRFC3339 把 RFC3339 串归一成 UTC RFC3339；解析失败保留原文——外部
// 输入边界上原样暴露比吞掉更可排障（rfc3339Any 同口径）。
func normRFC3339(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// cachedModels 返回 TTL 内的模型目录缓存；缓存与并发收敛由
// h.modelsCache（ttlCache）承担，拉取在锁外跑。
func (h *Handler) cachedModels(ctx context.Context) ([]map[string]any, error) {
	return h.modelsCache.Get(ctx)
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
				m["supports_documents"] = features.GetSupportsDocuments()
				m["supports_document_urls"] = features.GetSupportsDocumentUrls()
				m["supports_video"] = features.GetSupportsVideo()
				m["supports_video_urls"] = features.GetSupportsVideoUrls()
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

// cachedProviders 返回 TTL 内的供应商列表缓存。RPC 在锁外进行：写锁
// 横跨上游调用（上限 610s）时，并发等待方阻塞在 Lock 上且不吃各自
// ctx——锁只护缓存读写。并发 miss 各打一趟、写侧复查竞胜者为准；
// 唯一调用路径（StatusReport）已被 statusSnapshot 的 singleflight 收敛。
func (h *Handler) cachedProviders(ctx context.Context) ([]map[string]any, error) {
	return h.providersCache.Get(ctx)
}

// fetchProviders 拉取并投影上游供应商列表；TTL/收敛由 providersCache 承担。
func (h *Handler) fetchProviders(ctx context.Context) ([]map[string]any, error) {
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
	return providers, nil
}

// cachedModelStatuses 返回 TTL 内的模型状态缓存；锁安排同
// cachedProviders（RPC 在锁外，写侧复查竞胜者）。
func (h *Handler) cachedModelStatuses(ctx context.Context) ([]map[string]any, error) {
	return h.modelStatusesCache.Get(ctx)
}

// fetchModelStatuses 拉取并投影上游模型状态表；TTL/收敛由 modelStatusesCache 承担。
func (h *Handler) fetchModelStatuses(ctx context.Context) ([]map[string]any, error) {
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
	return statuses, nil
}
