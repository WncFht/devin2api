//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeProcFixture 在假 procRoot 下写文件（自动建父目录）。
func writeProcFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestListenSocketInodes 钉住 /proc/net/tcp{,6} 的解析口径：只收 st=0A
// （LISTEN）且本地端口匹配的行，inode 取第 9 列。
func TestListenSocketInodes(t *testing.T) {
	root := t.TempDir()
	writeProcFixture(t, root, "net/tcp", `  sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BFB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11111 2 000000005b0ff343 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 22222 2 000000005b0ff343 100 0 0 10 0
   2: 0100007F:0BFB 0100007F:E6B0 06 00000000:00000000 00:00000000 00000000  1000        0 33333 2 000000005b0ff343 100 0 0 10 0
`)
	writeProcFixture(t, root, "net/tcp6", `  sl  local_address                         remote_address                        st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0BFB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 44444 2 000000001709aba5 100 0 0 10 0
`)
	inodes := listenSocketInodes(root, 0x0BFB)
	if len(inodes) != 2 || inodes["11111"] == "" || inodes["44444"] == "" {
		t.Fatalf("inodes = %v, want {11111, 44444}", inodes)
	}
	if inodes["11111"] != "0100007F:0BFB" {
		t.Fatalf("addr kept = %q", inodes["11111"])
	}
}

// TestListenHoldersClassifiesHolders 走全链路的假 proc：同端口 LISTEN
// socket 被三个进程持有——devin-2api+托管标记是合法交接载具、裸 comm
// 是野进程、本进程自身必须排除。
func TestListenHoldersClassifiesHolders(t *testing.T) {
	root := t.TempDir()
	writeProcFixture(t, root, "net/tcp", `  sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 00000000:0BFB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 999 2 000000005b0ff343 100 0 0 10 0
`)
	mkHolder := func(pid string, comm, environ string) {
		base := filepath.Join(root, pid)
		if err := os.MkdirAll(filepath.Join(base, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProcFixture(t, root, pid+"/comm", comm+"\n")
		writeProcFixture(t, root, pid+"/cmdline", comm+"\x00-arg\x00")
		if environ != "" {
			writeProcFixture(t, root, pid+"/environ", environ)
		}
		if err := os.Symlink("socket:[999]", filepath.Join(base, "fd", "3")); err != nil {
			t.Fatal(err)
		}
	}
	mkHolder("4242", "devin-2api", "PATH=/x\x00DEVIN2API_MANAGED=1\x00HOME=/h")
	mkHolder("4243", "rogue-joiner", "PATH=/x\x00")
	mkHolder(strconv.Itoa(os.Getpid()), "devin-2api", "DEVIN2API_MANAGED=1")

	holders := listenHolders(root, 0x0BFB)
	if len(holders) != 2 {
		t.Fatalf("holders = %+v, want 2 (self excluded)", holders)
	}
	byPID := map[int]listenHolder{}
	for _, h := range holders {
		byPID[h.pid] = h
	}
	if legit := byPID[4242]; !legit.legit || legit.comm != "devin-2api" {
		t.Fatalf("managed holder = %+v, want legit", legit)
	}
	if foreign := byPID[4243]; foreign.legit || foreign.comm != "rogue-joiner" {
		t.Fatalf("rogue holder = %+v, want foreign", foreign)
	}
}

// TestHolderHasManagedEnv 钉住托管出处判据：三个标记任一非空即合法，
// 空值与文件缺席都不算（与 reusePortProvenance 的 Getenv!="" 同口径）。
func TestHolderHasManagedEnv(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "1")
	cases := map[string]bool{
		"INVOCATION_ID=abc\x00":           true,
		"DEVIN2API_HANDOFF=1\x00":         true,
		"DEVIN2API_MANAGED=1\x00":         true,
		"XDEVIN2API_MANAGED=1\x00":        false, // 前缀不得误配
		"DEVIN2API_MANAGED=\x00":          false, // 空值非出处
		"PATH=/usr/bin\x00HOME=/root\x00": false,
		"":                                false,
	}
	for environ, want := range cases {
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "environ"), []byte(environ), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := holderHasManagedEnv(base); got != want {
			t.Fatalf("holderHasManagedEnv(%q) = %v, want %v", environ, got, want)
		}
	}
	// environ 文件缺席 → 未知持有者 → false（走 WARN）。
	if err := os.Remove(filepath.Join(base, "environ")); err != nil {
		t.Fatal(err)
	}
	if holderHasManagedEnv(base) {
		t.Fatal("missing environ should not be legit")
	}
}

// TestDescribeHolderExited 扫描途中进程退出（comm ENOENT）的持有者被丢弃——
// 它已不再是持有者，不该误报。
func TestDescribeHolderExited(t *testing.T) {
	if _, ok := describeHolder(t.TempDir(), 424242); ok {
		t.Fatal("exited holder should be dropped")
	}
}

// TestTruncateCmdline 钉住 cmdline 的 NUL→空格 转换与截断。
func TestTruncateCmdline(t *testing.T) {
	if got := truncateCmdline([]byte("devin-2api\x00-config\x00/etc/x.yaml\x00")); got != "devin-2api -config /etc/x.yaml" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", cmdlineMaxLen+50)
	if got := truncateCmdline([]byte(long)); len(got) <= cmdlineMaxLen || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation failed: len=%d", len(got))
	}
}

// TestListenHoldersRealSocket 走真实 /proc：测试进程自己的监听 socket
// 必须出现在扫描里、但被 self 排除——返回空集证明整条链路通畅。
func TestListenHoldersRealSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	if holders := listenHolders("/proc", port); len(holders) != 0 {
		t.Fatalf("holders = %+v, want empty (only self holds the port)", holders)
	}
}

// holdEnvVar 标记 helper 进程：-test.run 重入测试函数时经它转入
// 「bind 后阻塞」分支，成为真实的外部持有者。
const holdEnvVar = "LISTEN_WATCH_HOLD_PORT"

// startHoldHelper 起一个辅助进程，用 SO_REUSEPORT 并入 port 所在组——
// 复现 9-15 野进程的并组路径。bin 决定 helper 的 comm：经名为
// devin-2api 的软链 exec 时内核 comm 即 devin-2api（合法判据入口）。
func startHoldHelper(t *testing.T, bin string, port int, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "-test.run=^TestListenHoldersForeignProcess$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", holdEnvVar, port))
	cmd.Env = append(cmd.Env, extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// waitHolder 轮询扫描直到 pid 出现在持有者名单（或超时失败）；
// helper 从启动到入组有一小段调度窗口。
func waitHolder(t *testing.T, port, pid int) listenHolder {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, h := range listenHolders("/proc", port) {
			if h.pid == pid {
				return h
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never appeared as holder of port %d", pid, port)
	return listenHolder{}
}

// TestListenHoldersForeignProcess 是端到端核对：本测试进程用 REUSEPORT
// 绑端口后，两个 helper 先后并组——comm 非 devin-2api 的被判 foreign；
// comm 是 devin-2api 且带 DEVIN2API_MANAGED 的被判 legit（交接部署
// 认可的同类持有者，不告警）。
func TestListenHoldersForeignProcess(t *testing.T) {
	if os.Getenv(holdEnvVar) != "" {
		port, err := strconv.Atoi(os.Getenv(holdEnvVar))
		if err != nil {
			os.Exit(1)
		}
		lc := &net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
			return setReusePort(c)
		}}
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper bind:", err)
			os.Exit(1)
		}
		defer func() { _ = l.Close() }()
		select {}
	}

	lc := &net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return setReusePort(c)
	}}
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	// foreign helper：测试二进制名作 comm（devin-2api.test 截 15 字符后
	// 仍非 devin-2api），无托管标记——必须上榜 foreign。
	rogue := startHoldHelper(t, os.Args[0], port)
	if h := waitHolder(t, port, rogue.Process.Pid); h.legit {
		t.Fatalf("rogue helper %+v classified legit", h)
	}

	// legit helper：经 devin-2api 软链 exec（comm=devin-2api）+ 托管
	// 出处标记——上榜但 legit，告警路径跳过。
	link := filepath.Join(t.TempDir(), "devin-2api")
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Fatal(err)
	}
	managed := startHoldHelper(t, link, port, "DEVIN2API_MANAGED=1")
	if h := waitHolder(t, port, managed.Process.Pid); !h.legit {
		t.Fatalf("managed helper %+v classified foreign", h)
	}
}
