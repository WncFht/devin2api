// 本文件验证服务入口能向调用方返回服务器启动失败。
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
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
	if err := run(context.Background(), application, &http.Server{}, listener); err == nil {
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
	rt := accounts.New(configPath, dbStore, devinPool)
	rt.CommitConfig(prev)
	if _, _, err := rt.Apply(context.Background(), prev, nil); err != nil {
		t.Fatal(err)
	}
	application := app.New(devinPool, config.ServerConfig{}, manager)
	panel, err := ccpanel.New("pw", "https://example.com", func() string { return "t" }, "", false, nil, manager)
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
	tokenStore, err := authtoken.New(dbStore)
	if err != nil {
		t.Fatal(err)
	}

	// 丢 model 整单拒绝。
	if err := os.WriteFile(configPath, []byte("server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  accounts:\n    - {name: a, token: 't'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(rt, application, panel, manager, settings, tokenStore); err == nil {
		t.Fatal("reloadRuntimeConfig() error = nil, want non-empty validation error")
	}

	// 丢 accounts 允许热应用：合法空池，lane 名集变化计入 applied。
	if err := os.WriteFile(configPath, []byte("server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := reloadRuntimeConfig(rt, application, panel, manager, settings, tokenStore)
	if err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
	if !slices.Contains(report.Applied, "devin.accounts") {
		t.Fatalf("devin.accounts missing from applied: %v", report.Applied)
	}
	if lanes := devinPool.AccountLaneStates(); len(lanes) != 0 {
		t.Fatalf("pool lanes = %v, want empty", lanes)
	}

	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(rt, application, panel, manager, settings, tokenStore); err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
	if lanes := devinPool.AccountLaneStates(); len(lanes) != 1 {
		t.Fatalf("pool lanes = %v, want {a}", lanes)
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
auth:
  api_key: 'k'
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
			if path == "devin.token" {
				// devin.token 是迁移错误占位壳：任何非空值在 Load 期
				// 即拒绝，不存在「合法变异后可热应用」的形态，不进枚举。
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
			rt := accounts.New(configPath, dbStore, devinPool)
			rt.CommitConfig(prev)
			if _, _, err := rt.Apply(context.Background(), prev, nil); err != nil {
				t.Fatal(err)
			}
			application := app.New(devinPool, config.ServerConfig{}, manager)
			panel, err := ccpanel.New("pw", "https://example.com", func() string { return "t" }, "", false, nil, manager)
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
			tokenStore, err := authtoken.New(dbStore)
			if err != nil {
				t.Fatal(err)
			}
			report, err := reloadRuntimeConfig(rt, application, panel, manager, settings, tokenStore)
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
