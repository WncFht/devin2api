// 本文件验证 devin.accounts 账号池声明的加载期校验：残留 devin.token
// 产出迁移错误、name 合法性与唯一性（"default" 是普通名）、凭据来源
// 二选一、credentials.toml 解析与路径锚定、有效 token 去重；以及
// ResolveAccounts 的整表校验出口（API 干跑/reload 共用）。Load 测试
// 一律走真实入口，文件落 t.TempDir()。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile 写入测试用文件，失败即终止当前测试。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadWithDevin 把 devin 段内容拼进最小合法配置（仅补必填 server.listen），
// 写入 dir/config.yaml 后经真实 Load 加载。
func loadWithDevin(t *testing.T, dir, devinYAML string) (Config, error) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, "config.yaml"), "server:\n  listen: ':9090'\ndevin:\n"+devinYAML)
	return Load(filepath.Join(dir, "config.yaml"))
}

// TestLoadDevinTokenRemoved 钉住 devin.token 的迁移错误：字段语义已删除，
// yaml 键保留只为对残留值产出改写指引——只有 token 没有 accounts 的旧
// 单号配置启动必须报这条错，不能静默变空池；token 与 accounts 同现报
// 同一条错。纯空白 token 按未设置处理，不触发迁移错误。
func TestLoadDevinTokenRemoved(t *testing.T) {
	const want = "devin.token removed; declare devin.accounts"

	t.Run("token only", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(), "  token: tok-single\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Load() error = %v, want migration error containing %q", err, want)
		}
	})

	t.Run("token alongside accounts", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  token: tok-single\n  accounts:\n    - name: alpha\n      token: tok-a\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Load() error = %v, want migration error containing %q", err, want)
		}
	})

	t.Run("whitespace token is unset", func(t *testing.T) {
		if _, err := loadWithDevin(t, t.TempDir(),
			"  token: '   '\n  accounts:\n    - name: alpha\n      token: tok-a\n"); err != nil {
			t.Fatalf("Load() error = %v, want whitespace token treated as unset", err)
		}
	})
}

// TestLoadAccountsEmptySet 验证空账号池合法：accounts: [] 与 accounts 键
// 缺席都是空生效集；自动发现链已不进 load 路径——即便 DEVIN_TOKEN /
// WINDSURF_API_KEY 在环境里，加载结果也不带任何凭据。
func TestLoadAccountsEmptySet(t *testing.T) {
	t.Setenv("DEVIN_TOKEN", "tok-env")
	t.Setenv("WINDSURF_API_KEY", "tok-env2")

	t.Run("explicit empty list", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(), "  accounts: []\n")
		if err != nil {
			t.Fatalf("Load() error = %v, want empty pool to be legal", err)
		}
		if len(config.Devin.Accounts) != 0 {
			t.Fatalf("Accounts = %v, want empty", config.Devin.Accounts)
		}
		if config.Devin.Token != "" {
			t.Fatalf("Devin.Token = %q, want empty (discovery removed from load path)", config.Devin.Token)
		}
	})

	t.Run("accounts key absent", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(), "  model: swe-2\n")
		if err != nil {
			t.Fatalf("Load() error = %v, want empty pool to be legal", err)
		}
		if len(config.Devin.Accounts) != 0 || config.Devin.Token != "" {
			t.Fatalf("Devin = %+v, want no credentials resolved", config.Devin)
		}
	})
}

// TestLoadAccountsNameValidation 钉住账号 name 契约：必填、字符集
// [A-Za-z0-9_-]{1,32}、池内唯一；"default" 已解禁为普通账号名。
func TestLoadAccountsNameValidation(t *testing.T) {
	cases := []struct {
		name         string
		accountsYAML string
		wantErr      string
	}{
		{"missing name", "    - token: tok\n", "name must match"},
		{"blank name trims to empty", "    - name: '   '\n      token: tok\n", "name must match"},
		{"space in name", "    - name: 'a b'\n      token: tok\n", "name must match"},
		{"dot in name", "    - name: 'a.b'\n      token: tok\n", "name must match"},
		{"name over 32 chars", "    - name: '" + strings.Repeat("a", 33) + "'\n      token: tok\n", "name must match"},
		{"duplicate name", "    - name: alpha\n      token: tok-a\n    - name: alpha\n      token: tok-b\n", `duplicate name "alpha"`},
		{"valid charset", "    - name: 'alpha-1_B'\n      token: tok\n", ""},
		{"default is a normal name", "    - name: default\n      token: tok\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithDevin(t, t.TempDir(), "  accounts:\n"+tc.accountsYAML)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() error = %v, want success", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadAccountsCredentialSources 验证每个账号至少一种凭据来源：
// 三者皆缺报错；credentials_file 解不出记 LoadError 降级而非拒载
// （文件缺失与文件在但无键都落 LoadError，09-18 文件被删事故的
// 教训）；只给文件时 token 由文件播种；两者都给时 token 保持字面量、
// 文件留作自愈来源；只给 api_key 也可成号。
func TestLoadAccountsCredentialSources(t *testing.T) {
	t.Run("neither token nor file nor api_key", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(), "  accounts:\n    - name: alpha\n")
		if err == nil || !strings.Contains(err.Error(), "one of token/credentials_file/api_key is required") {
			t.Fatalf("Load() error = %v, want missing-credential error", err)
		}
	})

	t.Run("api_key only is a valid credential", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      api_key: cog_abc\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := config.Devin.Accounts[0].APIKey; got != "cog_abc" {
			t.Fatalf("APIKey = %q, want cog_abc", got)
		}
	})

	t.Run("duplicate api_key rejected", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      api_key: cog_same\n    - name: beta\n      api_key: cog_same\n")
		if err == nil || !strings.Contains(err.Error(), `api_key duplicates account "alpha"`) {
			t.Fatalf("Load() error = %v, want duplicate api_key error", err)
		}
	})

	// 文件缺席/无键不再拒载：LoadError 留证据、账号降级，进程继续
	// 加载——09-18 credentials.toml 被删→1922 次重启循环就是死在这里。
	t.Run("missing credentials file degrades", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(), "  accounts:\n    - name: alpha\n      credentials_file: 'missing.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v, want degraded load, not failure", err)
		}
		acc := config.Devin.Accounts[0]
		if acc.LoadError == "" || !strings.Contains(acc.LoadError, "no such file") {
			t.Fatalf("LoadError = %q, want file-miss evidence", acc.LoadError)
		}
		if !acc.Degraded() {
			t.Fatal("file-only account with unreadable file must be Degraded")
		}
	})

	t.Run("credentials file without key degrades", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "other_key = \"x\"\n")
		config, err := loadWithDevin(t, dir, "  accounts:\n    - name: alpha\n      credentials_file: 'creds.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v, want degraded load, not failure", err)
		}
		acc := config.Devin.Accounts[0]
		if !strings.Contains(acc.LoadError, "no windsurf_api_key") || !acc.Degraded() {
			t.Fatalf("LoadError = %q Degraded = %v", acc.LoadError, acc.Degraded())
		}
	})

	// 文件坏但有其它凭据在役：LoadError 证据记下，账号不降级——字面
	// token/api_key 照常服役，文件留作自愈源等回填。
	t.Run("missing file with literal token does not degrade", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      token: tok-lit\n      credentials_file: 'missing.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		acc := config.Devin.Accounts[0]
		if acc.LoadError == "" || acc.Degraded() || acc.Token != "tok-lit" {
			t.Fatalf("LoadError = %q Degraded = %v Token = %q", acc.LoadError, acc.Degraded(), acc.Token)
		}
	})

	t.Run("missing file with api_key does not degrade", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      api_key: cog_x\n      credentials_file: 'missing.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if acc := config.Devin.Accounts[0]; acc.LoadError == "" || acc.Degraded() {
			t.Fatalf("LoadError = %q Degraded = %v", acc.LoadError, acc.Degraded())
		}
	})

	// 全号皆降级也是合法加载：进程起来服务管理面，死 lane 标记冷却
	// 等文件回填——好过 systemd 重启空转。
	t.Run("all accounts degraded still loads", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      credentials_file: 'a.toml'\n    - name: beta\n      credentials_file: 'b.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		for _, acc := range config.Devin.Accounts {
			if !acc.Degraded() {
				t.Fatalf("account %q not degraded: %+v", acc.Name, acc)
			}
		}
	})

	t.Run("credentials file seeds token", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-xyz\"\n")
		config, err := loadWithDevin(t, dir, "  accounts:\n    - name: alpha\n      credentials_file: 'creds.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := config.Devin.Accounts[0].Token; got != "tok-xyz" {
			t.Fatalf("Token = %q, want seeded tok-xyz", got)
		}
	})

	t.Run("literal token wins over file", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-file\"\n")
		config, err := loadWithDevin(t, dir,
			"  accounts:\n    - name: alpha\n      token: tok-literal\n      credentials_file: 'creds.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := config.Devin.Accounts[0].Token; got != "tok-literal" {
			t.Fatalf("Token = %q, want literal tok-literal", got)
		}
	})
}

// TestLoadAccountsDedup 验证池内去重发生在有效身份上：字面 token 相同、
// credentials_file 路径相同、或一个条目的字面 token 与另一条目文件解出的
// token 相同，都算同一账号进池两次，加载期拒绝。去重前先 trim。
func TestLoadAccountsDedup(t *testing.T) {
	t.Run("duplicate literal token", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      token: tok-same\n    - name: beta\n      token: tok-same\n")
		if err == nil || !strings.Contains(err.Error(), `token duplicates account "alpha"`) {
			t.Fatalf("Load() error = %v, want duplicate token error", err)
		}
	})

	t.Run("duplicate credentials file", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-f\"\n")
		_, err := loadWithDevin(t, dir,
			"  accounts:\n    - name: alpha\n      credentials_file: 'creds.toml'\n    - name: beta\n      credentials_file: 'creds.toml'\n")
		if err == nil || !strings.Contains(err.Error(), `credentials_file already used by account "alpha"`) {
			t.Fatalf("Load() error = %v, want duplicate file error", err)
		}
	})

	t.Run("literal token collides with file token", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-shared\"\n")
		_, err := loadWithDevin(t, dir,
			"  accounts:\n    - name: alpha\n      token: tok-shared\n    - name: beta\n      credentials_file: 'creds.toml'\n")
		if err == nil || !strings.Contains(err.Error(), `token duplicates account "alpha"`) {
			t.Fatalf("Load() error = %v, want effective-token dedup error", err)
		}
	})

	t.Run("token dedup applies after trim", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      token: ' tok '\n    - name: beta\n      token: tok\n")
		if err == nil || !strings.Contains(err.Error(), "token duplicates") {
			t.Fatalf("Load() error = %v, want trimmed duplicate token error", err)
		}
	})
}

// TestLoadAccountsCredentialsFilePathResolution 验证 credentials_file 的
// 路径解析：相对路径锚定到配置文件所在目录而非进程 CWD（launchd 下
// CWD=/），开头 ~/ 按用户主目录展开，落库的是清理后的绝对路径。
func TestLoadAccountsCredentialsFilePathResolution(t *testing.T) {
	t.Chdir(t.TempDir()) // 挪走进程 CWD，证明相对路径锚定不依赖它。

	t.Run("relative anchors to config dir", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-a\"\n")
		config, err := loadWithDevin(t, dir, "  accounts:\n    - name: alpha\n      credentials_file: 'creds.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		got := config.Devin.Accounts[0].CredentialsFile
		if want := filepath.Join(dir, "creds.toml"); got != want {
			t.Fatalf("CredentialsFile = %q, want anchored %q", got, want)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("CredentialsFile = %q, want absolute path", got)
		}
	})

	t.Run("tilde expands via HOME", func(t *testing.T) {
		dir := t.TempDir()
		// os.UserHomeDir 在 unix 读 HOME、windows 读 USERPROFILE，两个都设
		t.Setenv("HOME", dir)
		t.Setenv("USERPROFILE", dir)
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-tilde\"\n")
		config, err := loadWithDevin(t, dir, "  accounts:\n    - name: alpha\n      credentials_file: '~/creds.toml'\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got, want := config.Devin.Accounts[0].CredentialsFile, filepath.Join(dir, "creds.toml"); got != want {
			t.Fatalf("CredentialsFile = %q, want %q", got, want)
		}
	})
}

// TestResolveAccounts 验证导出的整表校验面：规则与 Load 内嵌同源，
// 返回的是 trim/锚定/文件 token 回写后的副本，入参不被改动；空集与
// "default" 名都合法；校验错误原样透传。
func TestResolveAccounts(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "creds.toml"), "windsurf_api_key = \"tok-file\"\n")

	t.Run("resolves copy without mutating input", func(t *testing.T) {
		in := []DevinAccountConfig{
			{Name: "  alpha  ", CredentialsFile: "creds.toml"},
			{Name: "default", Token: " tok-b "},
		}
		got, err := ResolveAccounts(in, dir)
		if err != nil {
			t.Fatalf("ResolveAccounts() error = %v", err)
		}
		wantFile := filepath.Join(dir, "creds.toml")
		if got[0].Name != "alpha" || got[0].Token != "tok-file" || got[0].CredentialsFile != wantFile {
			t.Fatalf("resolved[0] = %+v, want {alpha tok-file %s}", got[0], wantFile)
		}
		if got[1].Name != "default" || got[1].Token != "tok-b" {
			t.Fatalf("resolved[1] = %+v, want {default tok-b} (default unreserved)", got[1])
		}
		if in[0].Name != "  alpha  " || in[0].Token != "" || in[0].CredentialsFile != "creds.toml" {
			t.Fatalf("input mutated: %+v", in[0])
		}
		if in[1].Token != " tok-b " {
			t.Fatalf("input mutated: %+v", in[1])
		}
	})

	t.Run("empty set is legal", func(t *testing.T) {
		got, err := ResolveAccounts(nil, dir)
		if err != nil || len(got) != 0 {
			t.Fatalf("ResolveAccounts(nil) = %v, %v; want empty, nil", got, err)
		}
	})

	t.Run("validation errors propagate", func(t *testing.T) {
		_, err := ResolveAccounts([]DevinAccountConfig{
			{Name: "alpha", Token: "tok-same"},
			{Name: "beta", Token: "tok-same"},
		}, dir)
		if err == nil || !strings.Contains(err.Error(), "token duplicates") {
			t.Fatalf("ResolveAccounts() error = %v, want duplicate token error", err)
		}
	})
}

// TestExpandHomeDir 验证 ~/ 展开：仅开头的 "~/"+rest 展开为用户主目录；
// 裸 "~"、"~user"、绝对路径与相对路径原样返回。
func TestExpandHomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	cases := []struct{ in, want string }{
		{"~/x", filepath.Join(home, "x")},
		{"~/.local/share/devin/credentials.toml", filepath.Join(home, ".local", "share", "devin", "credentials.toml")},
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
		{"~", "~"},
		{"~other/x", "~other/x"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := expandHomeDir(tc.in); got != tc.want {
			t.Fatalf("expandHomeDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoadAccountsTrims 验证 name/token 入库前 trim：裁剪发生在校验与
// 去重之前，"  alpha  " 落成 "alpha"，"  tok  " 落成 "tok"。
func TestLoadAccountsTrims(t *testing.T) {
	config, err := loadWithDevin(t, t.TempDir(),
		"  accounts:\n    - name: '  alpha  '\n      token: '  tok  '\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	account := config.Devin.Accounts[0]
	if account.Name != "alpha" {
		t.Fatalf("Name = %q, want trimmed alpha", account.Name)
	}
	if account.Token != "tok" {
		t.Fatalf("Token = %q, want trimmed tok", account.Token)
	}
}

// TestLoadAccountsMetaFields 验证 priority/max_rpm 号级元数据与两个
// 全局池字段的加载：非负值原样进 DevinAccountConfig/DevinConfig，
// 负值逐字段在加载期拒绝。
func TestLoadAccountsMetaFields(t *testing.T) {
	t.Run("fields load", func(t *testing.T) {
		config, err := loadWithDevin(t, t.TempDir(),
			"  session_affinity_ttl_seconds: 600\n  quota_low_threshold_percent: 20\n"+
				"  accounts:\n    - name: alpha\n      token: tok\n      priority: 5\n      max_rpm: 30\n")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		account := config.Devin.Accounts[0]
		if account.Priority != 5 || account.MaxRPM != 30 {
			t.Fatalf("account = %+v", account)
		}
		if config.Devin.SessionAffinityTTLSeconds != 600 || config.Devin.QuotaLowThresholdPercent != 20 {
			t.Fatalf("globals = %+v", config.Devin)
		}
	})

	t.Run("negative priority", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      token: tok\n      priority: -1\n")
		if err == nil || !strings.Contains(err.Error(), "priority must be >= 0") {
			t.Fatalf("Load() error = %v, want priority error", err)
		}
	})

	t.Run("negative max_rpm", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(),
			"  accounts:\n    - name: alpha\n      token: tok\n      max_rpm: -5\n")
		if err == nil || !strings.Contains(err.Error(), "max_rpm must be >= 0") {
			t.Fatalf("Load() error = %v, want max_rpm error", err)
		}
	})
}
