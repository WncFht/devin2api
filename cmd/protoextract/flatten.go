// 本文件负责将恢复的 protobuf 描述符展平为单个可编译的 proto 文件。
package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const preferredRootPackage = "exa.api_server_pb"

type symbolMapping struct {
	Kind      string `json:"kind"`
	Original  string `json:"original"`
	Flattened string `json:"flattened"`
}

type flattenMetadata struct {
	Package        string          `json:"package"`
	Syntax         string          `json:"syntax"`
	SymbolMappings []symbolMapping `json:"symbol_mappings"`
	Warnings       []string        `json:"warnings"`
}

type flattenState struct {
	rootPackage    string
	usedTopLevel   map[string]struct{}
	typeNames      map[string]string
	serviceNames   map[string]string
	extensionNames map[string]string
	enumValues     map[string]map[string]string
	mappings       []symbolMapping
}

// flattenDescriptors 把多文件描述符集合并成一个 proto2 单文件：根包
// 符号保留原名，非根包符号加包前缀重命名，类型引用逐处改写，源码注释
// 按新声明位次重映射；返回展平文件与重命名元数据（供 manifest 上报）。
func flattenDescriptors(files []*descriptorpb.FileDescriptorProto, preferredPackage string) (*descriptorpb.FileDescriptorProto, flattenMetadata, error) {
	rootPackage := chooseRootPackage(files, preferredPackage)
	state := &flattenState{
		rootPackage:    rootPackage,
		usedTopLevel:   make(map[string]struct{}),
		typeNames:      make(map[string]string),
		serviceNames:   make(map[string]string),
		extensionNames: make(map[string]string),
		enumValues:     make(map[string]map[string]string),
	}

	ordered := append([]*descriptorpb.FileDescriptorProto(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GetName() < ordered[j].GetName() })
	// Root-package declarations reserve their original names before imported
	// packages are assigned prefixed names.
	for _, rootPass := range []bool{true, false} {
		for _, file := range ordered {
			if (file.GetPackage() == rootPackage) != rootPass {
				continue
			}
			state.registerFileSymbols(file)
		}
	}

	flat := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(defaultBundleName),
		Package: proto.String(rootPackage),
		Syntax:  proto.String("proto2"),
	}
	var headerComments []string
	for _, file := range ordered {
		messageOffset := len(flat.MessageType)
		enumOffset := len(flat.EnumType)
		serviceOffset := len(flat.Service)
		extensionOffset := len(flat.Extension)

		for _, message := range file.GetMessageType() {
			original := qualify(file.GetPackage(), message.GetName())
			cloned := proto.Clone(message).(*descriptorpb.DescriptorProto)
			if err := state.rewriteMessage(file, original, cloned); err != nil {
				return nil, flattenMetadata{}, err
			}
			cloned.Name = proto.String(shortName(state.typeNames[original]))
			flat.MessageType = append(flat.MessageType, cloned)
		}
		for _, enum := range file.GetEnumType() {
			original := qualify(file.GetPackage(), enum.GetName())
			cloned := proto.Clone(enum).(*descriptorpb.EnumDescriptorProto)
			state.rewriteEnum(original, cloned)
			cloned.Name = proto.String(shortName(state.typeNames[original]))
			flat.EnumType = append(flat.EnumType, cloned)
		}
		for _, service := range file.GetService() {
			original := qualify(file.GetPackage(), service.GetName())
			cloned := proto.Clone(service).(*descriptorpb.ServiceDescriptorProto)
			state.rewriteService(cloned)
			cloned.Name = proto.String(shortName(state.serviceNames[original]))
			flat.Service = append(flat.Service, cloned)
		}
		for _, extension := range file.GetExtension() {
			original := qualify(file.GetPackage(), extension.GetName())
			cloned := proto.Clone(extension).(*descriptorpb.FieldDescriptorProto)
			state.rewriteField(file, cloned)
			cloned.Name = proto.String(shortName(state.extensionNames[original]))
			flat.Extension = append(flat.Extension, cloned)
		}

		mapped, unmapped := remapSourceInfo(file, messageOffset, enumOffset, serviceOffset, extensionOffset)
		mapped, removed := filterSourceInfo(flat, file.GetName(), mapped)
		flat.SourceCodeInfo = mergeSourceInfo(flat.GetSourceCodeInfo(), mapped)
		headerComments = append(headerComments, unmapped...)
		headerComments = append(headerComments, removed...)
	}
	if len(headerComments) > 0 {
		if flat.SourceCodeInfo == nil {
			flat.SourceCodeInfo = &descriptorpb.SourceCodeInfo{}
		}
		flat.SourceCodeInfo.Location = append(flat.SourceCodeInfo.Location, &descriptorpb.SourceCodeInfo_Location{
			Path:            []int32{},
			Span:            []int32{0, 0, 0},
			LeadingComments: proto.String(strings.Join(headerComments, "\n")),
		})
	}

	metadata := flattenMetadata{
		Package:        rootPackage,
		Syntax:         "proto2",
		SymbolMappings: state.mappings,
		Warnings: []string{
			"descriptors.pb is the lossless authority for original file names, packages, options, and syntax",
			"non-root package symbols are renamed and non-root service RPC paths therefore change",
			"proto3 declarations use proto2 optional syntax in the flattened view; wire encoding is preserved but generated presence and open-enum APIs may differ",
			"custom declaration options are removed from the flattened source so all definitions can compile without imports",
		},
	}
	return flat, metadata, nil
}

// chooseRootPackage 选 bundle 的根包：preferred 在场即用，否则取字典序
// 最小的非空包名；全无包名时落到固定占位包。
func chooseRootPackage(files []*descriptorpb.FileDescriptorProto, preferred string) string {
	for _, file := range files {
		if file.GetPackage() == preferred {
			return preferred
		}
	}
	packages := make([]string, 0, len(files))
	seen := make(map[string]struct{})
	for _, file := range files {
		if file.GetPackage() == "" {
			continue
		}
		if _, ok := seen[file.GetPackage()]; !ok {
			seen[file.GetPackage()] = struct{}{}
			packages = append(packages, file.GetPackage())
		}
	}
	sort.Strings(packages)
	if len(packages) > 0 {
		return packages[0]
	}
	return "protoextract_flattened"
}

// registerFileSymbols 登记一个文件的全部顶层符号：根包文件直接用原名，
// 其他包加 CamelCase 前缀；撞名经 reserve 加序号，原名→新名写映射表。
func (state *flattenState) registerFileSymbols(file *descriptorpb.FileDescriptorProto) {
	prefix := packagePrefix(file.GetPackage())
	root := file.GetPackage() == state.rootPackage
	for _, message := range file.GetMessageType() {
		original := qualify(file.GetPackage(), message.GetName())
		name := message.GetName()
		if !root {
			name = prefix + "_" + name
		}
		name = state.reserve(name)
		flattened := qualify(state.rootPackage, name)
		state.typeNames[original] = flattened
		state.addMapping("message", original, flattened)
		state.registerNestedMessageSymbols(original, flattened, message)
	}
	for _, enum := range file.GetEnumType() {
		original := qualify(file.GetPackage(), enum.GetName())
		name := enum.GetName()
		if !root {
			name = prefix + "_" + name
		}
		name = state.reserve(name)
		flattened := qualify(state.rootPackage, name)
		state.typeNames[original] = flattened
		state.addMapping("enum", original, flattened)

		values := make(map[string]string, len(enum.GetValue()))
		for _, value := range enum.GetValue() {
			newName := value.GetName()
			if !root {
				newName = state.reserve(name + "_" + value.GetName())
			} else {
				state.usedTopLevel[newName] = struct{}{}
			}
			values[value.GetName()] = newName
			if newName != value.GetName() {
				state.addMapping("enum_value", original+"."+value.GetName(), flattened+"."+newName)
			}
		}
		state.enumValues[original] = values
	}
	for _, service := range file.GetService() {
		original := qualify(file.GetPackage(), service.GetName())
		name := service.GetName()
		if !root {
			name = prefix + "_" + name
		}
		name = state.reserve(name)
		flattened := qualify(state.rootPackage, name)
		state.serviceNames[original] = flattened
		state.addMapping("service", original, flattened)
	}
	for _, extension := range file.GetExtension() {
		original := qualify(file.GetPackage(), extension.GetName())
		name := extension.GetName()
		if !root {
			name = prefix + "_" + name
		}
		name = state.reserve(name)
		flattened := qualify(state.rootPackage, name)
		state.extensionNames[original] = flattened
		state.addMapping("extension", original, flattened)
	}
}

// registerNestedMessageSymbols 递归登记嵌套消息与嵌套枚举：嵌套名不参与
// 顶层改名（展平只动顶层声明），但要进 typeNames/enumValues 供引用改写查。
func (state *flattenState) registerNestedMessageSymbols(originalParent, flattenedParent string, message *descriptorpb.DescriptorProto) {
	for _, nested := range message.GetNestedType() {
		original := qualify(originalParent, nested.GetName())
		flattened := qualify(flattenedParent, nested.GetName())
		state.typeNames[original] = flattened
		state.registerNestedMessageSymbols(original, flattened, nested)
	}
	for _, enum := range message.GetEnumType() {
		original := qualify(originalParent, enum.GetName())
		flattened := qualify(flattenedParent, enum.GetName())
		state.typeNames[original] = flattened
		values := make(map[string]string, len(enum.GetValue()))
		for _, value := range enum.GetValue() {
			values[value.GetName()] = value.GetName()
		}
		state.enumValues[original] = values
	}
}

// reserve 占用一个顶层名：撞名时追加 _2、_3… 序号直到空闲。
func (state *flattenState) reserve(candidate string) string {
	name := candidate
	for suffix := 2; ; suffix++ {
		if _, exists := state.usedTopLevel[name]; !exists {
			state.usedTopLevel[name] = struct{}{}
			return name
		}
		name = fmt.Sprintf("%s_%d", candidate, suffix)
	}
}

// addMapping 追加一条原名→展平名映射记录。
func (state *flattenState) addMapping(kind, original, flattened string) {
	state.mappings = append(state.mappings, symbolMapping{Kind: kind, Original: original, Flattened: flattened})
}

// rewriteMessage 就地改写消息副本：清自定义选项、拆 proto3 optional
// 合成 oneof、重写字段/扩展的类型引用，并递归处理嵌套声明。
func (state *flattenState) rewriteMessage(file *descriptorpb.FileDescriptorProto, original string, message *descriptorpb.DescriptorProto) error {
	message.Options = cleanMessageOptions(message.GetOptions())
	if err := removeSyntheticOneofs(message); err != nil {
		return fmt.Errorf("%s: %w", original, err)
	}
	for _, field := range message.GetField() {
		state.rewriteField(file, field)
	}
	for _, extension := range message.GetExtension() {
		state.rewriteField(file, extension)
	}
	for _, nested := range message.GetNestedType() {
		if err := state.rewriteMessage(file, qualify(original, nested.GetName()), nested); err != nil {
			return err
		}
	}
	for _, enum := range message.GetEnumType() {
		state.rewriteEnum(qualify(original, enum.GetName()), enum)
	}
	for _, oneof := range message.GetOneofDecl() {
		oneof.Options = nil
	}
	return nil
}

// rewriteEnum 就地改写枚举副本：按映射表替换被改名的枚举值并清掉值级选项。
func (state *flattenState) rewriteEnum(original string, enum *descriptorpb.EnumDescriptorProto) {
	enum.Options = cleanEnumOptions(enum.GetOptions())
	for _, value := range enum.GetValue() {
		if mapped := state.enumValues[original][value.GetName()]; mapped != "" {
			value.Name = proto.String(mapped)
		}
		value.Options = nil
	}
}

// rewriteService 就地改写服务副本：清服务/方法选项并把方法的输入输出
// 类型名重定向到展平后的符号。
func (state *flattenState) rewriteService(service *descriptorpb.ServiceDescriptorProto) {
	service.Options = nil
	for _, method := range service.GetMethod() {
		method.InputType = proto.String(state.rewriteTypeName(method.GetInputType()))
		method.OutputType = proto.String(state.rewriteTypeName(method.GetOutputType()))
		method.Options = nil
	}
}

// rewriteField 就地改写字段副本：类型/extendee 引用重定向、枚举默认值
// 跟随改名、扩展字段清 json_name（.proto 源里不可拼写）、packed 选项按
// 原文件语法显式落定、剥掉 proto3_optional 标记。
func (state *flattenState) rewriteField(file *descriptorpb.FileDescriptorProto, field *descriptorpb.FieldDescriptorProto) {
	isExtension := field.GetExtendee() != ""
	originalType := strings.TrimPrefix(field.GetTypeName(), ".")
	field.TypeName = optionalString(state.rewriteTypeName(field.GetTypeName()))
	field.Extendee = optionalString(state.rewriteTypeName(field.GetExtendee()))
	if isExtension {
		// Extension declarations cannot spell out json_name in .proto source.
		field.JsonName = nil
	}
	if field.GetDefaultValue() != "" && field.GetType() == descriptorpb.FieldDescriptorProto_TYPE_ENUM {
		if mapped := state.enumValues[originalType][field.GetDefaultValue()]; mapped != "" {
			field.DefaultValue = proto.String(mapped)
		}
	}
	if effectivePacked(file, field) {
		field.Options = &descriptorpb.FieldOptions{Packed: proto.Bool(true)}
	} else {
		field.Options = nil
	}
	field.Proto3Optional = nil
}

// rewriteTypeName 把一个 .pkg.Type 引用重定向到展平后的全限定名；
// 不在映射表里的名字原样返回（标量字段为空串、外部缺依赖类型原样保留）。
func (state *flattenState) rewriteTypeName(name string) string {
	if name == "" {
		return ""
	}
	original := strings.TrimPrefix(name, ".")
	if mapped := state.typeNames[original]; mapped != "" {
		return "." + mapped
	}
	return name
}

// optionalString 把空串折成 nil 指针：描述符里空名等价于字段缺席。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return proto.String(value)
}

// effectivePacked 计算字段的真实 packed 编码：proto3 标量/枚举 repeated
// 默认 packed，proto2 默认不 packed，显式 options.packed 恒优先——展平
// 后统一写 proto2 语法，必须把隐式默认落成显式选项保住线网编码。
func effectivePacked(file *descriptorpb.FileDescriptorProto, field *descriptorpb.FieldDescriptorProto) bool {
	if field.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REPEATED || !isPackable(field.GetType()) {
		return false
	}
	if field.GetOptions() != nil && field.GetOptions().Packed != nil {
		return field.GetOptions().GetPacked()
	}
	return file.GetSyntax() == "proto3"
}

// isPackable 报字段类型是否可 packed 编码（数值/布尔/枚举标量）。
func isPackable(kind descriptorpb.FieldDescriptorProto_Type) bool {
	switch kind {
	case descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
		descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		descriptorpb.FieldDescriptorProto_TYPE_INT64,
		descriptorpb.FieldDescriptorProto_TYPE_UINT64,
		descriptorpb.FieldDescriptorProto_TYPE_INT32,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED32,
		descriptorpb.FieldDescriptorProto_TYPE_BOOL,
		descriptorpb.FieldDescriptorProto_TYPE_UINT32,
		descriptorpb.FieldDescriptorProto_TYPE_ENUM,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED32,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_SINT32,
		descriptorpb.FieldDescriptorProto_TYPE_SINT64:
		return true
	default:
		return false
	}
}

// removeSyntheticOneofs 拆掉 proto3 optional 编译器合成的单成员 oneof：
// proto2 源不允许这种形态，optional 关键字直接表达 presence。引用被拆
// oneof 的字段重置索引，其余 oneof 索引重排；索引错位按结构损伤报错。
func removeSyntheticOneofs(message *descriptorpb.DescriptorProto) error {
	removed := make(map[int32]struct{})
	for _, field := range message.GetField() {
		if field.GetProto3Optional() {
			if field.OneofIndex == nil {
				return fmt.Errorf("proto3_optional field %s has no oneof index", field.GetName())
			}
			removed[field.GetOneofIndex()] = struct{}{}
		}
	}
	if len(removed) == 0 {
		return nil
	}
	indexMap := make(map[int32]int32, len(message.GetOneofDecl()))
	kept := make([]*descriptorpb.OneofDescriptorProto, 0, len(message.GetOneofDecl())-len(removed))
	for oldIndex, oneof := range message.GetOneofDecl() {
		if _, drop := removed[int32(oldIndex)]; drop {
			continue
		}
		indexMap[int32(oldIndex)] = int32(len(kept))
		kept = append(kept, oneof)
	}
	message.OneofDecl = kept
	for _, field := range message.GetField() {
		if field.GetProto3Optional() {
			field.OneofIndex = nil
			field.Proto3Optional = nil
			continue
		}
		if field.OneofIndex != nil {
			mapped, ok := indexMap[field.GetOneofIndex()]
			if !ok {
				return fmt.Errorf("field %s references removed synthetic oneof", field.GetName())
			}
			field.OneofIndex = proto.Int32(mapped)
		}
	}
	return nil
}

// cleanMessageOptions 只保留对展平 bundle 有语义的消息选项
// （message_set_wire_format、map_entry），其余全部剥掉。
func cleanMessageOptions(options *descriptorpb.MessageOptions) *descriptorpb.MessageOptions {
	if options == nil {
		return nil
	}
	clean := &descriptorpb.MessageOptions{}
	if options.MessageSetWireFormat != nil {
		clean.MessageSetWireFormat = proto.Bool(options.GetMessageSetWireFormat())
	}
	if options.MapEntry != nil {
		clean.MapEntry = proto.Bool(options.GetMapEntry())
	}
	if clean.MessageSetWireFormat == nil && clean.MapEntry == nil {
		return nil
	}
	return clean
}

// cleanEnumOptions 只保留 allow_alias（影响枚举编译合法性），其余剥掉。
func cleanEnumOptions(options *descriptorpb.EnumOptions) *descriptorpb.EnumOptions {
	if options == nil || options.AllowAlias == nil {
		return nil
	}
	return &descriptorpb.EnumOptions{AllowAlias: proto.Bool(options.GetAllowAlias())}
}

// remapSourceInfo 把一份文件的 SourceCodeInfo 路径按展平文件里的声明
// 偏移重编号；无法落到声明上的 location（文件级注释等）把注释文本提出
// 到返回值的第二槽，由调用方汇进 bundle 头部。
func remapSourceInfo(file *descriptorpb.FileDescriptorProto, messageOffset, enumOffset, serviceOffset, extensionOffset int) (*descriptorpb.SourceCodeInfo, []string) {
	if file.GetSourceCodeInfo() == nil {
		return nil, nil
	}
	mapped := &descriptorpb.SourceCodeInfo{}
	var unmapped []string
	for _, location := range file.GetSourceCodeInfo().GetLocation() {
		cloned := proto.Clone(location).(*descriptorpb.SourceCodeInfo_Location)
		if len(cloned.GetPath()) >= 2 {
			switch cloned.GetPath()[0] {
			case 4:
				cloned.Path[1] += int32(messageOffset)
			case 5:
				cloned.Path[1] += int32(enumOffset)
			case 6:
				cloned.Path[1] += int32(serviceOffset)
			case 7:
				cloned.Path[1] += int32(extensionOffset)
			default:
				if text := locationCommentText(file.GetName(), cloned); text != "" {
					unmapped = append(unmapped, text)
				}
				continue
			}
			mapped.Location = append(mapped.Location, cloned)
			continue
		}
		if text := locationCommentText(file.GetName(), cloned); text != "" {
			unmapped = append(unmapped, text)
		}
	}
	return mapped, unmapped
}

// locationCommentText 提取一条 location 携带的全部注释并冠以来源文件名。
func locationCommentText(fileName string, location *descriptorpb.SourceCodeInfo_Location) string {
	var parts []string
	parts = append(parts, location.GetLeadingDetachedComments()...)
	if location.GetLeadingComments() != "" {
		parts = append(parts, location.GetLeadingComments())
	}
	if location.GetTrailingComments() != "" {
		parts = append(parts, location.GetTrailingComments())
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Comments from %s:\n%s", fileName, strings.Join(parts, "\n"))
}

// mergeSourceInfo 把一份 SourceCodeInfo 的 location 追加进另一份。
func mergeSourceInfo(current, addition *descriptorpb.SourceCodeInfo) *descriptorpb.SourceCodeInfo {
	if addition == nil || len(addition.GetLocation()) == 0 {
		return current
	}
	if current == nil {
		current = &descriptorpb.SourceCodeInfo{}
	}
	current.Location = append(current.Location, addition.GetLocation()...)
	return current
}

// filterSourceInfo 复核重映射后的 location 路径在展平文件里真实存在
// （改名/删字段会让路径悬空）；悬空项把注释文本提出到第二返回值。
func filterSourceInfo(file *descriptorpb.FileDescriptorProto, originalName string, source *descriptorpb.SourceCodeInfo) (*descriptorpb.SourceCodeInfo, []string) {
	if source == nil {
		return nil, nil
	}
	valid := &descriptorpb.SourceCodeInfo{}
	var removed []string
	for _, location := range source.GetLocation() {
		if sourcePathExists(file.ProtoReflect(), location.GetPath()) {
			valid.Location = append(valid.Location, location)
			continue
		}
		if text := locationCommentText(originalName, location); text != "" {
			removed = append(removed, text)
		}
	}
	return valid, removed
}

// sourcePathExists 沿 source location 的字段号/下标交替路径逐级下钻，
// 验证路径在描述符消息上可解。
func sourcePathExists(message protoreflect.Message, path []int32) bool {
	if len(path) == 0 {
		return true
	}
	for index := 0; index < len(path); {
		field := message.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(path[index]))
		if field == nil {
			return false
		}
		index++
		if field.IsList() {
			if index >= len(path) {
				return true
			}
			itemIndex := int(path[index])
			index++
			list := message.Get(field).List()
			if itemIndex < 0 || itemIndex >= list.Len() {
				return false
			}
			if index == len(path) {
				return true
			}
			if field.Message() == nil {
				return false
			}
			message = list.Get(itemIndex).Message()
			continue
		}
		if field.Message() == nil {
			return index == len(path)
		}
		if !message.Has(field) {
			return false
		}
		if index == len(path) {
			return true
		}
		message = message.Get(field).Message()
	}
	return true
}

// qualify 按 proto 全限定名规则用点拼接父子名；任一侧为空时回另一侧。
func qualify(parent, name string) string {
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return parent + "." + name
}

// shortName 取全限定名的最后一段（去掉包/父消息前缀）。
func shortName(fullName string) string {
	if index := strings.LastIndexByte(fullName, '.'); index >= 0 {
		return fullName[index+1:]
	}
	return fullName
}

// packagePrefix 把包名转成 CamelCase 前缀（exa.chat_pb → ExaChatPb），
// 非字母数字字符当分词界；空包名回固定占位 "NoPackage"。
func packagePrefix(pkg string) string {
	var out strings.Builder
	upper := true
	for _, r := range pkg {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		out.WriteRune(r)
	}
	if out.Len() == 0 {
		return "NoPackage"
	}
	return out.String()
}
