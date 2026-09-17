package ccpanel

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// resolveRange 的月份分支曾因锚错日号回归（beginMonth 用了 t.Day()
// 而非 1），表驱动把每个分支的边界语义钉死。
func TestResolveRange(t *testing.T) {
	loc := time.Local
	at := func(y int, m time.Month, d, hh, mm, ss int) time.Time {
		return time.Date(y, m, d, hh, mm, ss, 0, loc)
	}
	now := at(2026, 9, 17, 15, 4, 5) // 周四

	cases := []struct {
		query         string
		wantSince     time.Time
		wantUntil     time.Time
		wantRangeName string
	}{
		{"", at(2026, 9, 17, 0, 0, 0), now, "today"},
		{"range=today", at(2026, 9, 17, 0, 0, 0), now, "today"},
		{"range=bogus", at(2026, 9, 17, 0, 0, 0), now, "today"},
		{"range=yesterday", at(2026, 9, 16, 0, 0, 0), at(2026, 9, 17, 0, 0, 0).Add(-time.Nanosecond), "yesterday"},
		{"range=day_before_yesterday", at(2026, 9, 15, 0, 0, 0), at(2026, 9, 16, 0, 0, 0).Add(-time.Nanosecond), "day_before_yesterday"},
		{"range=this_week", at(2026, 9, 14, 0, 0, 0), now, "this_week"},
		{"range=last_week", at(2026, 9, 7, 0, 0, 0), at(2026, 9, 14, 0, 0, 0).Add(-time.Nanosecond), "last_week"},
		{"range=this_month", at(2026, 9, 1, 0, 0, 0), now, "this_month"},
		{"range=last_month", at(2026, 8, 1, 0, 0, 0), at(2026, 9, 1, 0, 0, 0).Add(-time.Nanosecond), "last_month"},
		{"range=custom&start_time=1789600000000&end_time=1789620000000",
			time.UnixMilli(1789600000000), time.UnixMilli(1789620000000), "custom"},
		// custom 的 end 超 now 截到 now；非法参数回落 today。
		{"range=custom&start_time=1789600000000&end_time=99999999999999",
			time.UnixMilli(1789600000000), now, "custom"},
		{"range=custom&start_time=abc&end_time=def", at(2026, 9, 17, 0, 0, 0), now, "custom"},
		{"range=custom", at(2026, 9, 17, 0, 0, 0), now, "custom"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/dashboard/summary?"+c.query, nil)
		since, until, name := resolveRange(r, now)
		if !since.Equal(c.wantSince) || !until.Equal(c.wantUntil) || name != c.wantRangeName {
			t.Errorf("query %q: got (%v, %v, %q), want (%v, %v, %q)",
				c.query, since, until, name, c.wantSince, c.wantUntil, c.wantRangeName)
		}
	}
}

// 每月 1 号凌晨是 beginMonth 回归最伤的场景：旧错误实现会让
// this_month 的 since（今天 01:00）晚于 until（00:30），范围倒置。
func TestResolveRangeFirstOfMonth(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.Local)
	r := httptest.NewRequest("GET", "/x?range=this_month", nil)
	since, until, _ := resolveRange(r, now)
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)
	if !since.Equal(want) || !until.Equal(now) || since.After(until) {
		t.Fatalf("first-of-month this_month: got (%v, %v)", since, until)
	}

	r = httptest.NewRequest("GET", "/x?range=last_month", nil)
	since, until, _ = resolveRange(r, now)
	wantSince := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	wantUntil := want.Add(-time.Nanosecond)
	if !since.Equal(wantSince) || !until.Equal(wantUntil) {
		t.Fatalf("first-of-month last_month: got (%v, %v), want (%v, %v)",
			since, until, wantSince, wantUntil)
	}
}

// custom 的 start/end 用 url.Values 走一遍真实 query 编码路径。
func TestResolveRangeCustomEncoding(t *testing.T) {
	now := time.Date(2026, 9, 17, 15, 0, 0, 0, time.Local)
	v := url.Values{"range": {"custom"}, "start_time": {"1789600000000"}, "end_time": {"1789620000000"}}
	r := httptest.NewRequest("GET", "/x?"+v.Encode(), nil)
	since, until, _ := resolveRange(r, now)
	if !since.Equal(time.UnixMilli(1789600000000)) || !until.Equal(time.UnixMilli(1789620000000)) {
		t.Fatalf("custom: got (%v, %v)", since, until)
	}
}
