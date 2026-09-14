// 本文件提供 OpenAI 系 usage 投影共用的 token 合计口径。
package common

import "github.com/WncFht/devin2api/internal/llm"

// UsageTotals 返回 OpenAI 系 usage 的输入与总计 token 口径：输入把
// cache_read/cache_write 并进 prompt（缓存命中同样是已消耗的输入），
// 上游未给 total_tokens 时按 input+output 兜底合成。
func UsageTotals(usage llm.Usage) (input int64, total int64) {
	input = usage.Input + usage.CacheRead + usage.CacheWrite
	total = usage.TotalTokens
	if total == 0 {
		total = input + usage.Output
	}
	return input, total
}
