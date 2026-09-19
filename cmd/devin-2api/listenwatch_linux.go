//go:build linux

package main

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/WncFht/devin2api/internal/obs"
)

// listenWatchInterval 是监听归属核对周期：扫描只读 /proc（无上游/库 IO），
// 一分钟一拍足够把静默并组的存活窗口从「天」压到「分钟」。
const listenWatchInterval = time.Minute

// listenHolder 是持有本服务监听端口 socket 的一个外部进程快照；
// legit 标记它是不是交接部署认可的合法持有者（判据见 describeHolder）。
type listenHolder struct {
	pid     int
	uid     int // -1 = Stat 未取到；0 值uint 会把读失败误报成 root
	comm    string
	cmdline string
	addr    string // /proc/net/tcp{,6} 里该 socket 的 local_address 原文
	legit   bool
}

// watchListenOwnership 周期核对本进程是监听端口的唯一持有者。reuseport 并组
// 是静默的（同 euid 即可入组，不撞 EADDRINUSE）——9-15 野进程靠这一点并组
// 三天吃掉 ~99% 流量。入组准入由 reusePortProvenance 把关，本循环是运行期
// 兜底：不满足合法判据的持有者每轮扫描 WARN 一条（持续占用=持续告警，
// 告警次数即占用时长），当轮数量写进指标快照的 foreign_listen_holders*。
func watchListenOwnership(ctx context.Context, port int, metrics *obs.Metrics) {
	ticker := time.NewTicker(listenWatchInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		foreign := 0
		for _, h := range listenHolders("/proc", port) {
			if h.legit {
				continue
			}
			foreign++
			slog.Warn("foreign process holds listen port",
				"port", port, "pid", h.pid, "uid", h.uid,
				"comm", h.comm, "addr", h.addr, "cmdline", h.cmdline)
		}
		metrics.NoteForeignListenHolders(foreign)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// listenHolders 返回除本进程外所有持有 port 上 LISTEN socket 的进程快照。
// 链路：/proc/net/tcp{,6} 的 LISTEN 行给出 socket inode 与绑定地址，
// /proc/<pid>/fd 的符号链接把 inode 反查回持有 pid。
func listenHolders(procRoot string, port int) []listenHolder {
	inodes := listenSocketInodes(procRoot, port)
	if len(inodes) == 0 {
		return nil
	}
	self := os.Getpid()
	holders := make([]listenHolder, 0)
	for pid, addr := range socketInodeOwners(procRoot, inodes) {
		if pid == self {
			continue
		}
		h, ok := describeHolder(procRoot, pid)
		if !ok {
			continue
		}
		h.addr = addr
		holders = append(holders, h)
	}
	return holders
}

// listenSocketInodes 解析 procRoot/net/tcp{,6}，返回 port 上 LISTEN socket
// 的 inode → local_address 原文映射。行格式：
//
//	sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
//
// st=0A 即 TCP_LISTEN；local_address 是 hex「地址:端口」。
func listenSocketInodes(procRoot string, port int) map[string]string {
	inodes := map[string]string{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		file, err := os.Open(filepath.Join(procRoot, name))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			p, err := strconv.ParseUint(portHex, 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			if _, dup := inodes[fields[9]]; !dup {
				inodes[fields[9]] = fields[1]
			}
		}
		_ = file.Close()
	}
	return inodes
}

// socketInodeOwners 扫 procRoot/<pid>/fd 把 inodes 里的 socket inode 反查回
// 持有 pid（fd 链接形如 socket:[12345]）。同一 pid 持多个匹配 fd 只记一次。
// 读不到的进程目录（扫描途中退出、hidepid 遮蔽）跳过——查不到持有者的
// socket 不构成可告警对象。
func socketInodeOwners(procRoot string, inodes map[string]string) map[int]string {
	owners := map[int]string{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return owners
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procRoot, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := strings.CutPrefix(link, "socket:[")
			if !ok {
				continue
			}
			inode, ok = strings.CutSuffix(inode, "]")
			if !ok {
				continue
			}
			if addr, match := inodes[inode]; match {
				if _, dup := owners[pid]; !dup {
					owners[pid] = addr
				}
			}
		}
	}
	return owners
}

// describeHolder 读取 pid 的 comm/cmdline/uid 并判定合法性：comm 是本服务
// 二进制名且 environ 含任一托管出处标记（与 reusePortProvenance 同集——
// systemd 的 INVOCATION_ID、交接进程的 DEVIN2API_HANDOFF、服务定义显式
// 注入的 DEVIN2API_MANAGED）才算合法持有者，其余一律 foreign 走 WARN。
// comm 读到 ENOENT（扫描途中进程退出）时 ok=false——它已不再是持有者；
// 其它读失败按未知持有者返回（legit=false）：端口的未知持有者正是告警
// 对象。environ 只用于判据匹配，其内容（可能带密钥）不落任何日志。
func describeHolder(procRoot string, pid int) (listenHolder, bool) {
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	h := listenHolder{pid: pid, comm: "?", uid: -1}
	comm, err := os.ReadFile(filepath.Join(base, "comm"))
	if err != nil {
		if os.IsNotExist(err) {
			return listenHolder{}, false
		}
	} else {
		h.comm = strings.TrimSpace(string(comm))
	}
	if cmdline, err := os.ReadFile(filepath.Join(base, "cmdline")); err == nil {
		h.cmdline = truncateCmdline(cmdline)
	}
	if info, err := os.Stat(base); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			h.uid = int(stat.Uid)
		}
	}
	if h.comm == "devin-2api" {
		h.legit = holderHasManagedEnv(base)
	}
	return h, true
}

// holderHasManagedEnv 报告 environ 里是否存在非空的托管出处标记；environ
// 读不到（EPERM、扫描途中退出）按无标记处理——未知持有者走 WARN。
func holderHasManagedEnv(base string) bool {
	environ, err := os.ReadFile(filepath.Join(base, "environ"))
	if err != nil {
		return false
	}
	for _, entry := range strings.Split(string(environ), "\x00") {
		for _, key := range []string{"INVOCATION_ID=", "DEVIN2API_HANDOFF=", "DEVIN2API_MANAGED="} {
			if strings.HasPrefix(entry, key) && len(entry) > len(key) {
				return true
			}
		}
	}
	return false
}

// cmdlineMaxLen 是 WARN 里 cmdline 的截断长度：够认出是什么程序即可，
// 完整 argv（可能很长、带路径参数）留给运维按 pid 自查。
const cmdlineMaxLen = 200

// truncateCmdline 把 NUL 分隔的 /proc/<pid>/cmdline 转成单行并截断。
func truncateCmdline(raw []byte) string {
	s := strings.TrimRight(string(raw), "\x00")
	s = strings.ReplaceAll(s, "\x00", " ")
	if runes := []rune(s); len(runes) > cmdlineMaxLen {
		s = string(runes[:cmdlineMaxLen]) + "…"
	}
	return s
}
