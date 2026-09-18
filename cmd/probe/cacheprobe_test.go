package main

import "testing"

// okLeg 造一条成功腿的简写。
func okLeg(name string, cr int64) legResult {
	return legResult{Leg: name, Status: 200, CacheReadTokens: cr}
}

// TestClassifyGate 钉住门判读的分支语义：R 是基准、B 判轨迹是否进门、
// C 估变体 A 收益量级；比率阈值（0.7/0.3/0.5）与 hint 文本同源同测。
func TestClassifyGate(t *testing.T) {
	cases := []struct {
		name     string
		legs     []legResult
		wantHint string
		wantB    float64
		wantC    float64
	}{
		{
			name:     "content-addressed: fresh-traj resend hits like same-traj",
			legs:     []legResult{okLeg("A", 0), okLeg("R", 6000), okLeg("B", 5800), okLeg("C", 5900)},
			wantHint: "content-addressed",
			wantB:    5800.0 / 6000.0,
			wantC:    5900.0 / 6000.0,
		},
		{
			name:     "trajectory-gated: fresh-traj misses, warm-traj new-suffix reads",
			legs:     []legResult{okLeg("A", 0), okLeg("R", 6000), okLeg("B", 200), okLeg("C", 5500)},
			wantHint: "trajectory-gated",
			wantB:    200.0 / 6000.0,
			wantC:    5500.0 / 6000.0,
		},
		{
			name:     "unmeasurable: resend itself read nothing",
			legs:     []legResult{okLeg("A", 0), okLeg("R", 0), okLeg("B", 0), okLeg("C", 0)},
			wantHint: "unmeasurable",
		},
		{
			name:     "inconclusive: B in the gray band",
			legs:     []legResult{okLeg("A", 0), okLeg("R", 6000), okLeg("B", 3000), okLeg("C", 5500)},
			wantHint: "inconclusive",
		},
		{
			name:     "insufficient: no R leg",
			legs:     []legResult{okLeg("A", 0), okLeg("B", 100)},
			wantHint: "insufficient legs",
		},
		{
			name:     "insufficient: R failed",
			legs:     []legResult{okLeg("A", 0), {Leg: "R", Status: 429}, okLeg("B", 100), okLeg("C", 5000)},
			wantHint: "insufficient legs",
		},
		{
			// C 缺席时 c_over_r=0，低 B 落 default——暖轨迹可读性需要 C 佐证。
			name:     "no C leg: low B alone stays inconclusive",
			legs:     []legResult{okLeg("A", 0), okLeg("R", 6000), okLeg("B", 100)},
			wantHint: "inconclusive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyGate(tc.legs)
			if len(v.Hint) < len(tc.wantHint) || v.Hint[:len(tc.wantHint)] != tc.wantHint {
				t.Fatalf("hint = %q, want prefix %q", v.Hint, tc.wantHint)
			}
			if tc.wantB != 0 && abs(v.BOverR-tc.wantB) > 1e-9 {
				t.Fatalf("b_over_r = %v, want %v", v.BOverR, tc.wantB)
			}
			if tc.wantC != 0 && abs(v.COverR-tc.wantC) > 1e-9 {
				t.Fatalf("c_over_r = %v, want %v", v.COverR, tc.wantC)
			}
		})
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestParseLegs 校验腿字母表与解析边界：合法字母映射到预定的会话/内容
// 序号，未知字母与畸形段拒绝。
func TestParseLegs(t *testing.T) {
	legs, err := parseLegs("A,R,B,C")
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 4 {
		t.Fatalf("got %d legs", len(legs))
	}
	// A 与 R 同会话同内容（重发关系），B 换会话不换内容，C 换内容不换会话。
	if legs[0].sess != legs[1].sess || legs[0].msg != legs[1].msg {
		t.Fatal("A and R must share session and content")
	}
	if legs[2].sess == legs[0].sess || legs[2].msg != legs[0].msg {
		t.Fatal("B must switch session but keep content")
	}
	if legs[3].sess != legs[0].sess || legs[3].msg == legs[0].msg {
		t.Fatal("C must keep session but switch content")
	}
	for _, bad := range []string{"X", "A,X", "", "A,RR"} {
		if _, err := parseLegs(bad); err == nil {
			t.Fatalf("parseLegs(%q) should fail", bad)
		}
	}
}
