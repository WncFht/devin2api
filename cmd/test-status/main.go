package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

func main() {
	// 从环境变量读取 Devin session token，避免把真实 token 写进代码仓库
	token := os.Getenv("DEVIN_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "请设置环境变量 DEVIN_TOKEN")
		os.Exit(1)
	}
	hosts := []string{
		"https://server.self-serve.windsurf.com",
		"https://server.codeium.com",
	}
	for _, base := range hosts {
		fmt.Println("====", base)
		transport := &authTransport{base: http.DefaultTransport, token: token}
		httpClient := &http.Client{Transport: transport, Timeout: 20 * time.Second}
		meta := &devinproto.ExaCodeiumCommonPb_Metadata{
			ApiKey: proto.String(token), ExtensionName: proto.String("chisel"), ExtensionVersion: proto.String("3000.2.17"),
			IdeName: proto.String("chisel"), IdeVersion: proto.String("3000.2.17"), Locale: proto.String("en"), Os: proto.String("win"),
		}
		// SeatManagement GetUserStatus
		seat := devinprotoconnect.NewExaSeatManagementPb_SeatManagementServiceClient(httpClient, base)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := seat.GetUserStatus(ctx, connect.NewRequest(&devinproto.ExaSeatManagementPb_GetUserStatusRequest{Metadata: meta}))
		cancel()
		if err != nil {
			fmt.Println("Seat GetUserStatus ERR:", err)
		} else {
			us := resp.Msg.GetUserStatus()
			fmt.Println("Name:", us.GetName(), "Email:", us.GetEmail(), "Pro:", us.GetPro())
			fmt.Println("UsedPrompt:", us.GetUserUsedPromptCredits(), "UsedFlow:", us.GetUserUsedFlowCredits())
			ps := us.GetPlanStatus()
			fmt.Println("AvailPrompt:", ps.GetAvailablePromptCredits(), "AvailFlow:", ps.GetAvailableFlowCredits(), "AvailFlex:", ps.GetAvailableFlexCredits())
			fmt.Println("Daily%:", ps.GetDailyQuotaRemainingPercent(), "Weekly%:", ps.GetWeeklyQuotaRemainingPercent())
			fmt.Println("ACU:", ps.GetAcuConsumed(), "/", ps.GetAcuLimit())
			if pi := ps.GetPlanInfo(); pi != nil {
				fmt.Println("Plan:", pi.GetPlanName(), "MonthlyPrompt:", pi.GetMonthlyPromptCredits())
			}
			if pi := resp.Msg.GetPlanInfo(); pi != nil {
				fmt.Println("RespPlan:", pi.GetPlanName())
			}
		}
		// ApiServer CheckChatCapacity
		api := devinprotoconnect.NewApiServerServiceClient(httpClient, base)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		cap, err := api.CheckChatCapacity(ctx2, connect.NewRequest(&devinproto.CheckChatCapacityRequest{Metadata: meta}))
		cancel2()
		if err != nil {
			fmt.Println("CheckChatCapacity ERR:", err)
		} else {
			fmt.Println("Capacity:", cap.Msg.GetHasCapacity(), "sessions:", cap.Msg.GetActiveSessions(), "msg:", cap.Msg.GetMessage())
		}
	}
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.Header.Set("Authorization", "Basic "+t.token+"-"+t.token)
	return t.base.RoundTrip(c)
}
