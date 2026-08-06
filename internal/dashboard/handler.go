// Package dashboard 实现管理面板：模型列表、价格筛选、账户用量。
// password 为空时无需登录直接进入；非空时走 session cookie。
package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/leookun/devin-2api/internal/httpproxy"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"google.golang.org/protobuf/proto"
)

const (
	clientName    = "windsurf"
	clientVersion = "1.48.2"
	// seatUserStatusPath 与 Windsurf 官方 / WindsurfAPI 一致的 JSON Connect 路径。
	// 生成的 connect 包名 ExaSeatManagementPb_SeatManagementService 在上游会 404。
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// Handler 是面板 HTTP 处理器。
type Handler struct {
	password      string
	baseURL       string
	token         string
	apiClient     devinprotoconnect.ApiServerServiceClient
	httpClient    *http.Client
	baseTransport http.RoundTripper
	sessionTokens map[string]time.Time
}

// New 创建面板处理器。password 为空表示开放访问。proxy 为可选代理地址。
func New(password, baseURL, token, proxy string) *Handler {
	base := http.DefaultTransport
	if proxy != "" {
		if t, err := httpproxy.NewTransport(proxy); err == nil {
			base = t
		}
	}
	transport := &authTransport{base: base, token: token}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return &Handler{
		password:      password,
		baseURL:       strings.TrimRight(baseURL, "/"),
		token:         token,
		apiClient:     devinprotoconnect.NewApiServerServiceClient(httpClient, baseURL),
		httpClient:    httpClient,
		baseTransport: base,
		sessionTokens: make(map[string]time.Time),
	}
}

// Register 将面板路由注册到 mux。有 token 即可启用；密码仅控制是否登录。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/panel", h.servePanel)
	mux.Post("/panel/login", h.handleLogin)
	mux.Get("/panel/api/status", h.apiStatus)
	mux.Get("/panel/api/models", h.apiModels)
}

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.password != "" && !h.isAuthenticated(r) {
		_, _ = w.Write([]byte(loginPage))
		return
	}
	_, _ = w.Write([]byte(dashboardPage))
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.password == "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"open":true}`))
		return
	}
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(password), []byte(h.password)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"密码错误"}`))
		return
	}
	sessionID := generateSessionID()
	h.sessionTokens[sessionID] = time.Now().Add(24 * time.Hour)
	http.SetCookie(w, &http.Cookie{
		Name:     "devin_panel_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (h *Handler) isAuthenticated(r *http.Request) bool {
	if h.password == "" {
		return true
	}
	cookie, err := r.Cookie("devin_panel_session")
	if err != nil {
		return false
	}
	expiry, ok := h.sessionTokens[cookie.Value]
	if !ok || time.Now().After(expiry) {
		if ok {
			delete(h.sessionTokens, cookie.Value)
		}
		return false
	}
	return true
}

func (h *Handler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.isAuthenticated(r) {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"未授权"}`))
	return false
}

func (h *Handler) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	result := map[string]any{}

	// 正确路径：JSON Connect SeatManagement GetUserStatus（Bearer + metadata.api_key）
	if user, plan, planInfo, err := h.fetchUserStatus(ctx); err != nil {
		result["user_status_error"] = err.Error()
	} else {
		if user != nil {
			result["user"] = user
		}
		if plan != nil {
			result["plan_status"] = plan
		}
		if planInfo != nil {
			result["plan_info"] = planInfo
		}
	}

	capResp, err := h.apiClient.CheckChatCapacity(ctx, connect.NewRequest(&devinproto.CheckChatCapacityRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err != nil {
		result["capacity_error"] = err.Error()
	} else {
		result["capacity"] = map[string]any{
			"has_capacity":    capResp.Msg.GetHasCapacity(),
			"message":         capResp.Msg.GetMessage(),
			"active_sessions": capResp.Msg.GetActiveSessions(),
		}
	}

	statusResp, err := h.apiClient.GetStatus(ctx, connect.NewRequest(&devinproto.GetStatusRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err != nil {
		result["status_error"] = err.Error()
	} else {
		st := statusResp.Msg.GetStatus()
		result["ide_status"] = map[string]any{
			"level":   shortEnum(st.GetLevel().String(), "STATUS_LEVEL_"),
			"message": st.GetMessage(),
		}
		result["show_review_prompt"] = statusResp.Msg.GetShowReviewPrompt()
	}

	modelStatusResp, err := h.apiClient.GetModelStatuses(ctx, connect.NewRequest(&devinproto.GetModelStatusesRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err == nil {
		var statuses []map[string]any
		for _, s := range modelStatusResp.Msg.GetModelStatusInfos() {
			statuses = append(statuses, map[string]any{
				"model":  shortEnum(s.GetModel().String(), "MODEL_"),
				"status": shortEnum(s.GetStatus().String(), "MODEL_STATUS_"),
			})
		}
		result["model_statuses"] = statuses
	}

	providerResp, err := h.apiClient.GetModelProviders(ctx, connect.NewRequest(&devinproto.GetModelProvidersRequest{}))
	if err == nil {
		var providers []map[string]any
		for _, p := range providerResp.Msg.GetModelProviders() {
			providers = append(providers, map[string]any{
				"provider":     shortEnum(p.GetProvider().String(), "MODEL_PROVIDER_"),
				"display_name": p.GetDisplayName(),
			})
		}
		result["providers"] = providers
	}

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
	client := &http.Client{Timeout: 20 * time.Second, Transport: h.baseTransport}
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp, err := h.apiClient.GetCascadeModelConfigs(ctx, connect.NewRequest(&devinproto.GetCascadeModelConfigsRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"%s"}`, err.Error())
		return
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func buildMetadata(token string) *devinproto.ExaCodeiumCommonPb_Metadata {
	fingerprint, _ := randomHex(32)
	return &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String("win"),
		F:                proto.String(fingerprint),
	}
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// ApiServer Connect 客户端沿用 Basic token-token；Seat 单独走 Bearer。
	if clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Basic "+t.token+"-"+t.token)
	}
	return t.base.RoundTrip(clone)
}

func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateSessionID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shortEnum(full, prefix string) string {
	if full == "" {
		return ""
	}
	for _, p := range []string{
		"ExaCodeiumCommonPb_ModelProvider_MODEL_PROVIDER_",
		"ExaCodeiumCommonPb_APIProvider_API_PROVIDER_",
		"ExaCodeiumCommonPb_ModelPricingType_MODEL_PRICING_TYPE_",
		"ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_",
		"ExaCodeiumCommonPb_ModelDimensionKind_MODEL_DIMENSION_KIND_",
		"ExaCodeiumCommonPb_StatusLevel_STATUS_LEVEL_",
		"ExaCodeiumCommonPb_ModelStatus_MODEL_STATUS_",
		"ExaCodeiumCommonPb_TeamsTier_TEAMS_TIER_",
		"ExaCodeiumCommonPb_BillingStrategy_BILLING_STRATEGY_",
		"ExaCodeiumCommonPb_Model_",
		"MODEL_PROVIDER_", "API_PROVIDER_", "MODEL_PRICING_TYPE_", "MODEL_COST_TIER_",
		"MODEL_DIMENSION_KIND_", "STATUS_LEVEL_", "MODEL_STATUS_", "TEAMS_TIER_", "BILLING_STRATEGY_",
		"MODEL_",
		prefix,
	} {
		if p == "" {
			continue
		}
		if idx := strings.Index(full, p); idx >= 0 {
			return full[idx+len(p):]
		}
	}
	if i := strings.LastIndex(full, "_"); i >= 0 && i+1 < len(full) {
		return full[i+1:]
	}
	return full
}

func strAny(vals ...any) string {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case json.Number:
			return t.String()
		case float64:
			return fmt.Sprintf("%.0f", t)
		default:
			s := fmt.Sprint(t)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func boolAny(vals ...any) bool {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t == "true" || t == "1"
		}
	}
	return false
}

func numAny(vals ...any) any {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case float64, float32, int, int32, int64, json.Number:
			return t
		case string:
			if t != "" {
				return t
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
