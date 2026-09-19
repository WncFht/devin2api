// 本文件提供请求顶层字段的未消费扫描：解码器把识别不了/不支持的字段
// 记入 Dropped，避免客户端以为自己设置了 reasoning/store 等参数而实际
// 被静默丢弃。
package common

import (
	"encoding/json"
	"slices"
	"sort"
)

// UnconsumedFields 返回请求顶层中不在 consumed 名单内的字段名
// （"field:<name>" 形式），供解码器记入 RequestMessages.Dropped。
// 上游没有对应 wire 字段的参数必须透出为丢弃，而不是静默吞掉。
// data 不是合法 JSON 对象时返回 nil——解码错误由调用方的主解码路径上报。
//
// 三个调用点都在主 Unmarshal 成功之后喂入 data，故这里只做顶层键名
// 提取：值的位置用裸字节扫描跳过（字符串转义跳读、嵌套括号配对），
// 不为每个值建 RawMessage 拷贝——整包 Unmarshal 进 map 会把请求体
// （messages/tools 大头全在值里）再拷一份。
func UnconsumedFields(data []byte, consumed map[string]bool) []string {
	pos := skipJSONSpace(data, 0)
	if pos >= len(data) || data[pos] != '{' {
		return nil
	}
	pos++
	var dropped []string
	for {
		pos = skipJSONSpace(data, pos)
		if pos >= len(data) {
			return nil
		}
		if data[pos] == '}' {
			break
		}
		if data[pos] != '"' {
			return nil
		}
		end := skipJSONString(data, pos)
		var name string
		// 键含转义时交给 stdlib 反转义——原始区间本就是合法 JSON
		// 字符串，这里只为取值不为找界。
		if err := json.Unmarshal(data[pos:end], &name); err != nil {
			return nil
		}
		if !consumed[name] {
			dropped = append(dropped, "field:"+name)
		}
		pos = skipJSONSpace(data, end)
		if pos >= len(data) || data[pos] != ':' {
			return nil
		}
		pos = skipJSONValue(data, pos+1)
		pos = skipJSONSpace(data, pos)
		if pos >= len(data) {
			return nil
		}
		if data[pos] == ',' {
			pos++
		}
	}
	// map 形态的键天然唯一；顶层重复键（合法但病态）这里要去重对齐。
	sort.Strings(dropped)
	return slices.Compact(dropped)
}

// skipJSONSpace 跳过 pos 起的 JSON 空白符。
func skipJSONSpace(data []byte, pos int) int {
	for pos < len(data) && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\n' || data[pos] == '\r') {
		pos++
	}
	return pos
}

// skipJSONString 跳过 pos（开引号）起的字符串字面量，返回闭引号后一
// 字节；\" 回跳两位即覆盖全部转义——\uXXXX 的六位里不含引号与反斜线。
func skipJSONString(data []byte, pos int) int {
	pos++
	for pos < len(data) {
		switch data[pos] {
		case '\\':
			pos += 2
			continue
		case '"':
			return pos + 1
		}
		pos++
	}
	return pos
}

// skipJSONValue 跳过 pos 起的一个完整 JSON 值：字符串走跳读，对象/数组
// 按括号配对（配对不受字符串内容影响），字面量扫到结构界。
func skipJSONValue(data []byte, pos int) int {
	pos = skipJSONSpace(data, pos)
	if pos >= len(data) {
		return pos
	}
	switch data[pos] {
	case '"':
		return skipJSONString(data, pos)
	case '{', '[':
		depth := 0
		for pos < len(data) {
			switch data[pos] {
			case '"':
				pos = skipJSONString(data, pos)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return pos + 1
				}
			}
			pos++
		}
		return pos
	default:
		for pos < len(data) && data[pos] != ',' && data[pos] != '}' && data[pos] != ']' &&
			data[pos] != ' ' && data[pos] != '\t' && data[pos] != '\n' && data[pos] != '\r' {
			pos++
		}
		return pos
	}
}
