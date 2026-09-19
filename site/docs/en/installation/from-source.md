# From Source

The generated proto bindings are committed under `outputs/devin-proto-go`, so a clone builds with no extra codegen toolchain — you need only Go.

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
go run ./cmd/devin-2api -config config.yaml
```

Or produce a binary:

```bash
go build -o devin-2api ./cmd/devin-2api
./devin-2api -config config.yaml
```

The same [path resolution](/installation/prebuilt-binary#path-resolution) applies. For a managed install that still uses your own build, run the [deploy scripts](/installation/deploy-scripts) without `--release` — they build the working tree and install the result.
