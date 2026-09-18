// 本文件验证容量淘汰的集合化删除与旧逐目录实现的等价性。
package debuglog

import (
	"context"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/WncFht/devin2api/internal/store"
)

// capacityScenario 是一组容量淘汰场景：dirs 按名字典序构造（名序即时间序），
// 含 error.json 的目录进入保护候选，activeDirs 仍在写入必须豁免。
// sizes 取不可压缩内容的库存字节（>1MB 的随机数据不触发透明压缩，
// 库存尺寸即写入长度），policy 的 MB 粒度上限由此精确对齐。
type capacityScenario struct {
	name          string
	sizes         map[string]int64 // dir → 单文件库存字节
	errorDirs     []string
	activeDirs    []string
	maxTotalMB    int64
	keepErrorDirs int
}

// referenceCapacityEvict 复刻旧逐目录淘汰语义：排序候选、跳过活跃与保护
// 目录、累计删除到回到上限内——返回「旧实现会删的目录集合」作断言基准。
// 保护集与生产代码同出 protectedErrorDirs（该函数未改，等价性只针对
// 删除集合的圈定方式：逐目录 autocommit vs 单条 dir<bound 集合删除）。
func referenceCapacityEvict(t *testing.T, st *store.Store, manager *Manager, sc capacityScenario) map[string]bool {
	t.Helper()
	ctx := context.Background()
	sizes, err := st.DebugDirSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]struct{}{}
	for _, dir := range sc.activeDirs {
		active[dir] = struct{}{}
	}
	var totalBytes int64
	var candidates []string
	for dir, size := range sizes {
		if _, ok := active[dir]; ok {
			continue
		}
		totalBytes += size
		candidates = append(candidates, dir)
	}
	deleted := map[string]bool{}
	if totalBytes <= sc.maxTotalMB<<20 {
		return deleted
	}
	sort.Strings(candidates)
	errorDirs := map[string]bool{}
	if sc.keepErrorDirs > 0 {
		if errorDirs, err = st.DebugDirsContaining(ctx, ErrorFile); err != nil {
			t.Fatal(err)
		}
	}
	protected := manager.protectedErrorDirs(ctx, candidates, errorDirs, sc.keepErrorDirs)
	for _, dir := range candidates {
		if totalBytes <= sc.maxTotalMB<<20 {
			break
		}
		if protected[dir] {
			continue
		}
		deleted[dir] = true
		totalBytes -= sizes[dir]
	}
	return deleted
}

// TestCapacityEvictionMatchesPerDirLoop 对每组场景先算旧实现的删除集
// （参照函数），再跑真 cleanOnce 断言删后剩余集合逐项相等——集合化
// 删除必须是纯等价变换。
func TestCapacityEvictionMatchesPerDirLoop(t *testing.T) {
	const mb = 1 << 20
	scenarios := []capacityScenario{
		{
			// 保护目录交错在删除界内：d02/d04 跳删但界仍越过它们，
			// 验证集合谓词能表达「界内豁免」。
			name:          "protected-interleaved",
			sizes:         map[string]int64{"d01": 2 * mb, "d02": 1 * mb, "d03": 1 * mb, "d04": 1 * mb, "d05": 1 * mb},
			errorDirs:     []string{"d02", "d04"},
			maxTotalMB:    3, // need=3MB → 删 d01/d03
			keepErrorDirs: 4,
		},
		{
			// 界恰好落在 cap 上：首个候选累计即达 need，后续目录全留。
			name:       "boundary-on-cap",
			sizes:      map[string]int64{"d01": 3 * mb, "d02": 2 * mb, "d03": 1 * mb},
			maxTotalMB: 3, // need=3MB → 只删 d01
		},
		{
			// 界越过末位候选（"\xff" 分支）：候选全部删光也回不到上限，
			// 末位之后的目录名没有候选可借，须走超位界。
			name:          "delete-past-last-candidate",
			sizes:         map[string]int64{"d01": 2 * mb, "d02": 2 * mb},
			maxTotalMB:    1, // need=3MB → d01+d02 全删
			keepErrorDirs: 0,
		},
		{
			// 全部候选受保护：旧实现一个都删不掉（last<0 分支）。
			name:          "all-protected",
			sizes:         map[string]int64{"d01": 2 * mb, "d02": 2 * mb},
			errorDirs:     []string{"d01", "d02"},
			maxTotalMB:    1,
			keepErrorDirs: 4,
		},
		{
			// 活跃目录名小于删除界（长流仍在写旧名目录）：谓词界圈不出它，
			// 必须靠 exclude 豁免——漏掉它就误删在写目录。
			name:          "active-below-bound",
			sizes:         map[string]int64{"20200101-000000": 2 * mb, "d01": 1 * mb, "d02": 1 * mb},
			activeDirs:    []string{"20200101-000000"},
			maxTotalMB:    1, // 候选=d01/d02，删 d01；旧名活跃目录必留
			keepErrorDirs: 0,
		},
		{
			// 上限未满：早退分支，什么也不删。
			name:       "under-cap",
			sizes:      map[string]int64{"d01": 1 * mb},
			maxTotalMB: 4,
		},
		{
			// 零目录：候选空集，早退。
			name:       "empty",
			sizes:      map[string]int64{},
			maxTotalMB: 1,
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			st := openTestStore(t)
			ctx := context.Background()
			// 随机内容不触发透明压缩（压不出 10% 收益按原样存），
			// 库存字节即写入长度——sizes 与 DebugDirSizes 逐项对齐。
			rng := rand.New(rand.NewSource(1))
			for dir, size := range sc.sizes {
				payload := make([]byte, size)
				rng.Read(payload)
				name := MetaFile
				if slices.Contains(sc.errorDirs, dir) {
					name = ErrorFile
				}
				if err := st.PutDebugFile(ctx, dir, name, payload); err != nil {
					t.Fatal(err)
				}
			}
			manager := NewManager(filepath.Join(t.TempDir(), "logs"),
				RetentionPolicy{MaxTotalMB: sc.maxTotalMB, KeepErrorDirs: sc.keepErrorDirs}, st)
			defer manager.Close()
			// 活跃目录直接登记（等价 Start 后的在写状态）：cleanOnce 只
			// 读键集，recorder 内部字段不参与淘汰判定。
			for _, dir := range sc.activeDirs {
				manager.activeDirs[dir] = &Recorder{manager: manager, dir: dir}
			}
			expected := referenceCapacityEvict(t, st, manager, sc)
			removed := manager.cleanOnce()
			if int64(len(expected)) != int64(removed) {
				t.Fatalf("removed = %d, want %d (expected deleted %v)", removed, len(expected), expected)
			}
			remaining, err := st.DebugDirs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			remainingSet := map[string]bool{}
			for _, dir := range remaining {
				remainingSet[dir] = true
			}
			for dir := range sc.sizes {
				if remainingSet[dir] == !expected[dir] {
					continue
				}
				if expected[dir] {
					t.Fatalf("dir %s unexpectedly kept; remaining %v want-deleted %v", dir, remaining, expected)
				}
				t.Fatalf("dir %s unexpectedly deleted; remaining %v want-deleted %v", dir, remaining, expected)
			}
		})
	}
}
