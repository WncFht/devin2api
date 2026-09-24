module github.com/WncFht/devin2api

go 1.27.1

require (
	connectrpc.com/connect v1.21.0
	github.com/felixge/fgprof v0.9.5
	github.com/go-chi/chi/v5 v5.3.2
	github.com/gorilla/websocket v1.5.3
	github.com/jhump/protoreflect/v2 v2.0.0-beta.2
	github.com/klauspost/compress v1.20.0
	github.com/restic/chunker v0.5.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
	local/devinproto v0.0.0
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/giraffesyo/pdf v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20260802141513-ef3492d7dac3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace local/devinproto => ./outputs/devin-proto-go
