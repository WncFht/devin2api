// 本文件实现调试日志写盘值的脱敏与附件落库——recorder.go 的净化器实现。
//
// sanitize/sanitizeValue 把投影产物归一成 any 树并遮盖敏感键；extractImage 族
// 把 data URL / base64 图片从树中摘出写入 attachments/ 名下并留引用指针。脱敏规则
// 表（secretKeyNames 等）是写盘前最后一道闸，键名匹配口径在本文件收拢。
package debuglog

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
)

// sanitizeJSON 把待写值序列化为脱敏后的 JSON 字节并报告其 marshal
// 洁净度：先拿到原始 JSON（json.Marshaler 用其 MarshalJSON——
// protoJSON/SSE 包装的惰性序列化留在编码协程；其余类型一次
// json.Marshal），rawNeedsSanitize 预筛干净即原样采用——绝大多数
// 记录是自产投影/SSE 帧，unmarshal 建树+树遍历+重排的三趟成本在
// 每条 delta 上是纯开销；命中敏感键或内联图片才走完整脱敏。
// marshalClean 表示返回字节是 json.Marshal 直产（紧凑+HTML 转义
// 齐全，含脱敏慢路径与兜底的重 marshal）——Marshaler 快路径产物
// 不保证，供 JSONLRecord.DataMarshalClean 决定是否跳过外层
// compaction/转义复扫。与旧 sanitize 的语义差异：map/slice 输入
// 不再被原地改写（脱敏作用于 unmarshal 出的私有副本）。
func (recorder *Recorder) sanitizeJSON(value any) (data []byte, marshalClean bool) {
	var err error
	if marshaler, ok := value.(json.Marshaler); ok {
		data, err = marshaler.MarshalJSON()
	} else {
		if data, err = json.Marshal(value); err == nil {
			marshalClean = true
		}
	}
	if err == nil {
		if !rawNeedsSanitize(data) {
			return data, marshalClean
		}
		var generic any
		if err = json.Unmarshal(data, &generic); err == nil {
			if data, err = json.Marshal(recorder.sanitizeValue(generic, false)); err == nil {
				return data, true
			}
		}
	}
	fallback, _ := json.Marshal(map[string]any{"serialization_error": err.Error()})
	return fallback, true
}

// writeSanitizedJSONLine 把脱敏后的 JSON 连带结尾 '\n' 写进 buf——
// 与 sanitizeJSON 同一净化口径（输出字节等于 sanitizeJSON 结果 + '\n'），
// 但编码直写调用方缓冲：json.Marshal 的定长拷贝与 append(data,'\n')
// 的二次整拷贝都被省掉。非 Marshaler 值走 Encoder.Encode——它输出
// 自带一个 '\n'，先摘掉再统一补回，净化慢路径同样归一。仅写整文件
// 行（WriteJSON）使用，JSONL 的 data 段不含行尾换行仍用 sanitizeJSON。
func (recorder *Recorder) writeSanitizedJSONLine(buf *bytes.Buffer, value any) {
	start := buf.Len()
	var err error
	if marshaler, ok := value.(json.Marshaler); ok {
		// MarshalJSON 产物原样采用——与 sanitizeJSON 同口径：不替
		// marshaler 做 compaction，非法输出也按原文走预筛定夺。
		var data []byte
		if data, err = marshaler.MarshalJSON(); err == nil {
			buf.Write(data)
		}
	} else if err = json.NewEncoder(buf).Encode(value); err == nil {
		buf.Truncate(buf.Len() - 1)
	}
	if err == nil {
		data := buf.Bytes()[start:]
		if !rawNeedsSanitize(data) {
			buf.WriteByte('\n')
			return
		}
		var generic any
		if err = json.Unmarshal(data, &generic); err == nil {
			buf.Truncate(start)
			if err = json.NewEncoder(buf).Encode(recorder.sanitizeValue(generic, false)); err == nil {
				return
			}
		}
	}
	buf.Truncate(start)
	fallback, _ := json.Marshal(map[string]any{"serialization_error": err.Error()})
	buf.Write(fallback)
	buf.WriteByte('\n')
}

// sanitizeValue 递归脱敏 any 树。metadataScope 标记当前子树是否位于某个
// "metadata" 键之下——上游 Metadata.f（设备指纹）只在这一作用域内敏感，
// 全局脱敏会把客户端请求体里同名的 "f" 键一并遮盖。
func (recorder *Recorder) sanitizeValue(value any, metadataScope bool) any {
	switch value := value.(type) {
	case []any:
		for index := range value {
			value[index] = recorder.sanitizeValue(value[index], metadataScope)
		}
		return value
	case map[string]any:
		for key := range value {
			if secretKey(key) || (metadataScope && metadataSecretKey(key)) {
				value[key] = "<redacted>"
			}
		}
		if dataKey, reference, ok := recorder.extractImage(value); ok {
			// 只把携带 base64 正文的键换成附件引用：兄弟键（type:"image"、
			// cache_control 等）是投影证据的一部分，整节点替换会把它们
			// 从落盘 JSON 里抹掉。
			value[dataKey] = reference
		}
		for key, item := range value {
			value[key] = recorder.sanitizeValue(item, metadataScope || isMetadataKey(key))
		}
		return value
	case string:
		if strings.HasPrefix(value, "data:image/") {
			if reference, ok := recorder.writeDataURL(value); ok {
				return reference
			}
		}
		return value
	case json.RawMessage:
		// 嵌套的原始 JSON（如 01 的请求体）：沿用顶层的预筛口径，
		// 干净即原样透传免建树，命中敏感键/图片才 unmarshal 走完整脱敏。
		if !rawNeedsSanitize(value) {
			return value
		}
		var generic any
		if err := json.Unmarshal(value, &generic); err != nil {
			return value
		}
		return recorder.sanitizeValue(generic, false)
	default:
		return value
	}
}

// secretKeyNames 是会被脱敏的 JSON 键名（剔除 '_'/'-'、小写归一化后的形态）。
// secretKey 与 rawNeedsSanitize 共用同一份名单，避免两处漂移。
// obs/diagnostic.go 的 sensitiveAssignmentPattern 是本名单在自由文本错误上的
// 正则形态（那边按 [\s_-]* 分隔匹配原文键名）——增删要两侧同步。
var secretKeyNames = []string{
	"authorization", "cookie", "setcookie", "apikey", "accesskey", "token",
	"sessiontoken", "accesstoken", "refreshtoken", "bearertoken", "password",
	"clientsecret", "devicefingerprint",
	// modelAssignmentJwt 是 AssignModel 按请求签发的 router jwt，03 请求
	// 体里的凭证级字段；归一化形态（去 _/-、小写）列入名单。
	"modelassignmentjwt",
}

// keyNormalizer 归一化 JSON 键名：剔除 '_' 与 '-'，配合小写折叠让
// api_key / api-key / APIKEY 等变体命中同一份名单。
var keyNormalizer = strings.NewReplacer("_", "", "-", "")

// metadataSecretKeyNames 是只在 metadata 对象内才算敏感的键名：上游
// Metadata.f 是设备指纹必须脱敏，但 "f" 作为通用短键名在客户端负载里
// 合法存在，放到全局名单会误伤排障现场。
var metadataSecretKeyNames = []string{"f"}

func secretKey(key string) bool {
	normalized := strings.ToLower(keyNormalizer.Replace(key))
	for _, name := range secretKeyNames {
		if normalized == name {
			return true
		}
	}
	return false
}

// metadataSecretKey 判定仅 metadata 作用域内敏感的键名，归一方式同 secretKey。
func metadataSecretKey(key string) bool {
	normalized := strings.ToLower(keyNormalizer.Replace(key))
	for _, name := range metadataSecretKeyNames {
		if normalized == name {
			return true
		}
	}
	return false
}

// isMetadataKey 判定键是否进入 metadata 作用域（归一方式同 secretKey）。
func isMetadataKey(key string) bool {
	return strings.ToLower(keyNormalizer.Replace(key)) == "metadata"
}

// rawNeedsSanitize 预筛 JSON 记录：含内联图片或敏感键名才需要完整的
// unmarshal+树遍历脱敏。判定口径与 sanitizeValue 对齐："image/" 子串同时
// 覆盖 data:image/ 值与 {"mime_type":"image/*","data":...} 对象两种图片
// 形态；键名只在 "key": 位置匹配（小写+剔除 '_'/'-' 归一，与 secretKey 一致），
// 字符串值里的同名文本不再误进慢路径。键名含转义序列时预筛看到的不是
// 解码后的真实键名（如 JSON 转义拼出的 apikey），无法按字节归一，
// 同样送慢路径定夺。扫描零分配。
func rawNeedsSanitize(data []byte) bool {
	if bytes.Contains(data, []byte("image/")) {
		return true
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		end := i + 1
		escaped := false
		for end < len(data) && data[end] != '"' {
			if data[end] == '\\' {
				escaped = true
				end++
			}
			end++
		}
		if end >= len(data) {
			break
		}
		colon := end + 1
		for colon < len(data) && (data[colon] == ' ' || data[colon] == '\t' || data[colon] == '\r' || data[colon] == '\n') {
			colon++
		}
		if colon < len(data) && data[colon] == ':' && (escaped || secretKeySpan(data[i+1:end])) {
			return true
		}
		i = end
	}
	return false
}

// secretKeySpan 判定引号内的键名是否命中脱敏名单，归一方式与 secretKey
// 一致：忽略 '_' 与 '-'、大小写不敏感。预筛分不清嵌套层级，metadata 专属
// 键名也算命中——宁多进一次慢路径，由 sanitizeValue 按作用域定夺。
func secretKeySpan(span []byte) bool {
	for _, name := range secretKeyNames {
		if equalFoldKey(span, name) {
			return true
		}
	}
	for _, name := range metadataSecretKeyNames {
		if equalFoldKey(span, name) {
			return true
		}
	}
	return false
}

// equalFoldKey 比较引号内键名原文与归一化名单项：跳过 '_' 与 '-'、大小写折叠。
func equalFoldKey(span []byte, name string) bool {
	i := 0
	for j := 0; j < len(name); j++ {
		for i < len(span) && (span[i] == '_' || span[i] == '-') {
			i++
		}
		if i >= len(span) {
			return false
		}
		c := span[i]
		i++
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != name[j] {
			return false
		}
	}
	for i < len(span) && (span[i] == '_' || span[i] == '-') {
		i++
	}
	return i == len(span)
}

// extractImage 识别「mime_type: image/* + data/base64*」形状的图片节点；
// 命中时返回携带 base64 正文的键名与写好的附件引用，由调用方原地替换该键。
func (recorder *Recorder) extractImage(value map[string]any) (string, attachmentReference, bool) {
	mimeType := stringField(value, "mime_type", "mimeType", "MIMEType")
	if !strings.HasPrefix(mimeType, "image/") {
		return "", attachmentReference{}, false
	}
	var encoded, dataKey string
	for _, key := range []string{"data", "base64_data", "base64Data", "Data"} {
		if text, ok := value[key].(string); ok && text != "" {
			encoded, dataKey = text, key
			break
		}
	}
	if encoded == "" {
		return "", attachmentReference{}, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", attachmentReference{}, false
	}
	return dataKey, recorder.writeAttachment(data, mimeType), true
}

func (recorder *Recorder) writeDataURL(value string) (attachmentReference, bool) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return attachmentReference{}, false
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return attachmentReference{}, false
	}
	return recorder.writeAttachment(data, mimeType), true
}

func (recorder *Recorder) writeAttachment(data []byte, mimeType string) attachmentReference {
	hashBytes := sha256.Sum256(data)
	hash := hex.EncodeToString(hashBytes[:])
	// 查重表只在编码协程上访问：同一 recorder 的任务恒由同一分片协程
	// 串行执行，map 无并发读写。
	if reference, ok := recorder.attachmentByHash[hash]; ok {
		return reference
	}
	recorder.attachmentCount++
	extension := imageExtension(mimeType)
	// name 是 debug_files 行键，形如 "attachments/image-001.png"——与
	// 01/02 JSON 体内的 file 指针及 /file/{name} 端点入参同形。
	name := fmt.Sprintf("%s/image-%03d%s", AttachmentsDir, recorder.attachmentCount, extension)
	reference := attachmentReference{File: name, MIMEType: mimeType, Size: len(data), SHA256: hash}
	recorder.attachmentByHash[hash] = reference
	// 附件 op 先于引用它的父文件 op 推进 insertQ（同一编码协程顺序
	// 推送），读侧不会在文件引用就绪时找不到附件行。预算满丢弃后
	// 父文件内的引用指向缺失行——与 workerGone 丢失同形态，读侧按
	// 「证据在压力下被裁」理解。
	stored, usize := recorder.encodePayload(data)
	n := int64(len(stored))
	if !recorder.manager.chargePayload(n, 0) {
		recorder.noteEncodeDrop(n)
		return reference
	}
	recorder.pushInsert(n, func() {
		recorder.stageFile(name, stagedFile{stored: stored, usize: usize})
	})
	return reference
}

func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		extensions, _ := mime.ExtensionsByType(mimeType)
		if len(extensions) > 0 {
			return extensions[0]
		}
		return ".bin"
	}
}

func stringField(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok {
			return text
		}
	}
	return ""
}
