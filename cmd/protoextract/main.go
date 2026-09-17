// 本文件提供从编译后的 Go 二进制提取 protobuf 描述符的命令行入口。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhump/protoreflect/v2/protoprint"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

const defaultBundleName = "all-protos.proto"

type scanStats struct {
	Candidates int `json:"candidates"`
	Duplicates int `json:"duplicates"`
}

type fileManifest struct {
	Name                  string   `json:"name"`
	Package               string   `json:"package,omitempty"`
	Syntax                string   `json:"syntax"`
	Dependencies          []string `json:"dependencies,omitempty"`
	OptionDependencies    []string `json:"option_dependencies,omitempty"`
	Messages              int      `json:"messages"`
	Enums                 int      `json:"enums"`
	Services              int      `json:"services"`
	Extensions            int      `json:"extensions"`
	SourceInfoLocations   int      `json:"source_info_locations"`
	LocationsWithComments int      `json:"locations_with_comments"`
}

type extractionManifest struct {
	Binary                    string          `json:"binary"`
	Bundle                    string          `json:"bundle"`
	DescriptorCount           int             `json:"descriptor_count"`
	CandidateCount            int             `json:"candidate_count"`
	DuplicateCount            int             `json:"duplicate_count"`
	FilesWithSourceInfo       int             `json:"files_with_source_info"`
	FilesWithComments         int             `json:"files_with_comments"`
	CommentLocationCount      int             `json:"comment_location_count"`
	MissingDependencies       []string        `json:"missing_dependencies"`
	BundleIsCompilableAsOne   bool            `json:"bundle_is_compilable_as_one_proto"`
	BundleCompilationGuidance string          `json:"bundle_compilation_guidance"`
	Flattened                 flattenMetadata `json:"flattened"`
	Files                     []fileManifest  `json:"files"`
}

// main 扫描源二进制内嵌的 FileDescriptorProto，产出 descriptors.pb
// （原始描述符集）、all-protos.proto（展平单文件 bundle）与 manifest.json。
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <source-binary> <output-directory>\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	binaryPath := os.Args[1]
	outputDir := os.Args[2]

	binaryPath, outputDir, err := prepareFreshOutput(binaryPath, outputDir)
	check(err)

	binary, err := os.ReadFile(binaryPath)
	check(err)
	files, stats := scanFileDescriptors(binary)
	if len(files) == 0 {
		check(errors.New("no FileDescriptorProto values found"))
	}

	all := sortedDescriptors(files)
	descriptorSet := &descriptorpb.FileDescriptorSet{File: all}
	descriptorBin, err := proto.Marshal(descriptorSet)
	check(err)
	check(os.WriteFile(filepath.Join(outputDir, "descriptors.pb"), descriptorBin, 0o644))

	if resolutionErr := resolveDescriptors(descriptorSet); resolutionErr != nil {
		fmt.Fprintf(os.Stderr, "warning: descriptor set is not fully resolvable: %v\n", resolutionErr)
	}

	flattened, flattening, err := flattenDescriptors(all, preferredRootPackage)
	check(err)
	bundle, compilable, err := renderFlattened(flattened, len(all), protoprint.Printer{})
	check(err)
	check(os.WriteFile(filepath.Join(outputDir, defaultBundleName), bundle, 0o644))

	manifest := buildManifest(binaryPath, defaultBundleName, all, stats, flattening, compilable)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	check(err)
	manifestBytes = append(manifestBytes, '\n')
	check(os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifestBytes, 0o644))

	fmt.Printf("extracted %d unique descriptors (%d duplicate candidates)\n", len(all), stats.Duplicates)
	if compilable {
		fmt.Printf("compilable flattened proto: %s\n", filepath.Join(outputDir, defaultBundleName))
	} else {
		fmt.Printf("flattened proto (has unresolved references): %s\n", filepath.Join(outputDir, defaultBundleName))
	}
	fmt.Printf("comments: %d locations across %d files\n", manifest.CommentLocationCount, manifest.FilesWithComments)
	if len(manifest.MissingDependencies) > 0 {
		fmt.Printf("warning: %d referenced dependencies were not embedded; see manifest.json\n", len(manifest.MissingDependencies))
	}
}

// prepareFreshOutput 规范化源与输出路径并在安全校验后重建输出目录：
// 输出目录被整体删除重建，调用方拿到的两个返回值都是绝对路径。
func prepareFreshOutput(sourcePath, outputPath string) (string, string, error) {
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", "", err
	}
	sourceAbs = filepath.Clean(sourceAbs)
	sourceInfo, err := os.Stat(sourceAbs)
	if err != nil {
		return "", "", fmt.Errorf("source binary: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return "", "", fmt.Errorf("source binary must be a regular file: %s", sourceAbs)
	}

	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return "", "", err
	}
	outputAbs = filepath.Clean(outputAbs)
	if err := validateDestructiveOutputPath(sourceAbs, outputAbs); err != nil {
		return "", "", err
	}

	if err := os.RemoveAll(outputAbs); err != nil {
		return "", "", fmt.Errorf("remove output directory %s: %w", outputAbs, err)
	}
	if err := os.MkdirAll(outputAbs, 0o755); err != nil {
		return "", "", fmt.Errorf("create output directory %s: %w", outputAbs, err)
	}
	return sourceAbs, outputAbs, nil
}

// validateDestructiveOutputPath 在 RemoveAll 之前拒绝危险输出路径：
// 文件系统根、用户家目录、包含当前工作目录或源二进制的路径。
func validateDestructiveOutputPath(sourceAbs, outputAbs string) error {
	if outputAbs == string(filepath.Separator) {
		return errors.New("refusing to delete filesystem root as output directory")
	}
	home, err := os.UserHomeDir()
	if err == nil && outputAbs == filepath.Clean(home) {
		return errors.New("refusing to delete the user home directory as output directory")
	}
	cwd, err := os.Getwd()
	if err == nil {
		cwd, _ = filepath.Abs(cwd)
		if outputAbs == filepath.Clean(cwd) || pathContains(outputAbs, cwd) {
			return fmt.Errorf("refusing to delete an output directory that contains the current working directory: %s", outputAbs)
		}
	}
	if outputAbs == sourceAbs || pathContains(outputAbs, sourceAbs) {
		return fmt.Errorf("refusing to delete an output directory that contains the source binary: %s", outputAbs)
	}
	return nil
}

// pathContains 报 candidate 是否等于或位于 directory 之内（相对路径不含 .. 上跳）。
func pathContains(directory, candidate string) bool {
	rel, err := filepath.Rel(directory, candidate)
	if err != nil || rel == "." {
		return err == nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sortedDescriptors 把去重后的文件名映射摊平成按文件名排序的切片，
// 让产物（descriptors.pb、manifest、bundle 内声明序）与 map 遍历序无关。
func sortedDescriptors(files map[string]*descriptorpb.FileDescriptorProto) []*descriptorpb.FileDescriptorProto {
	all := make([]*descriptorpb.FileDescriptorProto, 0, len(files))
	for _, file := range files {
		all = append(all, file)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	return all
}

// resolveDescriptors 只做整体可解性校验：返回值即唯一消费物（调用方只看
// error 决定是否告警），不再构造无人使用的 resolved 映射。
func resolveDescriptors(set *descriptorpb.FileDescriptorSet) error {
	if _, err := protodesc.NewFiles(set); err == nil {
		return nil
	} else {
		registryErr := err
		// A binary may reference a descriptor that is not linked into that binary.
		// Still render every descriptor we did recover, with unresolved references
		// kept as fully-qualified placeholders, and report missing imports separately.
		for _, file := range set.GetFile() {
			if _, err := (protodesc.FileOptions{AllowUnresolvable: true}).New(file, nil); err != nil {
				return fmt.Errorf("%s: %w", file.GetName(), err)
			}
		}
		return registryErr
	}
}

// renderFlattened 把展平后的描述符解析成 FileDescriptor 并打印成 .proto 源，
// 返回值第二位报告 bundle 是否单文件自洽可编译。先走严格解析；仅当存在
// 未嵌入二进制的引用类型时回落到容忍解析——未解引用按全限定名占位照常
// 落盘，与 resolveDescriptors 的 warn-only 契约一致。容忍解析也失败说明
// 是缺类型之外的结构损伤，仍按硬错误处理。
func renderFlattened(file *descriptorpb.FileDescriptorProto, originalFileCount int, printer protoprint.Printer) ([]byte, bool, error) {
	resolved, err := protodesc.NewFile(file, nil)
	compilable := err == nil
	if !compilable {
		resolved, err = (protodesc.FileOptions{AllowUnresolvable: true}).New(file, nil)
		if err != nil {
			return nil, false, fmt.Errorf("resolve flattened descriptor: %w", err)
		}
	}
	var source bytes.Buffer
	if err := printer.PrintProtoFile(resolved, &source); err != nil {
		return nil, false, fmt.Errorf("print flattened descriptor: %w", err)
	}
	var out bytes.Buffer
	fmt.Fprintln(&out, "// Generated by protoextract from embedded FileDescriptorProto values.")
	fmt.Fprintf(&out, "// Flattened original file count: %d. See manifest.json for renamed symbols.\n", originalFileCount)
	fmt.Fprintln(&out, "// descriptors.pb preserves the original packages, syntax, options, and file boundaries.")
	out.Write(source.Bytes())
	return out.Bytes(), compilable, nil
}

// buildManifest 汇总提取结果成 manifest.json：逐文件清单、缺失依赖表与
// bundle 可编译性。compilable 取 renderFlattened 严格解析的结果——不可
// 编译时把指引换成「缺引用类型、需随 missing_dependencies 一起编译」。
func buildManifest(binaryPath, bundleName string, files []*descriptorpb.FileDescriptorProto, stats scanStats, flattening flattenMetadata, compilable bool) extractionManifest {
	guidance := "Compile all-protos.proto directly. Use descriptors.pb when original package names, service paths, syntax, or declaration options are required."
	if !compilable {
		guidance = "all-protos.proto is not self-contained: it references types that were not embedded in the binary (see missing_dependencies). Provide those .proto files when compiling, or use descriptors.pb which preserves the original file boundaries."
	}
	manifest := extractionManifest{
		Binary:                    binaryPath,
		Bundle:                    bundleName,
		DescriptorCount:           len(files),
		CandidateCount:            stats.Candidates,
		DuplicateCount:            stats.Duplicates,
		MissingDependencies:       missingDependencies(files),
		BundleIsCompilableAsOne:   compilable,
		BundleCompilationGuidance: guidance,
		Flattened:                 flattening,
		Files:                     make([]fileManifest, 0, len(files)),
	}
	for _, file := range files {
		locations := file.GetSourceCodeInfo().GetLocation()
		commentLocations := countCommentLocations(file)
		if len(locations) > 0 {
			manifest.FilesWithSourceInfo++
		}
		if commentLocations > 0 {
			manifest.FilesWithComments++
			manifest.CommentLocationCount += commentLocations
		}
		manifest.Files = append(manifest.Files, fileManifest{
			Name:                  file.GetName(),
			Package:               file.GetPackage(),
			Syntax:                descriptorSyntax(file),
			Dependencies:          append([]string(nil), file.GetDependency()...),
			OptionDependencies:    append([]string(nil), file.GetOptionDependency()...),
			Messages:              len(file.GetMessageType()),
			Enums:                 len(file.GetEnumType()),
			Services:              len(file.GetService()),
			Extensions:            len(file.GetExtension()),
			SourceInfoLocations:   len(locations),
			LocationsWithComments: commentLocations,
		})
	}
	return manifest
}

// descriptorSyntax 归一化文件的语法标注：syntax 缺省按 editions/proto2 推断。
func descriptorSyntax(file *descriptorpb.FileDescriptorProto) string {
	if file.GetSyntax() != "" {
		return file.GetSyntax()
	}
	if file.GetEdition() != descriptorpb.Edition_EDITION_UNKNOWN {
		return "editions"
	}
	return "proto2"
}

// countCommentLocations 统计文件内带注释的 source location 数。
func countCommentLocations(file *descriptorpb.FileDescriptorProto) int {
	count := 0
	for _, location := range file.GetSourceCodeInfo().GetLocation() {
		if location.GetLeadingComments() != "" || location.GetTrailingComments() != "" || len(location.GetLeadingDetachedComments()) > 0 {
			count++
		}
	}
	return count
}

// missingDependencies 列出被 dependency/option_dependency 引用却不在提取
// 结果里的 .proto 名——二进制没有链接这些文件的描述符，排序后供 manifest 上报。
func missingDependencies(files []*descriptorpb.FileDescriptorProto) []string {
	present := make(map[string]struct{}, len(files))
	for _, file := range files {
		present[file.GetName()] = struct{}{}
	}
	missingSet := make(map[string]struct{})
	for _, file := range files {
		dependencies := append(append([]string(nil), file.GetDependency()...), file.GetOptionDependency()...)
		for _, dependency := range dependencies {
			if _, ok := present[dependency]; !ok {
				missingSet[dependency] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for dependency := range missingSet {
		missing = append(missing, dependency)
	}
	sort.Strings(missing)
	return missing
}

// scanFileDescriptors 逐字节扫描二进制，按「0x0a tag + .proto 文件名」
// 锚点定位内嵌 FileDescriptorProto 并试解；同名重复候选保留质量最高者。
func scanFileDescriptors(binary []byte) (map[string]*descriptorpb.FileDescriptorProto, scanStats) {
	files := make(map[string]*descriptorpb.FileDescriptorProto)
	stats := scanStats{}
	for offset := 0; offset < len(binary)-8; offset++ {
		if binary[offset] != 0x0a { // FileDescriptorProto.name, field 1, bytes
			continue
		}
		name, nameBytes := protowire.ConsumeBytes(binary[offset+1:])
		if nameBytes < 0 || len(name) < 7 || !strings.HasSuffix(string(name), ".proto") || !isProtoPath(name) {
			continue
		}

		descriptor, ok := extractDescriptorAt(binary, offset, string(name))
		if !ok {
			continue
		}
		stats.Candidates++
		if current, exists := files[descriptor.GetName()]; exists {
			stats.Duplicates++
			if descriptorQuality(descriptor) <= descriptorQuality(current) {
				continue
			}
		}
		files[descriptor.GetName()] = descriptor
	}
	return files, stats
}

// extractDescriptorAt 从锚点偏移起尝试消费一段合法的 FileDescriptorProto
// 线网编码：先按字段边界走完整段，再从最长前缀回退试 unmarshal——正常
// 情况一次成功，末尾混入的偶然描述符样字节不会污染提取。
func extractDescriptorAt(binary []byte, start int, expectedName string) (*descriptorpb.FileDescriptorProto, bool) {
	data := binary[start:]
	consumed := 0
	boundaries := make([]int, 0, 32)

	for len(data) > 0 {
		number, wireType, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 || !validFileDescriptorField(number, wireType) {
			break
		}
		data = data[tagBytes:]
		consumed += tagBytes

		valueBytes := protowire.ConsumeFieldValue(number, wireType, data)
		if valueBytes < 0 {
			break
		}
		data = data[valueBytes:]
		consumed += valueBytes
		boundaries = append(boundaries, consumed)
	}

	// The normal case succeeds on the first attempt: the last recognized field
	// is the end of the serialized descriptor. Walk backwards so incidental
	// descriptor-looking bytes immediately after it cannot poison extraction.
	for i := len(boundaries) - 1; i >= 0; i-- {
		var descriptor descriptorpb.FileDescriptorProto
		if err := proto.Unmarshal(binary[start:start+boundaries[i]], &descriptor); err != nil {
			continue
		}
		if validDescriptor(&descriptor, expectedName) {
			return &descriptor, true
		}
	}
	return nil, false
}

// validDescriptor 校验试解结果确为描述符而非偶然可解的字节序：
// 名回读一致、无 unknown 字段、syntax/依赖名/依赖索引均在合法域内。
func validDescriptor(descriptor *descriptorpb.FileDescriptorProto, expectedName string) bool {
	if descriptor.GetName() != expectedName || !isProtoPath([]byte(descriptor.GetName())) {
		return false
	}
	if len(descriptor.ProtoReflect().GetUnknown()) != 0 {
		return false
	}
	syntax := descriptor.GetSyntax()
	if syntax != "" && syntax != "proto2" && syntax != "proto3" && syntax != "editions" {
		return false
	}
	for _, dependency := range descriptor.GetDependency() {
		if !strings.HasSuffix(dependency, ".proto") || !isProtoPath([]byte(dependency)) {
			return false
		}
	}
	for _, dependency := range descriptor.GetOptionDependency() {
		if !strings.HasSuffix(dependency, ".proto") || !isProtoPath([]byte(dependency)) {
			return false
		}
	}
	dependencyCount := int32(len(descriptor.GetDependency()))
	for _, index := range append(append([]int32(nil), descriptor.GetPublicDependency()...), descriptor.GetWeakDependency()...) {
		if index < 0 || index >= dependencyCount {
			return false
		}
	}
	return true
}

// descriptorQuality 给同名候选打分：声明数优先，其次注释/源码信息，
// 最后体积——同一文件的多个内嵌副本取信息最全的一份。
func descriptorQuality(descriptor *descriptorpb.FileDescriptorProto) int {
	// Prefer complete declarations, then source information/comments, then size.
	declarations := len(descriptor.GetMessageType()) + len(descriptor.GetEnumType()) + len(descriptor.GetService()) + len(descriptor.GetExtension())
	return declarations*1_000_000 + countCommentLocations(descriptor)*100_000 + len(descriptor.GetSourceCodeInfo().GetLocation())*1_000 + proto.Size(descriptor)
}

// validFileDescriptorField 按 FileDescriptorProto 的字段号-线型表判断
// 当前 tag 是否可能属于一个描述符，用于在二进制里找字段边界。
func validFileDescriptorField(number protowire.Number, wireType protowire.Type) bool {
	switch number {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 12, 15:
		return wireType == protowire.BytesType
	case 10, 11:
		// public_dependency and weak_dependency are repeated int32. Accept both
		// unpacked and packed encodings.
		return wireType == protowire.VarintType || wireType == protowire.BytesType
	case 14:
		return wireType == protowire.VarintType
	default:
		return false
	}
}

// isProtoPath 校验候选名是否为合法的 .proto 相对路径：可打印 ASCII、
// 无反斜杠、无绝对路径与 . / .. 段——挡住扫描命中的非路径字节串。
func isProtoPath(value []byte) bool {
	if len(value) == 0 || value[0] == '/' || bytes.Contains(value, []byte("\\")) {
		return false
	}
	for _, b := range value {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	for _, part := range strings.Split(string(value), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// check 是 main 流程的统一错误出口：任何步骤失败打印错误并以 1 退出。
func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
