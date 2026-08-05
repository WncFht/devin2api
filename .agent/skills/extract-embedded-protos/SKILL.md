---
name: extract-embedded-protos
description: Recover all embedded FileDescriptorProto values from a compiled Go binary, preserve the original descriptor set, and flatten every recovered protobuf package into one searchable and protoc-compilable .proto file. Use when inspecting Devin, Windsurf, Codeium, or another compiled Go application for protobuf/gRPC schemas; locating RPC requests, messages, system prompts, tool definitions, field numbers, or service streaming contracts; or replacing guessed reverse-engineered structs with descriptor-backed definitions.
---

# Extract Embedded Protos

Recover protobuf schemas from a binary with the bundled deterministic Go extractor. Treat the original descriptor set and the flattened source as different evidence surfaces.

## Run the extractor

1. Resolve the exact compiled binary to inspect. Require a regular file.
2. Choose a dedicated output directory. The extractor deletes that directory completely before every run; never point it at a workspace root or a directory containing unrelated files.
3. From the project root, run the Go command with exactly two positional arguments:

```sh
go run ./cmd/protoextract \
  /absolute/path/to/source-binary \
  /absolute/path/to/output-directory
```

The first build requires Go and may need network access for module dependencies. Do not add flags or compatibility modes to the extractor CLI.

## Understand the outputs

Expect exactly these files in a fresh output directory:

- `descriptors.pb`: lossless recovered `FileDescriptorSet`. Use this as the authority for original file names, packages, service paths, syntax, options, field names, and field numbers.
- `all-protos.proto`: all recovered declarations in one compilable package. Use this for search, reading, and code generation experiments.
- `manifest.json`: extraction counts, missing dependencies, comment coverage, caveats, and original-to-flattened symbol mappings.

The flattened file retains `exa.api_server_pb` when present. It prefixes non-root symbols, rewrites all message/enum/service/extension references, renames colliding enum values, and uses proto2 syntax to carry mixed proto2/proto3 definitions. It preserves known-field wire structure, including `required`, maps, oneofs, extension ranges, enum numbers, field numbers, and packed encoding.

Do not treat flattened names as original protocol names. Non-root generated type names and service RPC paths change. Proto3 presence and open-enum APIs cannot be represented exactly in the proto2 view.

## Validate every extraction

Read `manifest.json` and report:

- descriptor, candidate, and duplicate counts;
- missing dependencies;
- comment locations and files with comments;
- selected flattened package and syntax.

Compile the flattened file when `protoc` is available without retaining a check artifact:

```sh
protoc \
  --proto_path=/absolute/path/to/output-directory \
  --descriptor_set_out=/dev/null \
  /absolute/path/to/output-directory/all-protos.proto
```

Run the project Go tests before modifying extractor behavior:

```sh
go test -race ./cmd/protoextract
go vet ./...
```

## Analyze a protocol

Search `all-protos.proto` by exact service, method, message, and field names. Then confirm original identities against `descriptors.pb` or `manifest.json` mappings before making claims about an RPC path.

For requests involving statefulness, messages, system prompts, or tools:

1. Record exact request and response field numbers and types.
2. Separate a prompt string, structured message history, IDs, and tool definitions instead of inferring semantics from guessed names.
3. Trace related HTTP/SSE calls separately; a schema alone cannot prove whether the server persists conversation state.
4. State the extraction boundary: only descriptors embedded in this binary are recoverable. Stripped, dynamically downloaded, or separately shipped schemas are absent.
5. Never invent comments. Original comments exist only when `SourceCodeInfo` survived compilation; use manifest counts as evidence.

If dependencies are missing or the flattened file fails compilation, preserve `descriptors.pb`, report the exact unresolved names, and inspect adjacent binaries or application resources before changing the extraction algorithm.
