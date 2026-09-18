//go:build linux

package obs

import (
	"os"
	"testing"
)

// TestStatmResidentBytes 验证 statm 解析取第 2 列常驻页数并换算字节。
func TestStatmResidentBytes(t *testing.T) {
	want := int64(12345) * int64(os.Getpagesize())
	if got := statmResidentBytes("67890 12345 678 0 0 999 0\n"); got != want {
		t.Fatalf("statmResidentBytes = %d, want %d", got, want)
	}
	if got := statmResidentBytes("garbage"); got != 0 {
		t.Fatalf("statmResidentBytes(garbage) = %d, want 0", got)
	}
}
