// 本文件是 /admin|/dashboard/stats 的进程内结果缓存。面板轮询把同一
// 聚合查询高频重打，一轮聚合是格子扫描（一遍喂 per-model/rpm/健康桶
// 三个累加器）+ recentWindow×2 + lastByModel + GROUP BY recentRPM 的
// 查询组——抄 ccLoad StatsCache 的模式：miss 回源全价算，命中直接回
// 放。键按生效参数（解析后范围+过滤+身份/账号收敛），TTL 按数据新旧
// 分档：含「现在」的窗口还在变给短档，已封存的历史窗口给长档。
// （/admin/usage 另有 usageSnapshot 的 singleflight+stale 机制，不经
// 本缓存。）
package ccpanel

import (
	"fmt"
	"sync"
	"time"
)

// statsCache 是有界 TTL 结果缓存：map+mutex，写满先清过期项、仍满则
// 整体重置——端点少、key 空间小，不需要逐出策略。data 缓存的是
// respondOK 的载荷（map/结构），存后不再改，命中时按只读共享重编码。
type statsCache struct {
	mu      sync.Mutex
	entries map[string]statsCacheEntry
}

type statsCacheEntry struct {
	data   any
	expiry time.Time
}

const statsCacheMaxEntries = 512

func newStatsCache() *statsCache {
	return &statsCache{entries: make(map[string]statsCacheEntry)}
}

func (c *statsCache) load(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiry) {
		return nil, false
	}
	return e.data, true
}

func (c *statsCache) store(key string, data any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, updating := c.entries[key]; !updating && len(c.entries) >= statsCacheMaxEntries {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expiry) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= statsCacheMaxEntries {
			clear(c.entries)
		}
	}
	c.entries[key] = statsCacheEntry{data: data, expiry: time.Now().Add(ttl)}
}

// statsTTL 按窗口右端的新旧给 TTL（ccLoad calculateTTL 同档）：右端
// 贴「现在」的数据仍在变，越旧的窗口越接近封存，聚合结果敢给长 TTL。
func statsTTL(until, now time.Time) time.Duration {
	switch {
	case until.After(now.Add(-time.Hour)):
		return 30 * time.Second
	case until.After(now.Add(-24 * time.Hour)):
		return 5 * time.Minute
	case until.After(now.Add(-7 * 24 * time.Hour)):
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}

// statsCacheKey 生成聚合结果缓存键并给出该条的 TTL。范围取解析后的
// since/until——不同拼法的同效窗口（range=today 与等价的 custom）共享
// 条目；rangeName 单列入键：同窗口下 is_today 不同响应不同。活动窗口
// 的 until≈now 每秒在变，按最短档 TTL 把右端分桶取整，否则永不命中
// （ccLoad cacheKeyEndUnix 同手法）；封存窗口 until 固定，精确值入键。
// scope 全字段入键：kh 折叠 api_token/auth_token_id 身份维，account
// 折叠号池 lane 维。
func statsCacheKey(rangeName string, since, until time.Time, scope statScope, now time.Time) (string, time.Duration) {
	ttl := statsTTL(until, now)
	endUnix := until.Unix()
	if ttl <= 30*time.Second {
		bucket := int64(ttl / time.Second)
		endUnix = endUnix / bucket * bucket
	}
	return fmt.Sprintf("%s|%d|%d|%s|%s|%s|%s|%s",
		rangeName, since.Unix(), endUnix, scope.kh, scope.api, scope.model, scope.modelLike, scope.account), ttl
}
