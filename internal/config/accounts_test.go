// 本文件验证 devin.accounts 账号池声明的加载期校验：与 devin.token 互斥、
// name 合法性与唯一性、凭据来源二选一、credentials.toml 解析与路径锚定、
// 有效 token 去重。测试一律走真实 Load 入口，文件落 t.TempDir()。
package config

import (
	"errors"
	"io/fs"
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

// TestLoadAccountsMutualExclusion 验证 devin.token 与非空 devin.accounts
// 互斥；纯空白 token 视作未设置，不触发互斥。
func TestLoadAccountsMutualExclusion(t *testing.T) {
	_, err := loadWithDevin(t, t.TempDir(),
		"  token: tok-single\n  accounts:\n    - name: alpha\n      token: tok-a\n")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Load() error = %v, want mutual exclusion error", err)
	}

	if _, err := loadWithDevin(t, t.TempDir(),
		"  token: '   '\n  accounts:\n    - name: alpha\n      token: tok-a\n"); err != nil {
		t.Fatalf("Load() error = %v, want whitespace token treated as unset", err)
	}
}

// TestLoadAccountsEmptyList 验证 accounts: [] 与不写 accounts 等价：
// devin.token 单号路径照常生效，本机自动发现链也不被关闭（此时不断言
// Token 取值——真实 home 下可能存在 credentials.toml）。
func TestLoadAccountsEmptyList(t *testing.T) {
	config, err := loadWithDevin(t, t.TempDir(), "  token: tok-single\n  accounts: []\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.Devin.Token != "tok-single" {
		t.Fatalf("Devin.Token = %q, want tok-single", config.Devin.Token)
	}

	t.Setenv("DEVIN_TOKEN", "")
	t.Setenv("WINDSURF_API_KEY", "")
	if _, err := loadWithDevin(t, t.TempDir(), "  accounts: []\n"); err != nil {
		t.Fatalf("Load() error = %v, want success (auto-discovery stays live)", err)
	}
}

// TestLoadAccountsNameValidation 钉住账号 name 契约：必填、字符集
// [A-Za-z0-9_-]{1,32}、"default" 保留给隐式单 lane、池内唯一。
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
		{"reserved name default", "    - name: default\n      token: tok\n", "reserved"},
		{"duplicate name", "    - name: alpha\n      token: tok-a\n    - name: alpha\n      token: tok-b\n", `duplicate name "alpha"`},
		{"valid charset", "    - name: 'alpha-1_B'\n      token: tok\n", ""},
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
// 两者皆缺报错；credentials_file 在加载期就必须解出 windsurf_api_key
// （文件缺失与文件在但无键是两种报错）；只给文件时 token 由文件播种；
// 两者都给时 token 保持字面量、文件留作自愈来源。
func TestLoadAccountsCredentialSources(t *testing.T) {
	t.Run("neither token nor file", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(), "  accounts:\n    - name: alpha\n")
		if err == nil || !strings.Contains(err.Error(), "one of token/credentials_file is required") {
			t.Fatalf("Load() error = %v, want missing-credential error", err)
		}
	})

	t.Run("missing credentials file", func(t *testing.T) {
		_, err := loadWithDevin(t, t.TempDir(), "  accounts:\n    - name: alpha\n      credentials_file: 'missing.toml'\n")
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Load() error = %v, want read failure wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("credentials file without key", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "creds.toml"), "other_key = \"x\"\n")
		_, err := loadWithDevin(t, dir, "  accounts:\n    - name: alpha\n      credentials_file: 'creds.toml'\n")
		if err == nil || !strings.Contains(err.Error(), "no windsurf_api_key") {
			t.Fatalf("Load() error = %v, want no windsurf_api_key", err)
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
		t.Setenv("HOME", dir)
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
// 去重之前，"  yanjian  " 落成 "yanjian"，"  tok  " 落成 "tok"。
func TestLoadAccountsTrims(t *testing.T) {
	config, err := loadWithDevin(t, t.TempDir(),
		"  accounts:\n    - name: '  yanjian  '\n      token: '  tok  '\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	account := config.Devin.Accounts[0]
	if account.Name != "yanjian" {
		t.Fatalf("Name = %q, want trimmed yanjian", account.Name)
	}
	if account.Token != "tok" {
		t.Fatalf("Token = %q, want trimmed tok", account.Token)
	}
}
