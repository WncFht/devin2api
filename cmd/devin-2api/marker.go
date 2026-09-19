// 本文件是 logs/ 下 JSON 取证标记的统一机制：事件侧读改写累计（first_at/
// last_at/count + 事件字段），恢复侧补一条告警并用 recovered_at 记忆已
// 汇报到哪。标记刻意不依赖 db 健康——端口冲突与配置兜底正是 db 可能
// 不可用时的事故证据。
package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/WncFht/devin2api/internal/config"
)

// forensicMarker 是取证标记的磁盘形态：first/last/count 与 recovered_at
// 由机制统一记账；addr/holder 与 reason/cached_at 分属两种标记的事件
// 字段，omitempty 保证各自文件里的 JSON 键序与旧格式逐字节一致。
type forensicMarker struct {
	FirstAt     string `json:"first_at"`
	LastAt      string `json:"last_at"`
	Count       int    `json:"count"`
	Addr        string `json:"addr,omitempty"`
	Holder      string `json:"holder,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CachedAt    string `json:"cached_at,omitempty"`
	RecoveredAt string `json:"recovered_at,omitempty"`
}

// bumpMarker 读改写一份取证标记：文件缺席或不可解从空白开始，first/last/
// count 由这里记账，事件字段由 fill 填写。文件只增不删——恢复后的汇报
// 与清理由成功侧负责。
func bumpMarker(path string, fill func(*forensicMarker)) {
	var marker forensicMarker
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &marker)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if marker.Count == 0 {
		marker.FirstAt = now
	}
	marker.LastAt = now
	marker.Count++
	fill(&marker)
	if raw, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// warnIfRecovered 读取证标记：有未汇报过的事件（last_at 晚于上次钉住的
// recovered_at）就打 msg 告警并把 recovered_at 推进到当前时刻——事故
// 复盘靠这条边界知道「冲突/兜底期已经结束」。attrs 拼各标记自己的告警
// 字段，只在真正告警时才求值。
func warnIfRecovered(path, msg string, attrs func(*forensicMarker) []any) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var marker forensicMarker
	if err := json.Unmarshal(raw, &marker); err != nil || marker.Count == 0 {
		return
	}
	if marker.RecoveredAt != "" && marker.LastAt <= marker.RecoveredAt {
		return
	}
	slog.Warn(msg, attrs(&marker)...)
	marker.RecoveredAt = time.Now().UTC().Format(time.RFC3339)
	if raw, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// bindFailureFile 是 EADDRINUSE 退出前落在 logs/ 的冲突标记名。
const bindFailureFile = "bind-failure.json"

// recordBindFailure 给 bind-failure.json 记一次端口冲突：count 加一，
// holder 取最新一次探活结果。
func recordBindFailure(logRoot, listen, holder string) {
	bumpMarker(filepath.Join(logRoot, bindFailureFile), func(m *forensicMarker) {
		m.Addr = listen
		m.Holder = holder
	})
}

// warnIfBindContentionRecovered 在成功绑定后读冲突标记：上一轮
// EADDRINUSE 循环（KeepAlive 拉起 vs 旧实例排空）若发生过，补一条
// 恢复告警把「冲突已解除、共失败几次、谁占的坑」并进 stderr.log。
func warnIfBindContentionRecovered(logRoot string) {
	warnIfRecovered(filepath.Join(logRoot, bindFailureFile), "port contention recovered",
		func(m *forensicMarker) []any {
			return []any{"addr", m.Addr, "holder", m.Holder,
				"count", m.Count, "first_at", m.FirstAt, "last_at", m.LastAt}
		})
}

// configFallbackFile 是兜底服役事件落在 logs/ 的标记名。
const configFallbackFile = "config-fallback.json"

// recordConfigFallback 给 config-fallback.json 记一次兜底服役：count 加一，
// reason 取最新一次加载失败原因，cached_at 是所服缓存的写入时刻。
func recordConfigFallback(logRoot string, loadErr error, cached config.LastGood) {
	bumpMarker(filepath.Join(logRoot, configFallbackFile), func(m *forensicMarker) {
		m.Reason = loadErr.Error()
		m.CachedAt = cached.CachedAt.UTC().Format(time.RFC3339)
	})
}

// warnIfConfigFallbackRecovered 在配置成功加载后读兜底标记：上一轮兜底
// 服役若发生过，补一条恢复告警把「兜底过几次、最新失败原因、服的缓存
// 时刻」并进 stderr.log——兜底期的事故复盘需要这条边界。
func warnIfConfigFallbackRecovered(logRoot string) {
	warnIfRecovered(filepath.Join(logRoot, configFallbackFile), "config fallback recovered",
		func(m *forensicMarker) []any {
			return []any{"count", m.Count, "reason", m.Reason,
				"first_at", m.FirstAt, "last_at", m.LastAt, "cached_at", m.CachedAt}
		})
}
