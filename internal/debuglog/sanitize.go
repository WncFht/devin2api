// 本文件实现调试日志写盘值的脱敏与附件落盘——recorder.go 的净化器实现。
//
// sanitize/sanitizeValue 把投影产物归一成 any 树并遮盖敏感键；extractImage 族
// 把 data URL / base64 图片从树中摘出写入 attachments/ 子目录并留引用指针。脱敏规则
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
	"os"
	"path/filepath"
	"strings"
)

// sanitize 把待写值归一成 any 树后递归脱敏。map/slice/string 本身已是
// any 树节点（投影函数的产物），直接递归；json.RawMessage（03/04 的
// protojson 帧、06 的 SSE data）只需一次 unmarshal——此前先 marshal 回
// 字节再 unmarshal 是纯浪费。注意 map/slice 输入会被原地改写（secret 键
// 遮盖、图片提取），调用方传入的都是当次投影专用结构，原地改写是安全的。
func (recorder *Recorder) sanitize(value any) any {
	var generic any
	switch value := value.(type) {
	case json.Marshaler:
		// RawMessage 与惰性序列化包装（protoJSON 等）共用此路：序列化在
		// worker 内发生，调用方 goroutine 不承担 marshal 成本。快路径
		// 预筛不敏感即原样透传——记录多为自产 SSE 帧与 proto 投影，完整
		// unmarshal+树遍历+marshal 在每条 delta 上是纯开销。
		data, err := value.MarshalJSON()
		if err != nil {
			return map[string]any{"serialization_error": err.Error()}
		}
		if !rawNeedsSanitize(data) {
			return json.RawMessage(data)
		}
		if err := json.Unmarshal(data, &generic); err != nil {
			return map[string]any{"serialization_error": err.Error()}
		}
	case map[string]any, []any, string:
		generic = value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return map[string]any{"serialization_error": err.Error()}
		}
		if err := json.Unmarshal(data, &generic); err != nil {
			return map[string]any{"serialization_error": err.Error()}
		}
	}
	return recorder.sanitizeValue(generic, false)
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
		if reference, ok := recorder.extractImage(value); ok {
			return reference
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

func (recorder *Recorder) extractImage(value map[string]any) (attachmentReference, bool) {
	mimeType, _ := stringField(value, "mime_type", "mimeType", "MIMEType")
	encoded, _ := stringField(value, "data", "base64_data", "base64Data", "Data")
	if !strings.HasPrefix(mimeType, "image/") || encoded == "" {
		return attachmentReference{}, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return attachmentReference{}, false
	}
	return recorder.writeAttachment(data, mimeType), true
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
	if reference, ok := recorder.attachmentByHash[hash]; ok {
		return reference
	}
	recorder.attachmentCount++
	extension := imageExtension(mimeType)
	fileName := fmt.Sprintf("image-%03d%s", recorder.attachmentCount, extension)
	relativePath := filepath.Join(AttachmentsDir, fileName)
	directory := filepath.Join(recorder.directory, AttachmentsDir)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		recorder.noteIOErr("file", err)
	}
	if err := os.WriteFile(filepath.Join(directory, fileName), data, 0o600); err != nil {
		recorder.noteIOErr("file", err)
	}
	reference := attachmentReference{File: filepath.ToSlash(relativePath), MIMEType: mimeType, Size: len(data), SHA256: hash}
	recorder.attachmentByHash[hash] = reference
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

func stringField(value map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if text, ok := value[key].(string); ok {
			return text, true
		}
	}
	return "", false
}
