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
	"google.golang.org/protobuf/reflect/protoreflect"
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
	writeProtoBinary(filepath.Join(outputDir, "descriptors.pb"), descriptorSet)

	_, resolutionErr := resolveDescriptors(descriptorSet)
	if resolutionErr != nil {
		fmt.Fprintf(os.Stderr, "warning: descriptor set is not fully resolvable: %v\n", resolutionErr)
	}

	flattened, flattening, err := flattenDescriptors(all, preferredRootPackage)
	check(err)
	bundle, err := renderFlattened(flattened, len(all), protoprint.Printer{})
	check(err)
	check(os.WriteFile(filepath.Join(outputDir, defaultBundleName), bundle, 0o644))

	manifest := buildManifest(binaryPath, defaultBundleName, all, stats, flattening)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	check(err)
	manifestBytes = append(manifestBytes, '\n')
	check(os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifestBytes, 0o644))

	fmt.Printf("extracted %d unique descriptors (%d duplicate candidates)\n", len(all), stats.Duplicates)
	fmt.Printf("compilable flattened proto: %s\n", filepath.Join(outputDir, defaultBundleName))
	fmt.Printf("comments: %d locations across %d files\n", manifest.CommentLocationCount, manifest.FilesWithComments)
	if len(manifest.MissingDependencies) > 0 {
		fmt.Printf("warning: %d referenced dependencies were not embedded; see manifest.json\n", len(manifest.MissingDependencies))
	}
}

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

func pathContains(directory, candidate string) bool {
	rel, err := filepath.Rel(directory, candidate)
	if err != nil || rel == "." {
		return err == nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedDescriptors(files map[string]*descriptorpb.FileDescriptorProto) []*descriptorpb.FileDescriptorProto {
	all := make([]*descriptorpb.FileDescriptorProto, 0, len(files))
	for _, file := range files {
		all = append(all, file)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	return all
}

func writeProtoBinary(path string, message proto.Message) {
	data, err := proto.Marshal(message)
	check(err)
	check(os.WriteFile(path, data, 0o644))
}

func resolveDescriptors(set *descriptorpb.FileDescriptorSet) (map[string]protoreflect.FileDescriptor, error) {
	resolved := make(map[string]protoreflect.FileDescriptor, len(set.GetFile()))
	registry, registryErr := protodesc.NewFiles(set)
	if registryErr == nil {
		for _, file := range set.GetFile() {
			fd, err := registry.FindFileByPath(file.GetName())
			if err != nil {
				return nil, err
			}
			resolved[file.GetName()] = fd
		}
		return resolved, nil
	}

	// A binary may reference a descriptor that is not linked into that binary.
	// Still render every descriptor we did recover, with unresolved references
	// kept as fully-qualified placeholders, and report missing imports separately.
	for _, file := range set.GetFile() {
		fd, err := (protodesc.FileOptions{AllowUnresolvable: true}).New(file, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.GetName(), err)
		}
		resolved[file.GetName()] = fd
	}
	return resolved, registryErr
}

func renderFlattened(file *descriptorpb.FileDescriptorProto, originalFileCount int, printer protoprint.Printer) ([]byte, error) {
	resolved, err := protodesc.NewFile(file, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve flattened descriptor: %w", err)
	}
	var source bytes.Buffer
	if err := printer.PrintProtoFile(resolved, &source); err != nil {
		return nil, fmt.Errorf("print flattened descriptor: %w", err)
	}
	var out bytes.Buffer
	fmt.Fprintln(&out, "// Generated by protoextract from embedded FileDescriptorProto values.")
	fmt.Fprintf(&out, "// Flattened original file count: %d. See manifest.json for renamed symbols.\n", originalFileCount)
	fmt.Fprintln(&out, "// descriptors.pb preserves the original packages, syntax, options, and file boundaries.")
	out.Write(source.Bytes())
	return out.Bytes(), nil
}

func buildManifest(binaryPath, bundleName string, files []*descriptorpb.FileDescriptorProto, stats scanStats, flattening flattenMetadata) extractionManifest {
	manifest := extractionManifest{
		Binary:                    binaryPath,
		Bundle:                    bundleName,
		DescriptorCount:           len(files),
		CandidateCount:            stats.Candidates,
		DuplicateCount:            stats.Duplicates,
		MissingDependencies:       missingDependencies(files),
		BundleIsCompilableAsOne:   true,
		BundleCompilationGuidance: "Compile all-protos.proto directly. Use descriptors.pb when original package names, service paths, syntax, or declaration options are required.",
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

func descriptorSyntax(file *descriptorpb.FileDescriptorProto) string {
	if file.GetSyntax() != "" {
		return file.GetSyntax()
	}
	if file.GetEdition() != descriptorpb.Edition_EDITION_UNKNOWN {
		return "editions"
	}
	return "proto2"
}

func countCommentLocations(file *descriptorpb.FileDescriptorProto) int {
	count := 0
	for _, location := range file.GetSourceCodeInfo().GetLocation() {
		if location.GetLeadingComments() != "" || location.GetTrailingComments() != "" || len(location.GetLeadingDetachedComments()) > 0 {
			count++
		}
	}
	return count
}

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

func descriptorQuality(descriptor *descriptorpb.FileDescriptorProto) int {
	// Prefer complete declarations, then source information/comments, then size.
	declarations := len(descriptor.GetMessageType()) + len(descriptor.GetEnumType()) + len(descriptor.GetService()) + len(descriptor.GetExtension())
	return declarations*1_000_000 + countCommentLocations(descriptor)*100_000 + len(descriptor.GetSourceCodeInfo().GetLocation())*1_000 + proto.Size(descriptor)
}

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

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
