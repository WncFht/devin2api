// 本文件验证服务入口能向调用方返回服务器启动失败。
package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// TestListenURL verifies listen address descriptions used in the startup log.
func TestListenURL(t *testing.T) {
	cases := map[string]string{
		":8080":          ":8080 (http://localhost:8080)",
		"0.0.0.0:8080":   "0.0.0.0:8080 (http://localhost:8080)",
		"127.0.0.1:9090": "http://127.0.0.1:9090",
		"[::]:8080":      "[::]:8080 (http://localhost:8080)",
		"invalid":        "invalid",
	}
	for listen, want := range cases {
		if got := listenURL(listen); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

// TestReusePortProvenance verifies the reuseport admission check accepts the
// three managed-service provenance envs and refuses bare invocations — the
// guard exists because SO_REUSEPORT join is silent (no EADDRINUSE), so a
// rogue/manual process with the env would otherwise shadow the real instance.
func TestReusePortProvenance(t *testing.T) {
	for _, env := range []string{"INVOCATION_ID", "DEVIN2API_HANDOFF", "DEVIN2API_MANAGED"} {
		t.Setenv(env, "")
	}
	if reusePortProvenance() {
		t.Error("reusePortProvenance() = true with no provenance env")
	}
	for _, env := range []string{"INVOCATION_ID", "DEVIN2API_HANDOFF", "DEVIN2API_MANAGED"} {
		t.Setenv(env, "x")
		if !reusePortProvenance() {
			t.Errorf("reusePortProvenance() = false with %s set", env)
		}
		t.Setenv(env, "")
	}
}

// TestRunReturnsServeError verifies unexpected server failures are returned to main.
func TestRunReturnsServeError(t *testing.T) {
	// 已关闭的 listener 让 Serve 立即返回错误——无需伪造 server。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	devinAdapter, err := devin.New(devin.Config{Endpoint: devin.Endpoint{BaseURL: "https://example.com"}, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	application := app.New(devinAdapter, config.ServerConfig{},
		debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}, nil))
	if err := run(context.Background(), application, &http.Server{}, listener, time.Second); err == nil {
		t.Fatal("run() error = nil, want serve error")
	}
}

// TestReloadRuntimeConfigRejectsEmptyUpstream verifies a live adapter refuses a
// reload that drops devin.model/base_url — committing empty values would fail
// every request. accounts 刻意不在必填集：补号的通道正是 /admin/accounts
// 与本端点，空账号集是合法空池（全部 lane 摘出）而非配置事故。
func TestReloadRuntimeConfigRejectsEmptyUpstream(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	valid := "server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n  accounts:\n    - {name: a, token: 't'}\n"
	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	manager := debuglog.NewManager(dir, debuglog.RetentionPolicy{}, nil)
	defer manager.Close()
	dbStore, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbStore.Close() }()
	devinPool, err := devin.NewPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := accounts.New(configPath, dir, dbStore, devinPool)
	rt.CommitConfig(prev)
	if _, err := rt.Apply(context.Background(), prev, nil); err != nil {
		t.Fatal(err)
	}
	application := app.New(devinPool, config.ServerConfig{}, manager)
	panel, err := ccpanel.New(ccpanel.Deps{Password: "pw", BaseURL: "https://example.com", TokenFunc: func() string { return "t" }, Debug: manager})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := ccpanel.NewPanelSettings(dbStore, ccpanel.SettingsDeps{
		Debug:       manager,
		DevinConfig: devinPool.CurrentConfig,
		UpdateDevin: func(mutate func(*devin.Config) error) error {
			_, err := devinPool.UpdateConfig(mutate)
			return err
		},
		MaxConcurrency:    application.MaxConcurrency,
		SetMaxConcurrency: application.SetMaxConcurrency,
		QuotaInterval:     panel.QuotaInterval,
		SetQuotaInterval:  panel.SetQuotaInterval,
		PprofListen:       currentPprofListen,
		SetPprofListen:    rebindPprof,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 丢 model 整单拒绝。
	if err := os.WriteFile(configPath, []byte("server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  accounts:\n    - {name: a, token: 't'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(rt, application, panel, manager, settings); err == nil {
		t.Fatal("reloadRuntimeConfig() error = nil, want non-empty validation error")
	}

	// 丢 accounts 允许热应用：合法空池，lane 名集变化计入 applied。
	if err := os.WriteFile(configPath, []byte("server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := reloadRuntimeConfig(rt, application, panel, manager, settings)
	if err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
	if !slices.Contains(report.Applied, "devin.accounts") {
		t.Fatalf("devin.accounts missing from applied: %v", report.Applied)
	}
	if lanes := devinPool.Snapshot().Accounts; len(lanes) != 0 {
		t.Fatalf("pool lanes = %v, want empty", lanes)
	}

	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(rt, application, panel, manager, settings); err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
	if lanes := devinPool.Snapshot().Accounts; len(lanes) != 1 {
		t.Fatalf("pool lanes = %v, want {a}", lanes)
	}

	// 兜底服役的恢复半程：healthz 的 config_last_good 必须随 reload 成功
	// 复位——否则 /admin/config 已报恢复文件服役而探活仍判降级，两面矛盾。
	application.SetServingLastGoodConfig(true)
	if _, err := reloadRuntimeConfig(rt, application, panel, manager, settings); err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
	recorder := httptest.NewRecorder()
	application.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var hz map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &hz); err != nil {
		t.Fatal(err)
	}
	if hz["config_last_good"] != false {
		t.Fatalf("config_last_good = %v, want false after reload recovery", hz["config_last_good"])
	}
}

// TestReloadClassifiesEveryConfigField 钉住「reload 分类无兜底」的契约：
// 反射枚举 config.Config 的全部 yaml 叶子字段，逐字段变异后走真实
// reloadRuntimeConfig——任一字段既不在 applied 也不在 requires_restart，
// 测试即失败。新增配置项漏接热更链时第一时间暴露，而不是静默躺在
// /admin/config 里显示已生效。
func TestReloadClassifiesEveryConfigField(t *testing.T) {
	baseYAML := `server:
  listen: '127.0.0.1:1'
  max_concurrency: 64
devin:
  base_url: 'https://example.com'
  model: 'm'
  accounts:
    - {name: a, token: 'tok'}
  proxy: 'http://127.0.0.1:7890'
  force_http1: false
  aliases: {a: b}
  client_name: 'chisel'
  client_version: '1.0'
  client_os: 'mac'
  max_rpm: 12
  gate_max_hold_seconds: 9
  gate_drip_interval_seconds: 4
  gate_default_latch_seconds: 30
  gate_window_offset_seconds: 1
  gate_window_guard_seconds: 3
debug:
  enabled: true
  retention_days: 7
  max_total_mb: 256
  payload_hours: 12
  keep_error_dirs: 5
  quota_interval_minutes: 6
  pprof_listen: '127.0.0.1:0'
dashboard:
  password: 'pw'
`
	// 聚合上报名：retention 四个字段在报告里合并为一条 debug.retention。
	reported := map[string]string{
		"debug.retention_days":  "debug.retention",
		"debug.max_total_mb":    "debug.retention",
		"debug.payload_hours":   "debug.retention",
		"debug.keep_error_dirs": "debug.retention",
	}
	type leaf struct {
		path  string
		index []int
	}
	var leaves []leaf
	var walk func(typ reflect.Type, prefix string, index []int)
	walk = func(typ reflect.Type, prefix string, index []int) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			path := prefix + tag
			if path == "devin.token" || path == "auth.api_key" {
				// devin.token 与 auth.api_key 是迁移错误占位壳：任何非空
				// 值在 Load 期即拒绝，不存在「合法变异后可热应用」的形态，
				// 不进枚举。
				continue
			}
			fieldType := field.Type
			if fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if fieldType.Kind() == reflect.Struct {
				walk(fieldType, path+".", append(index, i))
				continue
			}
			leaves = append(leaves, leaf{path: path, index: append(index, i)})
		}
	}
	walk(reflect.TypeOf(config.Config{}), "", nil)

	for _, lf := range leaves {
		t.Run(lf.path, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(baseYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			prev, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}

			// 变异 yaml 树上该叶子：值取与 prev 不同的形态。
			var tree map[string]any
			if err := yaml.Unmarshal([]byte(baseYAML), &tree); err != nil {
				t.Fatal(err)
			}
			segments := strings.Split(lf.path, ".")
			node := tree
			for _, seg := range segments[:len(segments)-1] {
				next, _ := node[seg].(map[string]any)
				if next == nil {
					next = map[string]any{}
					node[seg] = next
				}
				node = next
			}
			current := reflect.ValueOf(prev)
			for _, i := range lf.index {
				current = current.Field(i)
				if current.Kind() == reflect.Pointer {
					current = current.Elem()
				}
			}
			// 通用 +"-mutated" 规则对个别字段会产出非法形态：proxy 进
			// transport 构建（非法即整单拒绝——validate-then-commit 的
			// 正确行为，测试要的是「变了但合法」），给它固定合法替代值。
			mutateOverride := map[string]any{
				"devin.proxy": "http://127.0.0.2:7890",
			}
			if override, ok := mutateOverride[lf.path]; ok {
				node[segments[len(segments)-1]] = override
			} else if lf.path == "devin.accounts" {
				// 变异成另一组名集：lane 名序变化单独上报
				// devin.accounts（增删不进单 lane 字段差集）。
				node[segments[len(segments)-1]] = []any{map[string]any{"name": "pool-a", "token": "tok2"}}
			} else {
				switch current.Kind() {
				case reflect.Bool:
					node[segments[len(segments)-1]] = !current.Bool()
				case reflect.Int, reflect.Int64:
					node[segments[len(segments)-1]] = current.Int() + 7
				case reflect.Float64:
					node[segments[len(segments)-1]] = current.Float() + 0.5
				case reflect.Slice:
					node[segments[len(segments)-1]] = []string{"mutated"}
				case reflect.Map:
					node[segments[len(segments)-1]] = map[string]any{"mutated": "yes"}
				default:
					node[segments[len(segments)-1]] = current.String() + "-mutated"
				}
			}
			mutated, err := yaml.Marshal(tree)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}

			manager := debuglog.NewManager(dir, debuglog.RetentionPolicy{}, nil)
			defer manager.Close()
			dbStore, err := store.Open(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dbStore.Close() }()
			devinPool, err := devin.NewPool(nil)
			if err != nil {
				t.Fatal(err)
			}
			rt := accounts.New(configPath, dir, dbStore, devinPool)
			rt.CommitConfig(prev)
			if _, err := rt.Apply(context.Background(), prev, nil); err != nil {
				t.Fatal(err)
			}
			application := app.New(devinPool, config.ServerConfig{}, manager)
			panel, err := ccpanel.New(ccpanel.Deps{Password: "pw", BaseURL: "https://example.com", TokenFunc: func() string { return "t" }, Debug: manager})
			if err != nil {
				t.Fatal(err)
			}
			settings, err := ccpanel.NewPanelSettings(dbStore, ccpanel.SettingsDeps{
				Debug:       manager,
				DevinConfig: devinPool.CurrentConfig,
				UpdateDevin: func(mutate func(*devin.Config) error) error {
					_, err := devinPool.UpdateConfig(mutate)
					return err
				},
				MaxConcurrency:    application.MaxConcurrency,
				SetMaxConcurrency: application.SetMaxConcurrency,
				QuotaInterval:     panel.QuotaInterval,
				SetQuotaInterval:  panel.SetQuotaInterval,
				PprofListen:       currentPprofListen,
				SetPprofListen:    rebindPprof,
			})
			if err != nil {
				t.Fatal(err)
			}
			report, err := reloadRuntimeConfig(rt, application, panel, manager, settings)
			if err != nil {
				t.Fatalf("reloadRuntimeConfig() error = %v", err)
			}
			name := lf.path
			if alias, ok := reported[lf.path]; ok {
				name = alias
			}
			if !slices.Contains(report.Applied, name) && !slices.Contains(report.RequiresRestart, name) {
				t.Fatalf("%s changed but reported nowhere: applied=%v requires_restart=%v",
					lf.path, report.Applied, report.RequiresRestart)
			}
		})
	}
}

// TestLoadBootConfigWritesCache 钉住成功加载路径：文件值服役
// （lastGood 为空）且 last-good 缓存落盘——它是后续崩溃重启的兜底本钱。
func TestLoadBootConfigWritesCache(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath,
		[]byte("server:\n  listen: ':3033'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n  accounts:\n    - {name: a, token: 't'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, lastGood, err := loadBootConfig(configPath, dir, filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatalf("loadBootConfig: %v", err)
	}
	if lastGood != nil {
		t.Fatalf("lastGood = %+v, want nil (file config served)", lastGood)
	}
	if cfg.Server.Listen != ":3033" {
		t.Fatalf("Listen = %q", cfg.Server.Listen)
	}
	if _, err := config.ReadLastGood(dir); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

// TestLoadBootConfigMissingNoCache 钉住兜底缺席语义：文件加载失败且
// 状态目录无缓存——错误照旧上交，首装机器不会出现「凭空服役」。
func TestLoadBootConfigMissingNoCache(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := loadBootConfig(filepath.Join(dir, "gone.yaml"), dir, filepath.Join(dir, "logs")); err == nil {
		t.Fatal("loadBootConfig() error = nil, want load failure")
	}
}

// TestLoadBootConfigServesLastGood 复刻 09-18 事故形态并钉住兜底契约：
// credentials_file 账号的配置成功加载并写缓存 → credentials.toml 与
// config.yaml 双双消失 → boot 加载失败转服役缓存投影（token 已物化、
// credentials_file 已摘除，Apply 的重校验不会在同一处再死）→
// config-fallback.json 标记落盘。
func TestLoadBootConfigServesLastGood(t *testing.T) {
	dir := t.TempDir()
	logRoot := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	credsPath := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(credsPath, []byte("windsurf_api_key = \"tok-file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath,
		[]byte("server:\n  listen: ':3033'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n  accounts:\n    - {name: a, credentials_file: 'credentials.toml'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadBootConfig(configPath, dir, logRoot); err != nil {
		t.Fatalf("prime loadBootConfig: %v", err)
	}
	// 事故：引用凭据文件与配置本体同时消失。
	for _, p := range []string{credsPath, configPath} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	cfg, lastGood, err := loadBootConfig(configPath, dir, logRoot)
	if err != nil {
		t.Fatalf("fallback loadBootConfig: %v", err)
	}
	if lastGood == nil {
		t.Fatal("lastGood = nil, want cached config served")
	}
	if cfg.Server.Listen != ":3033" || cfg.Devin.Model != "m" {
		t.Fatalf("cached cfg = listen %q model %q", cfg.Server.Listen, cfg.Devin.Model)
	}
	acc := cfg.Devin.Accounts[0]
	if acc.Token != "tok-file" || acc.CredentialsFile != "" {
		t.Fatalf("cached account = {token:%q file:%q}, want {token:tok-file file:\"\"}", acc.Token, acc.CredentialsFile)
	}
	// 兜底投影必须过 Apply 的整表重校验——这是事故链上真正的死因。
	if _, err := config.ResolveAccounts(cfg.Devin.Accounts, dir); err != nil {
		t.Fatalf("cached accounts fail Apply-time re-validation: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(logRoot, "config-fallback.json"))
	if err != nil {
		t.Fatalf("fallback marker missing: %v", err)
	}
	var marker forensicMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Count != 1 || marker.Reason == "" || marker.CachedAt == "" {
		t.Fatalf("marker = %+v, want count=1 with reason/cached_at", marker)
	}
}

// TestLoadBootConfigRecoveredMarksEpisode 钉住恢复告警记账：兜底期后
// 第一次成功加载要把 marker 的 recovered_at 钉上——事故复盘靠它划界。
func TestLoadBootConfigRecoveredMarksEpisode(t *testing.T) {
	dir := t.TempDir()
	logRoot := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	good := []byte("server:\n  listen: ':3033'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n")
	if err := os.WriteFile(configPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadBootConfig(configPath, dir, logRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{{{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, lastGood, err := loadBootConfig(configPath, dir, logRoot); err != nil || lastGood == nil {
		t.Fatalf("fallback: cfg err=%v lastGood=%v", err, lastGood)
	}
	if err := os.WriteFile(configPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, lastGood, err := loadBootConfig(configPath, dir, logRoot); err != nil || lastGood != nil {
		t.Fatalf("recovered load: err=%v lastGood=%v", err, lastGood)
	}
	raw, err := os.ReadFile(filepath.Join(logRoot, "config-fallback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker forensicMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.RecoveredAt == "" {
		t.Fatalf("RecoveredAt empty, marker = %+v", marker)
	}
}

// drainBlockAdapter 的 Stream 挂起直到 ctx 取消——模拟上游长流中的在途
// 请求，让 drain 超时强掐路径有目标可打。
type drainBlockAdapter struct{ entered chan struct{} }

func (b *drainBlockAdapter) Stream(ctx context.Context, _ llm.RequestMessages) (llm.ResponseStream, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *drainBlockAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return nil, nil
}

// TestDrainTimeoutKillsInflightWithBookkeeping 走真实 SIGTERM-mid-stream
// 路径：drainTimeout 缩短触发强掐，断言被掐请求落 logs 行
// （result=aborted、error_stage=drain_timeout）与 error.json——
// server.Close 不等 handler 协程，缺收尾等待时这些行随进程退出丢失。
func TestDrainTimeoutKillsInflightWithBookkeeping(t *testing.T) {

	dir := t.TempDir()
	dbStore, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbStore.Close() }()
	manager := debuglog.NewManager(filepath.Join(dir, "logs"), debuglog.RetentionPolicy{}, dbStore)
	fake := &drainBlockAdapter{entered: make(chan struct{})}
	application := app.New(fake, config.ServerConfig{Listen: "127.0.0.1:0"}, manager)
	server := application.HTTPServer()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- run(ctx, application, server, listener, 200*time.Millisecond) }()

	// 真实 conn 上的在途流式请求：server.Close 的强掐走完整传输路径。
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := http.Post("http://"+listener.Addr().String()+"/v1/responses", "application/json",
			strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	<-fake.entered
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("run() = %v", err)
	}
	<-clientDone
	// 复刻进程退出序：run 返回后 deferred Close 排空写队列，哨兵收尾落库。
	manager.Close()

	rows, _, err := dbStore.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("logs rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Result != "aborted" || row.ErrorStage != debuglog.ErrStageDrainTimeout {
		t.Fatalf("log row = result %q stage %q, want aborted/drain_timeout", row.Result, row.ErrorStage)
	}
	data, _, ok, err := dbStore.DebugFile(context.Background(), row.Dir, debuglog.ErrorFile, 1<<20)
	if err != nil || !ok {
		t.Fatalf("error.json missing: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(data), debuglog.ErrStageDrainTimeout) {
		t.Fatalf("error.json = %s, want drain_timeout stage", data)
	}
}
