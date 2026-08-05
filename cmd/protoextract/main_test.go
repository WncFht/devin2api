// 本文件验证 protobuf 描述符提取命令的候选发现和输出行为。
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhump/protoreflect/v2/protoprint"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestPrepareFreshOutputDeletesExistingContents(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "language-server")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "generated")
	if err := os.MkdirAll(filepath.Join(output, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(output, "stale", "old.proto")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotSource, gotOutput, err := prepareFreshOutput(source, output)
	if err != nil {
		t.Fatalf("prepare output: %v", err)
	}
	if gotSource != source || gotOutput != output {
		t.Fatalf("unexpected resolved paths: source=%q output=%q", gotSource, gotOutput)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale output survived reset: %v", err)
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatalf("fresh output directory missing: info=%v err=%v", info, err)
	}
}

func TestValidateDestructiveOutputPathRejectsBroadOrSourceContainingTargets(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "bin", "language-server")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, output := range []string{string(filepath.Separator), root, filepath.Dir(source), source} {
		if err := validateDestructiveOutputPath(source, output); err == nil {
			t.Errorf("expected output %q to be rejected", output)
		}
	}
	if err := validateDestructiveOutputPath(source, filepath.Join(root, "generated")); err != nil {
		t.Fatalf("safe sibling output rejected: %v", err)
	}
}

func TestScanFileDescriptorsHandlesProto2CommentsAndDuplicates(t *testing.T) {
	proto2Poor := testFile("example/legacy.proto", "example.legacy", "", "Legacy")
	proto2Rich := proto.Clone(proto2Poor).(*descriptorpb.FileDescriptorProto)
	proto2Rich.SourceCodeInfo = &descriptorpb.SourceCodeInfo{
		Location: []*descriptorpb.SourceCodeInfo_Location{{
			Path:            []int32{4, 0},
			Span:            []int32{2, 0, 3, 1},
			LeadingComments: proto.String(" Legacy message comment.\n"),
		}},
	}
	proto3 := testFile("example/current.proto", "example.current", "proto3", "Current")
	proto3.Dependency = []string{"example/legacy.proto"}
	proto3.PublicDependency = []int32{0}
	proto3.OptionDependency = []string{"example/legacy.proto"}

	var binary bytes.Buffer
	binary.WriteString("prefix\x00\xff")
	writeDescriptorForTest(t, &binary, proto2Poor)
	binary.Write([]byte{0xff, 0x00, 0x7f})
	writeDescriptorForTest(t, &binary, proto3)
	binary.Write([]byte{0xff, 0x00})
	writeDescriptorForTest(t, &binary, proto2Rich)
	binary.WriteString("suffix")

	files, stats := scanFileDescriptors(binary.Bytes())
	if len(files) != 2 {
		t.Fatalf("got %d descriptors, want 2", len(files))
	}
	if stats.Duplicates != 1 {
		t.Fatalf("got %d duplicate candidates, want 1", stats.Duplicates)
	}
	if got := files["example/legacy.proto"].GetSyntax(); got != "" {
		t.Fatalf("proto2 syntax should remain omitted, got %q", got)
	}
	if got := countCommentLocations(files["example/legacy.proto"]); got != 1 {
		t.Fatalf("got %d comment locations, want 1", got)
	}
	if got := files["example/current.proto"].GetPublicDependency(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("public dependency was not preserved: %v", got)
	}
	if got := files["example/current.proto"].GetOptionDependency(); len(got) != 1 || got[0] != "example/legacy.proto" {
		t.Fatalf("option dependency was not preserved: %v", got)
	}
}

func TestFlattenDescriptorsProducesOneCompilableWireCompatibleFile(t *testing.T) {
	root := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("api.proto"),
		Package:    proto.String(preferredRootPackage),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"chat.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Request"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("tool"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".exa.chat_pb.ToolDef"),
			}},
		}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("ApiServerService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("GetChatMessage"),
				InputType:  proto.String(".exa.api_server_pb.Request"),
				OutputType: proto.String(".exa.chat_pb.ToolDef"),
			}},
		}},
	}
	chat := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("chat.proto"),
		Package: proto.String("exa.chat_pb"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("ToolDef"),
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{
				Name: proto.String("_note"),
			}},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("LabelsEntry"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("key"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("value"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			}},
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("numbers"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
				{Name: proto.String("note"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0), Proto3Optional: proto.Bool(true)},
				{Name: proto.String("labels"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".exa.chat_pb.ToolDef.LabelsEntry")},
			},
		}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{
			Path:            []int32{4, 0},
			Span:            []int32{2, 0, 12, 1},
			LeadingComments: proto.String(" Tool definition documentation.\n"),
		}}},
	}
	legacy := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("legacy.proto"),
		Package: proto.String("old.pkg"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Mode"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("M0"), Number: proto.Int32(0)},
				{Name: proto.String("M1"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Legacy"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("mode"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".old.pkg.Mode"), DefaultValue: proto.String("M1")},
			},
		}},
	}

	flat, metadata, err := flattenDescriptors([]*descriptorpb.FileDescriptorProto{legacy, chat, root}, preferredRootPackage)
	if err != nil {
		t.Fatalf("flatten descriptors: %v", err)
	}
	if flat.GetPackage() != preferredRootPackage || flat.GetSyntax() != "proto2" || len(flat.GetDependency()) != 0 {
		t.Fatalf("unexpected flattened file header: package=%q syntax=%q dependencies=%v", flat.GetPackage(), flat.GetSyntax(), flat.GetDependency())
	}
	if _, err := protodesc.NewFile(flat, nil); err != nil {
		t.Fatalf("flattened descriptor does not resolve by itself: %v", err)
	}

	request := findMessage(t, flat, "Request")
	if got := request.GetField()[0].GetTypeName(); got != ".exa.api_server_pb.ExaChatPb_ToolDef" {
		t.Fatalf("external type was not rewritten: %q", got)
	}
	tool := findMessage(t, flat, "ExaChatPb_ToolDef")
	if len(tool.GetOneofDecl()) != 0 || tool.GetField()[1].GetProto3Optional() || tool.GetField()[1].OneofIndex != nil {
		t.Fatalf("synthetic proto3 optional oneof survived: oneofs=%d field=%v", len(tool.GetOneofDecl()), tool.GetField()[1])
	}
	if !tool.GetField()[0].GetOptions().GetPacked() {
		t.Fatal("proto3 repeated numeric field lost packed wire encoding")
	}
	if !tool.GetNestedType()[0].GetOptions().GetMapEntry() {
		t.Fatal("map-entry marker was not preserved")
	}
	legacyFlat := findMessage(t, flat, "OldPkg_Legacy")
	if legacyFlat.GetField()[0].GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REQUIRED {
		t.Fatal("proto2 required label was not preserved")
	}
	if got := legacyFlat.GetField()[1].GetDefaultValue(); got != "OldPkg_Mode_M1" {
		t.Fatalf("renamed enum default not rewritten: %q", got)
	}
	if len(flat.GetService()) != 1 || flat.GetService()[0].GetName() != "ApiServerService" {
		t.Fatalf("root service name changed: %v", flat.GetService())
	}
	method := flat.GetService()[0].GetMethod()[0]
	if method.GetInputType() != ".exa.api_server_pb.Request" || method.GetOutputType() != ".exa.api_server_pb.ExaChatPb_ToolDef" {
		t.Fatalf("method types not rewritten: input=%q output=%q", method.GetInputType(), method.GetOutputType())
	}

	bundle, err := renderFlattened(flat, 3, protoprint.Printer{})
	if err != nil {
		t.Fatalf("render flattened proto: %v", err)
	}
	text := string(bundle)
	for _, want := range []string{
		"package exa.api_server_pb;",
		"message ExaChatPb_ToolDef",
		"map<string, string> labels",
		"packed = true",
		"required string id",
		"// Tool definition documentation.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("flattened proto does not contain %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "package ") != 1 || metadata.Package != preferredRootPackage {
		t.Fatalf("output is not a single package: metadata=%+v", metadata)
	}
}

func TestManifestReportsMissingDependenciesAndCommentCoverage(t *testing.T) {
	file := testFile("example/service.proto", "example", "proto3", "Request")
	file.Dependency = []string{"missing/types.proto"}
	file.SourceCodeInfo = &descriptorpb.SourceCodeInfo{
		Location: []*descriptorpb.SourceCodeInfo_Location{{
			Path:             []int32{4, 0},
			Span:             []int32{2, 0, 3, 1},
			TrailingComments: proto.String(" trailing\n"),
		}},
	}

	manifest := buildManifest("binary", defaultBundleName, []*descriptorpb.FileDescriptorProto{file}, scanStats{Candidates: 1}, flattenMetadata{Package: "example", Syntax: "proto2"})
	if len(manifest.MissingDependencies) != 1 || manifest.MissingDependencies[0] != "missing/types.proto" {
		t.Fatalf("unexpected missing dependencies: %v", manifest.MissingDependencies)
	}
	if manifest.FilesWithComments != 1 || manifest.CommentLocationCount != 1 {
		t.Fatalf("unexpected comment coverage: files=%d locations=%d", manifest.FilesWithComments, manifest.CommentLocationCount)
	}
	if !manifest.BundleIsCompilableAsOne {
		t.Fatal("flattened bundle must be marked compilable")
	}
}

func findMessage(t *testing.T, file *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	t.Helper()
	for _, message := range file.GetMessageType() {
		if message.GetName() == name {
			return message
		}
	}
	t.Fatalf("message %q not found", name)
	return nil
}

func testFile(name, pkg, syntax, messageName string) *descriptorpb.FileDescriptorProto {
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(name),
		Package: proto.String(pkg),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String(messageName),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("value"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				JsonName: proto.String("value"),
			}},
		}},
	}
	if syntax != "" {
		file.Syntax = proto.String(syntax)
	}
	return file
}

func writeDescriptorForTest(t *testing.T, out *bytes.Buffer, file *descriptorpb.FileDescriptorProto) {
	t.Helper()
	data, err := proto.Marshal(file)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	out.Write(data)
}

func TestIndividualProtoCanBeReconstructed(t *testing.T) {
	file := testFile("example/simple.proto", "example", "proto3", "Simple")
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("new file: %v", err)
	}
	var source bytes.Buffer
	printer := protoprint.Printer{}
	if err := printer.PrintProtoFile(fd, &source); err != nil {
		t.Fatalf("print proto: %v", err)
	}
	if !strings.Contains(source.String(), "message Simple") {
		t.Fatalf("unexpected proto source:\n%s", source.String())
	}
}
