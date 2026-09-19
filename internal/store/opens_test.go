package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestStoreOpensSnapshot 钉住台账快照的三件套：Total 全行数、Recent
// 按 id 倒序截到最近行、DistinctPIDs24h 只数回看窗内的 pid——陌生
// pid 附着与窗外老行各自归位。
func TestStoreOpensSnapshot(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// Open 已落一行（本进程 pid）。先补一行 24h 窗外的老 pid，再补
	// 一行窗内「外来者」——id 序即附着序，Recent 新在前。
	if _, err := s.db.Exec(`INSERT INTO store_opens(at, pid, argv, build, path) VALUES(?,?,?,?,?)`,
		now-25*3600*1000, 9999, `["old-probe"]`, "v0-old", s.path); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO store_opens(at, pid, argv, build, path) VALUES(?,?,?,?,?)`,
		now-1000, 4242, `["rogue","--attach"]`, "v9-rogue", s.path); err != nil {
		t.Fatalf("seed rogue: %v", err)
	}

	rep, err := s.StoreOpens(ctx)
	if err != nil {
		t.Fatalf("StoreOpens: %v", err)
	}
	if rep.Total != 3 {
		t.Fatalf("Total = %d, want 3", rep.Total)
	}
	if len(rep.Recent) != 3 {
		t.Fatalf("Recent len = %d, want 3", len(rep.Recent))
	}
	if rep.Recent[0].PID != 4242 || rep.Recent[0].Argv != `["rogue","--attach"]` || rep.Recent[0].Build != "v9-rogue" {
		t.Fatalf("Recent[0] = %+v, want rogue row first (id DESC)", rep.Recent[0])
	}
	if rep.Recent[2].PID != int64(os.Getpid()) {
		t.Fatalf("Recent[2].PID = %d, want this process %d", rep.Recent[2].PID, os.Getpid())
	}
	// 窗内：open 行(pid=self) + rogue(4242) = 2；9999 在窗外不计。
	if rep.DistinctPIDs24h != 2 {
		t.Fatalf("DistinctPIDs24h = %d, want 2", rep.DistinctPIDs24h)
	}
}

// TestStoreOpensMissingTable 钉住读侧的失败姿态：表缺席（老库、被
// 清理）时 StoreOpens 报错上抛——调用方据此省略投影组，观测面
// 不拖死端点。
func TestStoreOpensMissingTable(t *testing.T) {
	s := openTemp(t)
	if _, err := s.db.Exec(`DROP TABLE store_opens`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := s.StoreOpens(context.Background()); err == nil {
		t.Fatal("StoreOpens over dropped table = nil error, want failure")
	}
}
