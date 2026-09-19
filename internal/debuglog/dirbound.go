// 本文件是调试目录名语法的唯一所有者。
//
// 目录名内嵌本地时区秒级时间戳（"20060102-150405"，同秒并发加 -NN 后缀），
// 字典序即时间序：store 侧把目录界当不透明字符串按字典序比较（谓词界圈出
// 删除/剥离范围），本文件负责全部「时刻↔界」换算与界的哨兵约定。
// 改目录格式只动这一个文件。
package debuglog

import (
	"fmt"
	"regexp"
	"time"
)

// dirTimeFormat 是目录名内嵌的时间戳布局（本地时区，秒级精度）。
const dirTimeFormat = "20060102-150405"

// requestDirPattern 约束请求目录名，防止伪造目录名探测库内其它行。
var requestDirPattern = regexp.MustCompile(`^\d{8}-\d{6}(-\d{2,})?$`)

// boundAll 是越过一切目录名的界哨兵：合法名全是 ASCII 数字与 '-'，"\xff"
// 字典序更大——枚举删除走到候选末尾、没有更大名可借时用它圈住全集。
const boundAll = "\xff"

// dirStamp 把时刻格式化为目录名基座。界与名同语法：DeleteDebug*Before
// 系列的 bound 参数就是某个时刻的 dirStamp。
func dirStamp(t time.Time) string {
	return t.Format(dirTimeFormat)
}

// suffixedDirName 在基座名上拼同秒并发后缀（-NN，序号从 2 起——
// 基座名本身相当于隐式 -01）。
func suffixedDirName(base string, n int) string {
	return fmt.Sprintf("%s-%02d", base, n)
}

// boundAfter 返回圈住 candidates[:i+1] 的字典序界：有下一候选名借它
// （dir<bound 正好圈出前缀），删到末尾用 boundAll。
func boundAfter(candidates []string, i int) string {
	if i+1 < len(candidates) {
		return candidates[i+1]
	}
	return boundAll
}
