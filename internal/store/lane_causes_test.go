// 本文件验证 lane_attempt_causes 的两条写路径展开与读/删：InsertLog 与
// WriteDebugBatch 把 LogRow.SwitchCauses 按 (day,lane,cause) 累加进表
// （dir 撞唯一索引被 OR IGNORE 跳过的行不复记），LaneAttemptCauses
// 按日过滤、PruneLaneAttemptCauses 按日删除。
package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestLaneAttemptCausesRollup 验证 SwitchCauses 的同事务展开：两行各带
// 多成因经 InsertLog 累加、批量行经 WriteDebugBatch 合入；OR IGNORE
// 去重命中（dir 重放）的行不再贡献——写 worker 重试不会把计数翻倍。
func TestLaneAttemptCausesRollup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	// 钉在本地正午：跨午夜的测试运行不会把日行拆到两个日期键。
	today := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(),
		12, 0, 0, 0, time.Local)
	day := today.Format("2006-01-02")

	for _, r := range []*LogRow{
		{Dir: "c-1", StartedAt: today, Result: "completed", SwitchCauses: map[SwitchCause]int{
			{Lane: "alpha", Cause: "local_gate:latch"}:   1,
			{Lane: "alpha", Cause: "resource_exhausted"}: 1,
		}},
		{Dir: "c-2", StartedAt: today, Result: "completed", SwitchCauses: map[SwitchCause]int{
			{Lane: "alpha", Cause: "local_gate:latch"}: 1,
			{Lane: "bravo", Cause: "nocode"}:           1,
		}},
	} {
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog %s: %v", r.Dir, err)
		}
	}
	batch := DebugBatch{LogRows: []*LogRow{
		{Dir: "c-3", StartedAt: today, Result: "completed", SwitchCauses: map[SwitchCause]int{
			{Lane: "bravo", Cause: "local_gate:quota"}: 2,
		}},
	}}
	if err := s.WriteDebugBatch(ctx, batch); err != nil {
		t.Fatalf("WriteDebugBatch: %v", err)
	}
	// 同 dir 重放：logs 行被 OR IGNORE 跳过，归因贡献必须同步跳过。
	batch.LogRows[0].SwitchCauses = map[SwitchCause]int{
		{Lane: "bravo", Cause: "local_gate:quota"}: 5,
	}
	if err := s.WriteDebugBatch(ctx, batch); err != nil {
		t.Fatalf("WriteDebugBatch replay: %v", err)
	}

	got, err := s.LaneAttemptCauses(ctx, day)
	if err != nil {
		t.Fatalf("LaneAttemptCauses: %v", err)
	}
	want := []LaneAttemptCause{
		{Date: day, Lane: "alpha", Cause: "local_gate:latch", N: 2},
		{Date: day, Lane: "alpha", Cause: "resource_exhausted", N: 1},
		{Date: day, Lane: "bravo", Cause: "local_gate:quota", N: 2},
		{Date: day, Lane: "bravo", Cause: "nocode", N: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("causes = %+v, want %+v", got, want)
	}
}

// TestLaneAttemptCausesPrune 验证日过滤与按龄删除：早于 sinceDay 的
// 聚合行不读出，PruneLaneAttemptCauses 把它们删除（与 logs 摘要行
// 同一保留期）。
func TestLaneAttemptCausesPrune(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	today := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(),
		12, 0, 0, 0, time.Local)
	old := today.AddDate(0, 0, -40)
	day, oldDay := today.Format("2006-01-02"), old.Format("2006-01-02")

	for _, r := range []*LogRow{
		{Dir: "p-old", StartedAt: old, Result: "completed", SwitchCauses: map[SwitchCause]int{
			{Lane: "alpha", Cause: "local_gate"}: 1,
		}},
		{Dir: "p-new", StartedAt: today, Result: "completed", SwitchCauses: map[SwitchCause]int{
			{Lane: "alpha", Cause: "nocode"}: 1,
		}},
	} {
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog %s: %v", r.Dir, err)
		}
	}

	got, err := s.LaneAttemptCauses(ctx, day)
	if err != nil {
		t.Fatalf("LaneAttemptCauses: %v", err)
	}
	if len(got) != 1 || got[0].Date != day || got[0].Cause != "nocode" {
		t.Fatalf("sinceDay filter = %+v, want only today's nocode row", got)
	}
	if deleted, err := s.PruneLaneAttemptCauses(ctx, day); err != nil || deleted != 1 {
		t.Fatalf("PruneLaneAttemptCauses = %d/%v, want 1", deleted, err)
	}
	got, err = s.LaneAttemptCauses(ctx, oldDay)
	if err != nil {
		t.Fatalf("LaneAttemptCauses after prune: %v", err)
	}
	if len(got) != 1 || got[0].Date != day {
		t.Fatalf("after prune = %+v, want only today's row left", got)
	}
}
