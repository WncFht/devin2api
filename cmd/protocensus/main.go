// 本文件实现协议普查与漂移检测：census 子命令用生成代码注册的描述符统计
// logs/ 里真实流量出现的字段、枚举值和未知键；diff 子命令对比两次提取的
// FileDescriptorSet，报告上游协议的新增/删除/变更。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "local/devinproto" // 注册上游描述符到全局 registry，供 census 按线网名解析

	"github.com/WncFht/devin2api/internal/debuglog"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// requestTypeName 与 responseTypeName 是代理实际调用的上游 RPC 的线网类型。
const (
	requestTypeName  = "exa.api_server_pb.GetChatMessageRequest"
	responseTypeName = "exa.api_server_pb.GetChatMessageResponse"
)

// msgCensus 累计单个消息类型在流量中的出现次数与字段命中数。
type msgCensus struct {
	Occurrences int            `json:"occurrences"`
	Fields      map[string]int `json:"fields"`
}

// unknownKey 记录 JSON 里无法被描述符解析的键。
type unknownKey struct {
	Message  string   `json:"message"`
	Key      string   `json:"key"`
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

// enumAnomaly 记录枚举字段出现的描述符之外的取值（数字值或未知名字），
// 是上游新增枚举成员的漂移信号。
type enumAnomaly struct {
	Message  string   `json:"message"`
	Field    string   `json:"field"`
	Value    string   `json:"value"`
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

// census 是一侧流量（请求或响应）的累计统计。
type census struct {
	Messages   map[string]*msgCensus
	unknown    map[string]*unknownKey
	enumAnom   map[string]*enumAnomaly
	currentDir string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "census":
		err = cmdCensus(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `subcommands:
  census [-logs DIR] [-max-dirs N]
                        scan request dirs, report field coverage, unknown keys,
                        enum anomalies (default logs dir: ./logs)
  diff OLD.pb NEW.pb    compare two FileDescriptorSets (descriptors.pb),
                        report added/removed/changed symbols`)
}

// messageDesc 从全局 registry 解析消息描述符；生成代码 import 后即已注册。
func messageDesc(name protoreflect.FullName) (protoreflect.MessageDescriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w (run task generate first)", name, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a message", name)
	}
	return md, nil
}

// ---- census ----

func cmdCensus(args []string) error {
	fs := flag.NewFlagSet("census", flag.ContinueOnError)
	logsDir := fs.String("logs", "logs", "debuglog directory")
	maxDirs := fs.Int("max-dirs", 0, "only scan newest N request dirs (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reqMD, err := messageDesc(requestTypeName)
	if err != nil {
		return err
	}
	respMD, err := messageDesc(responseTypeName)
	if err != nil {
		return err
	}
	dirs, err := requestDirs(*logsDir, *maxDirs)
	if err != nil {
		return err
	}
	req, resp := newCensus(), newCensus()
	var frames int
	for _, dir := range dirs {
		req.currentDir, resp.currentDir = dir, dir
		// 首个请求与 attemptN 重试分片都进普查——重试写给上游的 wire
		// 形态不同（如换 model/追加 continue），漏掉会低估字段覆盖。
		if requestStages, err := debuglog.DevinRequestStages(filepath.Join(*logsDir, dir)); err == nil {
			for _, stage := range requestStages {
				if raw, err := os.ReadFile(filepath.Join(*logsDir, dir, stage)); err == nil {
					var obj map[string]any
					if json.Unmarshal(raw, &obj) == nil {
						req.walk(reqMD, obj)
					}
				}
			}
		}
		if f, err := os.Open(filepath.Join(*logsDir, dir, debuglog.StageDevinResponse)); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 4<<20), 4<<20)
			for sc.Scan() {
				var obj map[string]any
				if json.Unmarshal(sc.Bytes(), &obj) == nil {
					frames++
					resp.walk(respMD, obj)
				}
			}
			// 单行超过 4MB 缓冲时 Scan 提前终止——不查 Err 会把截断
			// 当成正常读完，普查少计而不自知。
			if err := sc.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan %s/%s: %v\n", dir, debuglog.StageDevinResponse, err)
			}
			_ = f.Close()
		}
	}
	return printReport(len(dirs), frames, req, resp)
}

// requestDirs 返回按名字倒序的请求目录名（debuglog 目录名按时间排序，名字即时间序）。
func requestDirs(logsDir string, maxDirs int) ([]string, error) {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	if maxDirs > 0 && len(dirs) > maxDirs {
		dirs = dirs[:maxDirs]
	}
	return dirs, nil
}

func newCensus() *census {
	return &census{
		Messages: map[string]*msgCensus{},
		unknown:  map[string]*unknownKey{},
		enumAnom: map[string]*enumAnomaly{},
	}
}

// walk 把一个 JSON 对象按消息描述符逐键解析：已解字段计数并递归子消息，
// 未解字段记为 unknown key，枚举取值校验成员表。
func (c *census) walk(md protoreflect.MessageDescriptor, obj map[string]any) {
	typeName := string(md.FullName())
	ms := c.Messages[typeName]
	if ms == nil {
		ms = &msgCensus{Fields: map[string]int{}}
		c.Messages[typeName] = ms
	}
	ms.Occurrences++
	for key, val := range obj {
		fd := md.Fields().ByJSONName(key)
		if fd == nil {
			fd = md.Fields().ByName(protoreflect.Name(key))
		}
		if fd == nil {
			c.recordUnknown(typeName, key)
			continue
		}
		ms.Fields[string(fd.Name())]++
		c.walkValue(fd, val)
	}
}

func (c *census) recordUnknown(typeName, key string) {
	id := typeName + "|" + key
	u := c.unknown[id]
	if u == nil {
		u = &unknownKey{Message: typeName, Key: key}
		c.unknown[id] = u
	}
	u.Count++
	u.Examples = appendExample(u.Examples, c.currentDir)
}

// walkValue 按字段基数与类型分发：repeated 逐元素、map 逐值、消息递归、枚举校验。
func (c *census) walkValue(fd protoreflect.FieldDescriptor, val any) {
	if fd.IsMap() {
		obj, ok := val.(map[string]any)
		if !ok {
			return
		}
		vfd := fd.MapValue()
		for _, v := range obj {
			c.walkSingle(vfd, v)
		}
		return
	}
	if fd.IsList() {
		if arr, ok := val.([]any); ok {
			for _, item := range arr {
				c.walkSingle(fd, item)
			}
		}
		return
	}
	c.walkSingle(fd, val)
}

func (c *census) walkSingle(fd protoreflect.FieldDescriptor, val any) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if sub, ok := val.(map[string]any); ok {
			c.walk(fd.Message(), sub)
		}
	case protoreflect.EnumKind:
		c.checkEnum(fd, val)
	}
}

// checkEnum 校验枚举取值：protojson 对未知成员输出数字，对已知成员输出名字。
func (c *census) checkEnum(fd protoreflect.FieldDescriptor, val any) {
	var bad string
	switch v := val.(type) {
	case string:
		if fd.Enum().Values().ByName(protoreflect.Name(v)) == nil {
			bad = v
		}
	case float64:
		if fd.Enum().Values().ByNumber(protoreflect.EnumNumber(v)) == nil {
			bad = fmt.Sprintf("number:%v", v)
		}
	}
	if bad == "" {
		return
	}
	id := string(fd.Enum().FullName()) + "|" + string(fd.Name()) + "|" + bad
	a := c.enumAnom[id]
	if a == nil {
		a = &enumAnomaly{Message: string(fd.Enum().FullName()), Field: string(fd.Name()), Value: bad}
		c.enumAnom[id] = a
	}
	a.Count++
	a.Examples = appendExample(a.Examples, c.currentDir)
}

func appendExample(ex []string, dir string) []string {
	if dir == "" || len(ex) >= 3 {
		return ex
	}
	for _, e := range ex {
		if e == dir {
			return ex
		}
	}
	return append(ex, dir)
}

// neverSeen 汇总已观测消息类型中零命中的字段，作为"schema 有而流量没有"的差集。
func (c *census) neverSeen() []string {
	out := []string{}
	for typeName, ms := range c.Messages {
		if ms.Occurrences == 0 {
			continue
		}
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(typeName))
		if err != nil {
			continue
		}
		md, ok := d.(protoreflect.MessageDescriptor)
		if !ok {
			continue
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			name := string(fields.Get(i).Name())
			if ms.Fields[name] == 0 {
				out = append(out, typeName+"."+name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func printReport(dirs, frames int, req, resp *census) error {
	report := map[string]any{
		"dirs_scanned":    dirs,
		"response_frames": frames,
		"request":         censusSection(req),
		"response":        censusSection(resp),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func censusSection(c *census) map[string]any {
	unknown := []unknownKey{}
	for _, u := range c.unknown {
		unknown = append(unknown, *u)
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Count > unknown[j].Count })
	anoms := []enumAnomaly{}
	for _, a := range c.enumAnom {
		anoms = append(anoms, *a)
	}
	sort.Slice(anoms, func(i, j int) bool { return anoms[i].Count > anoms[j].Count })
	return map[string]any{
		"messages":          c.Messages,
		"fields_never_seen": c.neverSeen(),
		"unknown_keys":      unknown,
		"enum_anomalies":    anoms,
	}
}

// ---- diff ----

// join 按 proto 规则用点拼接包名与符号名。
func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// symbolTable 把 FileDescriptorSet 展平成可比较的符号映射。
type symbolTable struct {
	fields  map[string]string // "Msg.field" -> "number=3 type=TYPE_STRING label=REPEATED"
	enums   map[string]int32  // "Enum.VALUE" -> number
	methods map[string]string // "Svc.Method" -> "Req -> Resp (cs ss)"
	types   map[string]bool   // 消息/枚举/服务全限定名集合
}

func cmdDiff(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: diff OLD.pb NEW.pb")
	}
	oldT, err := loadSymbols(args[0])
	if err != nil {
		return err
	}
	newT, err := loadSymbols(args[1])
	if err != nil {
		return err
	}
	report := map[string]any{
		"added":   diffAdded(oldT, newT),
		"removed": diffAdded(newT, oldT),
		"changed": diffChanged(oldT, newT),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// loadSymbols 解析描述符集并展平成符号表；逐文件独立处理，缺依赖不影响。
func loadSymbols(path string) (*symbolTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	t := &symbolTable{
		fields:  map[string]string{},
		enums:   map[string]int32{},
		methods: map[string]string{},
		types:   map[string]bool{},
	}
	for _, f := range set.GetFile() {
		pkg := f.GetPackage()
		for _, m := range f.GetMessageType() {
			t.indexMessage(pkg, m)
		}
		for _, e := range f.GetEnumType() {
			t.indexEnum(join(pkg, e.GetName()), e)
		}
		for _, s := range f.GetService() {
			svc := join(pkg, s.GetName())
			t.types[svc] = true
			for _, m := range s.GetMethod() {
				sig := fmt.Sprintf("%s -> %s cs=%v ss=%v",
					strings.TrimPrefix(m.GetInputType(), "."), strings.TrimPrefix(m.GetOutputType(), "."),
					m.GetClientStreaming(), m.GetServerStreaming())
				t.methods[svc+"."+m.GetName()] = sig
			}
		}
	}
	return t, nil
}

func (t *symbolTable) indexMessage(prefix string, m *descriptorpb.DescriptorProto) {
	fqn := join(prefix, m.GetName())
	t.types[fqn] = true
	for _, fd := range m.GetField() {
		t.fields[fqn+"."+fd.GetName()] = fmt.Sprintf("number=%d type=%s type_name=%s label=%s",
			fd.GetNumber(), fd.GetType(), fd.GetTypeName(), fd.GetLabel())
	}
	for _, n := range m.GetNestedType() {
		t.indexMessage(fqn, n)
	}
	for _, e := range m.GetEnumType() {
		t.indexEnum(join(fqn, e.GetName()), e)
	}
}

func (t *symbolTable) indexEnum(fqn string, e *descriptorpb.EnumDescriptorProto) {
	t.types[fqn] = true
	for _, v := range e.GetValue() {
		t.enums[fqn+"."+v.GetName()] = v.GetNumber()
	}
}

// diffAdded 返回在 newT 存在而 oldT 不存在的符号（type/field/enum/rpc）。
func diffAdded(oldT, newT *symbolTable) []string {
	out := []string{}
	for name := range newT.types {
		if !oldT.types[name] {
			out = append(out, "type "+name)
		}
	}
	for name := range newT.fields {
		if _, ok := oldT.fields[name]; !ok {
			out = append(out, "field "+name)
		}
	}
	for name := range newT.enums {
		if _, ok := oldT.enums[name]; !ok {
			out = append(out, "enum "+name)
		}
	}
	for name := range newT.methods {
		if _, ok := oldT.methods[name]; !ok {
			out = append(out, "rpc "+name)
		}
	}
	sort.Strings(out)
	return out
}

// diffChanged 返回两边都存在但签名不同的符号。
func diffChanged(oldT, newT *symbolTable) []string {
	out := []string{}
	for name, newSig := range newT.fields {
		if oldSig, ok := oldT.fields[name]; ok && oldSig != newSig {
			out = append(out, fmt.Sprintf("field %s: %s -> %s", name, oldSig, newSig))
		}
	}
	for name, newNum := range newT.enums {
		if oldNum, ok := oldT.enums[name]; ok && oldNum != newNum {
			out = append(out, fmt.Sprintf("enum %s: %d -> %d", name, oldNum, newNum))
		}
	}
	for name, newSig := range newT.methods {
		if oldSig, ok := oldT.methods[name]; ok && oldSig != newSig {
			out = append(out, fmt.Sprintf("rpc %s: %s -> %s", name, oldSig, newSig))
		}
	}
	sort.Strings(out)
	return out
}
