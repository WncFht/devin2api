// randid 集中全仓库的随机 ID 生成，避免各协议包/上游调用各写一份
// crypto/rand 读取与格式化。
package randid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Hex 返回 size 字节随机数的十六进制编码。
func Hex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// UUID 返回随机 v4 UUID。crypto/rand 失败仅在理论可达，
// 返回全零 UUID 保持返回值可用而不引入错误分支。
func UUID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Prefixed 返回 prefix + 16 字节随机数的十六进制串，如 "msg_…"、"chatcmpl-…"。
// crypto/rand 失败时退化为纳秒时间戳，保持 ID 唯一性。
func Prefixed(prefix string) string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(value)
}
