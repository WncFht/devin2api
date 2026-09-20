// 本文件是脱钩流完成缓存的持久化面：completed 条目在泵终局把缓冲的
// llm.ResponseEvent 序列编成一条 blob 落 detached_blobs，REUSEPORT
// 交接后新进程开机把 TTL 内的行灌回本 lane 注册表——同键重试命中即
// 秒回重放，不再付一次静默上游再生（15-25min 档）。crash-safe：写
// 发生在 finish 定态点，不依赖有序退出。
//
// 编解码要点（Content 是接口、Failure.Cause 是 error，裸 JSON 不可
// 往返）：
//   - 块表去重：Partial 是逐事件累计快照（Content 切片头克隆、块值
//     共享），逐事件直写会把已成形块按事件数平方放大——「大 thinking
//     块收口 + 数千条工具参数增量」恰是最贵请求类，朴素序列化在 8MiB
//     缓冲上可膨胀到数百 MB。块值经类型标记 JSON 进表，消息侧只存
//     下标，blob 回到 ~2× 缓冲量级；逐位置 DeepEqual 续用让编码成本
//     随块变更数而非事件数×块体增长（marshal 只在块值真变时付）。
//   - Failure.Cause（error 接口，链上可能有不可序列化类型）按字段
//     影子落盘——派生标志在产线已物化，重放侧 FailureOf 只读标志位，
//     丢 Cause 不损分类语义。
//   - running/failed 不落盘：running 的泵不可迁移（上游协议限制），
//     failed 缓冲按设计只回答「走新上游」，重放价值为零。
package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// detachedBlobVersion 是载荷格式版本：解码只认当前版，未来改编码
//
//	bump 之，旧行按不可解码跳过（播种降级为未命中，与缓存不存在同语义）。
const detachedBlobVersion = 2

// detachedBlob 是单条 completed 条目的持久化载荷：blocks 是内容块
// 去重表（类型标记 JSON），events 里 Partial/Message/Error 的
// Content 以块表下标引用。
type detachedBlob struct {
	Version int               `json:"v"`
	Blocks  []json.RawMessage `json:"blocks"`
	Events  []blobEvent       `json:"events"`
}

// blobBlock 是块表条目：Type 是判别标记，五个具体型各占一格。
type blobBlock struct {
	Type         llm.ContentType      `json:"t"`
	Text         *llm.TextContent     `json:"text,omitempty"`
	Thinking     *llm.ThinkingContent `json:"think,omitempty"`
	Image        *llm.ImageContent    `json:"img,omitempty"`
	ToolCall     *llm.ToolCall        `json:"call,omitempty"`
	ServerResult *blobServerResult    `json:"sresult,omitempty"`
}

// blobServerResult 是 ServerToolResult 的线格式：Content 是接口切片，
// 不能 JSON 往返——嵌套块复用同一套判别标记 inline 携带（结果正文
// 很小，不进块表去重）。
type blobServerResult struct {
	ToolCallID    string                `json:"tcid"`
	ToolName      string                `json:"tn,omitempty"`
	Content       []blobBlock           `json:"c,omitempty"`
	SearchResults []llm.WebSearchResult `json:"sr,omitempty"`
	IsError       bool                  `json:"e,omitempty"`
	ErrorCode     string                `json:"ec,omitempty"`
}

// blobEvent 是 ResponseEvent 的线格式：字段名取短键省体积（blob
// 只进本表，不迁就外部词表）；Partial/Message/Error 换影子消息。
type blobEvent struct {
	Type         llm.ResponseEventType `json:"t"`
	ContentIndex int                   `json:"ci,omitempty"`
	Delta        string                `json:"d,omitempty"`
	Content      string                `json:"c,omitempty"`
	Partial      *blobMessage          `json:"p,omitempty"`
	ToolCallID   string                `json:"tid,omitempty"`
	ToolName     string                `json:"tn,omitempty"`
	ToolCall     *llm.ToolCall         `json:"tc,omitempty"`
	ServerResult *blobServerResult     `json:"sr,omitempty"`
	Reason       llm.StopReason        `json:"r,omitempty"`
	Message      *blobMessage          `json:"m,omitempty"`
	Error        *blobMessage          `json:"e,omitempty"`
}

// blobMessage 是 AssistantMessage 的影子：Content 换块表下标列表，
// Failure 换无 Cause 的影子记录，其余标量原样。
type blobMessage struct {
	Content           []int                            `json:"c,omitempty"`
	API               string                           `json:"api,omitempty"`
	Provider          string                           `json:"pv,omitempty"`
	Model             string                           `json:"m,omitempty"`
	ResponseModel     string                           `json:"rm,omitempty"`
	ResponseID        string                           `json:"rid,omitempty"`
	OutputID          string                           `json:"oid,omitempty"`
	UpstreamRequestID string                           `json:"urid,omitempty"`
	Diagnostics       []llm.AssistantMessageDiagnostic `json:"diag,omitempty"`
	Usage             llm.Usage                        `json:"u,omitempty"`
	StopReason        llm.StopReason                   `json:"sr,omitempty"`
	StopSequence      string                           `json:"ss,omitempty"`
	ErrorMessage      string                           `json:"em,omitempty"`
	Failure           *blobFailure                     `json:"f,omitempty"`
	DebugRef          string                           `json:"dr,omitempty"`
	TimestampMS       int64                            `json:"ts,omitempty"`
}

// blobFailure 是 llm.Failure 的影子：Cause 不可序列化按字段落下——
// 生产侧 derive 已把派生标志物化进记录，重放侧分类不依赖 Cause。
type blobFailure struct {
	Code              string `json:"code,omitempty"`
	Message           string `json:"msg,omitempty"`
	LocalGate         bool   `json:"lg,omitempty"`
	UpstreamFault     bool   `json:"uf,omitempty"`
	RetryAfterSeconds int    `json:"ras,omitempty"`
	RetryAfterMinute  bool   `json:"ram,omitempty"`
	GateReason        string `json:"gr,omitempty"`
	GateProbeMS       int64  `json:"gpm,omitempty"`
	GateSiblingEwMS   int64  `json:"gse,omitempty"`
	ContextLength     bool   `json:"cl,omitempty"`
	RateLimited       bool   `json:"rl,omitempty"`
	Canceled          bool   `json:"cx,omitempty"`
	Timeout           bool   `json:"to,omitempty"`
	ClientFixable     bool   `json:"cf,omitempty"`
	TraceID           string `json:"tid,omitempty"`
	ResetHint         bool   `json:"rh,omitempty"`
}

// blobEncoder 累积一条 blob 的编码态：块表 + 值去重索引 + 上一事件
// Partial 的逐位置续用快照。
type blobEncoder struct {
	blocks   []json.RawMessage
	blockIDs map[string]int
	// marshals 记块 marshal 次数——续用命中成本界（随块变更数而非
	// 事件数增长）的可观测面，测试断言用。
	marshals int
	// prevContent/prevIdx 是上一个编码事件 Partial.Content 的源块值与
	// 块表下标：逐位置 DeepEqual 命中即续用下标，只在块值真变时付
	// marshal（Delta 洪流里在产块恒小、成形块恒同值，成本压回线性）。
	prevContent []llm.Content
	prevIdx     []int
}

// encodeDetachedEvents 把条目缓冲事件序列编成 blob 载荷。
func encodeDetachedEvents(events []llm.ResponseEvent) ([]byte, error) {
	enc := &blobEncoder{blockIDs: make(map[string]int)}
	blob := detachedBlob{Version: detachedBlobVersion}
	for _, event := range events {
		out, err := enc.event(event)
		if err != nil {
			return nil, err
		}
		blob.Events = append(blob.Events, out)
	}
	blob.Blocks = enc.blocks
	return json.Marshal(blob)
}

// event 编码单条事件：Partial 走逐位置续用的块表引用；
// Message/Error 是一次性终态指针，直接编码不续用。
func (enc *blobEncoder) event(event llm.ResponseEvent) (blobEvent, error) {
	out := blobEvent{
		Type:         event.Type,
		ContentIndex: event.ContentIndex,
		Delta:        event.Delta,
		Content:      event.Content,
		ToolCallID:   event.ToolCallID,
		ToolName:     event.ToolName,
		ToolCall:     event.ToolCall,
		Reason:       event.Reason,
	}
	var err error
	if out.ServerResult, err = encodeBlobServerResult(event.ServerResult); err != nil {
		return out, err
	}
	if event.Partial != nil {
		if out.Partial, err = enc.partial(event.Partial); err != nil {
			return out, err
		}
	} else {
		// Partial 缺席即续用基线作废：下一事件的逐位置比较不得
		// 跨过这条事件引用更早的块值。
		enc.prevContent, enc.prevIdx = nil, nil
	}
	if out.Message, err = enc.message(event.Message); err != nil {
		return out, err
	}
	if out.Error, err = enc.message(event.Error); err != nil {
		return out, err
	}
	return out, nil
}

// partial 编码逐事件累计快照：Content 逐位置与上一事件同位块值
// DeepEqual——命中续用旧下标免 marshal，未命中（含块 append 出的
// 新位置）marshal 进表。源块值随快照更新供下一事件比较。
func (enc *blobEncoder) partial(message *llm.AssistantMessage) (*blobMessage, error) {
	idx := make([]int, len(message.Content))
	for i, block := range message.Content {
		if i < len(enc.prevIdx) && i < len(enc.prevContent) && sameContentBlock(enc.prevContent[i], block) {
			idx[i] = enc.prevIdx[i]
			continue
		}
		id, err := enc.blockIndex(block)
		if err != nil {
			return nil, err
		}
		idx[i] = id
	}
	enc.prevContent, enc.prevIdx = message.Content, idx
	out, err := enc.messageBase(message)
	if err != nil {
		return nil, err
	}
	out.Content = idx
	return out, nil
}

// sameContentBlock 判定相邻事件同位块值未变：逐流事件的热路径是
// text/thinking 增量，字段全标量的块型直接 ==（编译期字段比较，比
// DeepEqual 的反射派发快一两个量级）；带切片的 ToolCall 逐字段比
// 加 bytes.Equal，含嵌套 Content 的 ServerToolResult 等罕见块型落回
// DeepEqual——它们不在增量流的高频形态里。
func sameContentBlock(prev, block llm.Content) bool {
	switch typed := prev.(type) {
	case llm.TextContent:
		other, ok := block.(llm.TextContent)
		return ok && typed == other
	case llm.ThinkingContent:
		other, ok := block.(llm.ThinkingContent)
		return ok && typed == other
	case llm.ImageContent:
		other, ok := block.(llm.ImageContent)
		return ok && typed == other
	case llm.DocumentContent:
		other, ok := block.(llm.DocumentContent)
		return ok && typed == other
	case llm.VideoContent:
		other, ok := block.(llm.VideoContent)
		return ok && typed == other
	case llm.ToolCall:
		other, ok := block.(llm.ToolCall)
		return ok && typed.ID == other.ID && typed.Name == other.Name &&
			typed.Custom == other.Custom && typed.Server == other.Server &&
			typed.Signature == other.Signature && typed.SignatureType == other.SignatureType &&
			bytes.Equal(typed.Arguments, other.Arguments)
	default:
		return reflect.DeepEqual(prev, block)
	}
}

// message 编码一次性终态消息：标量基板加 Content 逐块进表——终态
// 无续用对象，逐块直进是正确成本。
func (enc *blobEncoder) message(message *llm.AssistantMessage) (*blobMessage, error) {
	out, err := enc.messageBase(message)
	if err != nil || out == nil {
		return out, err
	}
	for _, block := range message.Content {
		id, err := enc.blockIndex(block)
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, id)
	}
	return out, nil
}

// messageBase 编码消息标量面与 Failure 影子，Content 留给调用方：
// partial 以续用下标回填，终态消息由 message 逐块进表。
func (enc *blobEncoder) messageBase(message *llm.AssistantMessage) (*blobMessage, error) {
	if message == nil {
		return nil, nil
	}
	out := &blobMessage{
		API:               message.API,
		Provider:          message.Provider,
		Model:             message.Model,
		ResponseModel:     message.ResponseModel,
		ResponseID:        message.ResponseID,
		OutputID:          message.OutputID,
		UpstreamRequestID: message.UpstreamRequestID,
		Diagnostics:       message.Diagnostics,
		Usage:             message.Usage,
		StopReason:        message.StopReason,
		StopSequence:      message.StopSequence,
		ErrorMessage:      message.ErrorMessage,
		DebugRef:          message.DebugRef,
		TimestampMS:       message.TimestampMS,
	}
	if f := message.Failure; f != nil {
		out.Failure = &blobFailure{
			Code: f.Code, Message: f.Message, LocalGate: f.LocalGate,
			UpstreamFault: f.UpstreamFault, RetryAfterSeconds: f.RetryAfterSeconds,
			RetryAfterMinute: f.RetryAfterMinute, GateReason: f.GateReason,
			GateProbeMS: f.GateProbeMS, GateSiblingEwMS: f.GateSiblingEwMS,
			ContextLength: f.ContextLength, RateLimited: f.RateLimited,
			Canceled: f.Canceled, Timeout: f.Timeout, ClientFixable: f.ClientFixable,
			TraceID: f.TraceID, ResetHint: f.ResetHint,
		}
	}
	return out, nil
}

// tagBlock 把内容块装入判别标记外壳：块表去重与 ServerToolResult
// 正文嵌套块共用同一套装配。
func tagBlock(block llm.Content) (blobBlock, error) {
	tagged := blobBlock{Type: block.ContentType()}
	switch content := block.(type) {
	case llm.TextContent:
		tagged.Text = &content
	case llm.ThinkingContent:
		tagged.Thinking = &content
	case llm.ImageContent:
		tagged.Image = &content
	case llm.ToolCall:
		tagged.ToolCall = &content
	case llm.ServerToolResult:
		result, err := encodeBlobServerResult(&content)
		if err != nil {
			return tagged, err
		}
		tagged.ServerResult = result
	default:
		return tagged, fmt.Errorf("detached blob: unknown content block %T", block)
	}
	return tagged, nil
}

// encodeBlobServerResult 把托管工具结果转成线格式影子，正文块逐块
// 打判别标记。
func encodeBlobServerResult(result *llm.ServerToolResult) (*blobServerResult, error) {
	if result == nil {
		return nil, nil
	}
	out := &blobServerResult{
		ToolCallID: result.ToolCallID, ToolName: result.ToolName,
		SearchResults: result.SearchResults, IsError: result.IsError, ErrorCode: result.ErrorCode,
	}
	for _, block := range result.Content {
		tagged, err := tagBlock(block)
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, tagged)
	}
	return out, nil
}

// decode 把线格式影子解回 ServerToolResult；嵌套正文块逐块解标记。
func (result *blobServerResult) decode() (*llm.ServerToolResult, error) {
	if result == nil {
		return nil, nil
	}
	out := &llm.ServerToolResult{
		ToolCallID: result.ToolCallID, ToolName: result.ToolName,
		SearchResults: result.SearchResults, IsError: result.IsError, ErrorCode: result.ErrorCode,
	}
	for _, tagged := range result.Content {
		block, err := tagged.decode()
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, block)
	}
	return out, nil
}

// blockIndex 把一个内容块编进块表：值相同（类型标记 JSON 字节相同）
// 的块去重到同一下标。
func (enc *blobEncoder) blockIndex(block llm.Content) (int, error) {
	tagged, err := tagBlock(block)
	if err != nil {
		return 0, err
	}
	enc.marshals++
	data, err := json.Marshal(tagged)
	if err != nil {
		return 0, err
	}
	if id, ok := enc.blockIDs[string(data)]; ok {
		return id, nil
	}
	id := len(enc.blocks)
	enc.blocks = append(enc.blocks, data)
	enc.blockIDs[string(data)] = id
	return id, nil
}

// decodeDetachedEvents 把 blob 载荷解回事件序列：先解块表再逐事件
// 复原——Partial/Message/Error 的块下标换回具体块值（块是不可变
// 值类型，跨消息共享同值与 snapshot 的克隆语义等价）。
func decodeDetachedEvents(payload []byte) ([]llm.ResponseEvent, error) {
	var blob detachedBlob
	if err := json.Unmarshal(payload, &blob); err != nil {
		return nil, err
	}
	if blob.Version != detachedBlobVersion {
		return nil, fmt.Errorf("detached blob version %d unsupported", blob.Version)
	}
	blocks := make([]llm.Content, len(blob.Blocks))
	for i, raw := range blob.Blocks {
		var tagged blobBlock
		if err := json.Unmarshal(raw, &tagged); err != nil {
			return nil, err
		}
		block, err := tagged.decode()
		if err != nil {
			return nil, err
		}
		blocks[i] = block
	}
	events := make([]llm.ResponseEvent, 0, len(blob.Events))
	for _, in := range blob.Events {
		event := llm.ResponseEvent{
			Type: in.Type, ContentIndex: in.ContentIndex, Delta: in.Delta,
			Content: in.Content, ToolCallID: in.ToolCallID, ToolName: in.ToolName,
			ToolCall: in.ToolCall, Reason: in.Reason,
		}
		var err error
		if event.ServerResult, err = in.ServerResult.decode(); err != nil {
			return nil, err
		}
		if event.Partial, err = in.Partial.decode(blocks); err != nil {
			return nil, err
		}
		if event.Message, err = in.Message.decode(blocks); err != nil {
			return nil, err
		}
		if event.Error, err = in.Error.decode(blocks); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// decode 解出一条块表引用列表：下标越界即载荷损坏（编码侧只写合法
// 下标，越界不猜不补）。
func (m *blobMessage) decode(blocks []llm.Content) (*llm.AssistantMessage, error) {
	if m == nil {
		return nil, nil
	}
	out := &llm.AssistantMessage{
		API: m.API, Provider: m.Provider, Model: m.Model,
		ResponseModel: m.ResponseModel, ResponseID: m.ResponseID,
		OutputID: m.OutputID, UpstreamRequestID: m.UpstreamRequestID,
		Diagnostics: m.Diagnostics, Usage: m.Usage, StopReason: m.StopReason,
		StopSequence: m.StopSequence, ErrorMessage: m.ErrorMessage,
		DebugRef: m.DebugRef, TimestampMS: m.TimestampMS,
	}
	for _, id := range m.Content {
		if id < 0 || id >= len(blocks) {
			return nil, fmt.Errorf("detached blob: block index %d out of range", id)
		}
		out.Content = append(out.Content, blocks[id])
	}
	if f := m.Failure; f != nil {
		out.Failure = &llm.Failure{
			Code: f.Code, Message: f.Message, LocalGate: f.LocalGate,
			UpstreamFault: f.UpstreamFault, RetryAfterSeconds: f.RetryAfterSeconds,
			RetryAfterMinute: f.RetryAfterMinute, GateReason: f.GateReason,
			GateProbeMS: f.GateProbeMS, GateSiblingEwMS: f.GateSiblingEwMS,
			ContextLength: f.ContextLength, RateLimited: f.RateLimited,
			Canceled: f.Canceled, Timeout: f.Timeout, ClientFixable: f.ClientFixable,
			TraceID: f.TraceID, ResetHint: f.ResetHint,
		}
	}
	return out, nil
}

// decode 取回判别标记对应的具体块值；标记与载荷槽不符即损坏。
func (tagged blobBlock) decode() (llm.Content, error) {
	switch tagged.Type {
	case llm.ContentTypeText:
		if tagged.Text != nil {
			return *tagged.Text, nil
		}
	case llm.ContentTypeThinking:
		if tagged.Thinking != nil {
			return *tagged.Thinking, nil
		}
	case llm.ContentTypeImage:
		if tagged.Image != nil {
			return *tagged.Image, nil
		}
	case llm.ContentTypeToolCall:
		if tagged.ToolCall != nil {
			return *tagged.ToolCall, nil
		}
	case llm.ContentTypeServerToolResult:
		if tagged.ServerResult != nil {
			result, err := tagged.ServerResult.decode()
			if err != nil {
				return nil, err
			}
			return *result, nil
		}
	}
	return nil, fmt.Errorf("detached blob: malformed block type %q", tagged.Type)
}

// persistBlob 把一条 completed 条目的缓冲事件编库：写入以
// lockedStateStoreTimeout 为上限与台账同口径，失败计 blobDrops——
// 种子层自身的丢失量必须可数，与台账 ledgerDrops 分开记账。
// 编码成本在泵 goroutine 付，不占 registry.mu。
func (registry *detachedRegistry) persistBlob(key string, events []llm.ResponseEvent, originDir string) {
	if registry.ledger == nil {
		return
	}
	payload, err := encodeDetachedEvents(events)
	if err != nil {
		registry.noteBlobDrop()
		slog.Warn("detached blob encode failed", "lane", registry.lane, "key", detachedRingKey(key), "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	err = registry.ledger.InsertDetachedBlob(ctx, store.DetachedBlob{
		Key: key, Lane: registry.lane, OriginDir: originDir,
		FinishedAt: time.Now(), Payload: payload,
	})
	cancel()
	if err != nil {
		registry.noteBlobDrop()
		slog.Warn("detached blob write failed", "lane", registry.lane, "key", detachedRingKey(key), "error", err)
	}
}

// seed 把上一进程留下的 completed 条目载荷灌回注册表：行按 lane
// 过滤（注册表是逐 lane 内存结构，跨 lane 挂接在进程内本就不存在——
// peek 只记账不服务，播种不扩大服务面）；条目按 finished_at+TTL
// 拿剩余窗口，过期行一行不灌。台账缺席或读失败时降级为空缓存，
// 与「持久层从未存在」同语义——不拦启动。调用方是 New() 的构造点，
// 注册表此刻无并发。
func (registry *detachedRegistry) seed() {
	if registry == nil || registry.ledger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	blobs, err := registry.ledger.LoadDetachedBlobs(ctx, registry.lane, time.Now().Add(-detachedCompletedTTL).UnixMilli())
	cancel()
	if err != nil {
		slog.Warn("detached blob seed load failed; starting with empty cache", "lane", registry.lane, "error", err)
		return
	}
	// 只灌最新的 detachedMaxEntries 条：缓存容量帽在 admit 侧有让位序，
	// 播种全灌会让内存驻留越帽；行按 finished_at 升序返回，截尾留新。
	if len(blobs) > detachedMaxEntries {
		slog.Info("detached seed rows exceed cache capacity; keeping newest", "lane", registry.lane, "rows", len(blobs), "cap", detachedMaxEntries)
		blobs = blobs[len(blobs)-detachedMaxEntries:]
	}
	now := time.Now()
	for _, blob := range blobs {
		expiresAt := blob.FinishedAt.Add(detachedCompletedTTL)
		if !now.Before(expiresAt) {
			continue
		}
		events, err := decodeDetachedEvents(blob.Payload)
		if err != nil {
			registry.noteBlobDrop()
			slog.Warn("detached blob decode failed; skipping seed row", "lane", registry.lane, "key", detachedRingKey(blob.Key), "error", err)
			continue
		}
		entry := &detachedEntry{
			events:     events,
			state:      detachedCompleted,
			replayable: true,
			originDir:  blob.OriginDir,
			// admittedAt 记真实完成时刻而非开机时刻：容量让位序按条目
			// 年龄排，播种条目不该凭「刚到」挤掉谁。detachIndex 记满长
			// ——落盘前的 pre/post-detach 分界已不可考，孤儿浪费口径取
			// 保守下偏（0）而非虚报。
			admittedAt:  blob.FinishedAt,
			expiresAt:   expiresAt,
			detachIndex: len(events),
		}
		registry.mu.Lock()
		registry.entries[blob.Key] = entry
		registry.Seeded++
		registry.pushEvent(detachedEventSeed, blob.Key, blob.OriginDir, "")
		registry.mu.Unlock()
	}
}
