package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// 编排进程的环境契约（spawn 侧注入）：
//
//	DEVIN2API_SELF_UPDATE      目标版本（同时也是编排身份标记）
//	DEVIN2API_UPDATE_INSTALL   托管二进制绝对路径（换名目标）
//	DEVIN2API_UPDATE_UNIT      systemd unit 名 / launchd label
//	DEVIN2API_UPDATE_OLD_PID   被替换的旧托管 pid（仅留痕）
//	DEVIN2API_UPDATE_ROLLBACK  置位走回滚换名分支
const (
	envSelfUpdate = "DEVIN2API_SELF_UPDATE"
	envInstall    = "DEVIN2API_UPDATE_INSTALL"
	envUnit       = "DEVIN2API_UPDATE_UNIT"
	envOldPID     = "DEVIN2API_UPDATE_OLD_PID"
	envRollback   = "DEVIN2API_UPDATE_ROLLBACK"
)

// takeoverTimeout 是编排进程等新托管实例报到（boot 收尾写 done）的
// 上限：store.Open 在大库上要跑 debug payload 全量聚合播种（实测
// 25s+），窗口按远大于冷启动 + RestartSec 给。超时记 failed 但保留
// 服役——本进程跑的就是目标版本，退出等于亲手制造无绑窗口。
const takeoverTimeout = 300 * time.Second

// DriverDeps 是编排侧跑一轮所需的全部参数；EnvDeps 从环境装配。
type DriverDeps struct {
	Store    StatusStore
	To       string
	Install  string
	Unit     string
	OldPID   int
	Rollback bool
	SelfExe  string
}

// EnvDeps 判定本进程是不是自更新编排进程并装配参数。标记缺席返回
// ok=false（普通实例与 deploy 交接进程都走这里但立即判否）。
func EnvDeps(st StatusStore) (*DriverDeps, bool) {
	to := os.Getenv(envSelfUpdate)
	if to == "" {
		return nil, false
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, false
	}
	oldPID, _ := strconv.Atoi(os.Getenv(envOldPID))
	return &DriverDeps{
		Store:    st,
		To:       to,
		Install:  os.Getenv(envInstall),
		Unit:     os.Getenv(envUnit),
		OldPID:   oldPID,
		Rollback: os.Getenv(envRollback) != "",
		SelfExe:  exe,
	}, true
}

// writePhase 推进 update:status 到下一阶段：读-改-写保住发起方记的
// from（编排进程自己不知道它），读不到时按零值写。
func (d *DriverDeps) writePhase(ctx context.Context, phase, errMsg string) {
	var st Status
	if raw, ok, err := d.Store.GetState(ctx, statusKey); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	st.Phase = phase
	st.To = d.To
	st.PID = os.Getpid()
	st.Rollback = d.Rollback
	st.Error = errMsg
	st.Since = time.Now().UnixMilli()
	raw, _ := json.Marshal(st)
	if err := d.Store.SetState(ctx, statusKey, string(raw)); err != nil {
		slog.Warn("selfupdate: status write failed", "phase", phase, "error", err)
	}
}

// Drive 是编排进程的主推进体：换名→重启托管 unit→等新实例报到→
// 自我 SIGTERM。由 main 在 store 就绪后以 goroutine 拉起；本函数
// 返回时要么已发信号（正常退场走 run() 排空），要么进入超时保活。
func (d *DriverDeps) Drive() {
	ctx := context.Background()
	slog.Info("selfupdate: orchestrator driving",
		"to", d.To, "install", d.Install, "unit", d.Unit, "rollback", d.Rollback)

	d.writePhase(ctx, phaseSwapping, "")
	if err := swapBinary(d.Install, d.SelfExe, d.Rollback); err != nil {
		d.writePhase(ctx, phaseFailed, "swap binary: "+err.Error())
		slog.Error("selfupdate: swap failed", "error", err)
		return // 保活：换名失败时旧实例未动，本进程仍在服役
	}
	d.writePhase(ctx, phaseRestarting, "")
	if err := d.restartUnit(ctx); err != nil {
		// 换名已完成——报错文案带人工兜底路径：手动重启即激活新版本。
		d.writePhase(ctx, phaseFailed,
			fmt.Sprintf("restart %s failed: %v (binary already swapped; manual restart activates %s)", d.Unit, err, d.To))
		slog.Error("selfupdate: restart failed", "error", err)
		return
	}

	// 等新托管实例的 boot 收尾把记录推到 done/failed——它报到即证明
	// 已 bind+开库（main.go 里 listen 在 store.Open 前），可安全退场。
	deadline := time.Now().Add(takeoverTimeout)
	for {
		raw, ok, err := d.Store.GetState(ctx, statusKey)
		if err == nil && ok {
			var st Status
			if json.Unmarshal([]byte(raw), &st) == nil {
				switch st.Phase {
				case phaseDone:
					slog.Info("selfupdate: managed instance took over", "to", d.To)
					_ = terminateProcess(os.Getpid())
					return
				case phaseFailed:
					slog.Error("selfupdate: managed instance reported failure", "error", st.Error)
					return // 保活：新实例版本不符是降级态，本进程继续服役
				}
			}
		}
		if time.Now().After(deadline) {
			d.writePhase(ctx, phaseFailed,
				fmt.Sprintf("new managed instance did not report within %s; orchestrator keeps serving", takeoverTimeout))
			slog.Error("selfupdate: takeover timed out; orchestrator stays serving")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// restartUnit 按平台发托管重启：Linux systemctl --user restart 触发
// 旧实例 SIGTERM 排空（其 drain 起点关 listener，reuseport 组内连接
// 全落本进程）再拉起新实例；macOS launchctl kickstart -k 同语义。
// 只发信号不等待——接管确认走 update:status 轮询，不同步等排空。
func (d *DriverDeps) restartUnit(ctx context.Context) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "systemctl", "--user", "restart", d.Unit)
		uid := strconv.Itoa(os.Getuid())
		cmd.Env = append(os.Environ(),
			"XDG_RUNTIME_DIR=/run/user/"+uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+uid+"/bus",
		)
	case "darwin":
		cmd = exec.CommandContext(ctx, "launchctl", "kickstart", "-k",
			fmt.Sprintf("gui/%d/%s", os.Getuid(), d.Unit))
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// swapBinary 把编排进程自身的二进制内容装到 install 位置。前向更新
// 两段换名：install→install.backup（原子覆盖旧备份）、self(.new)→
// install；回滚三段：install→.swap 暂存、self(.backup)→install、
// .swap→install.backup——换回后 .backup 里存的是刚被替换的版本，
// 再次回滚即还原，往返对称。运行中二进制换名合法（inode 不变）。
func swapBinary(install, selfExe string, rollback bool) error {
	backup := install + ".backup"
	if rollback {
		tmp := install + ".swap"
		if err := os.Rename(install, tmp); err != nil {
			return err
		}
		if err := os.Rename(selfExe, install); err != nil {
			_ = os.Rename(tmp, install) // 尽力还原：失败时 install 缺失比换一半更糟
			return err
		}
		return os.Rename(tmp, backup)
	}
	if err := os.Rename(install, backup); err != nil {
		return err
	}
	return moveFile(selfExe, install)
}

// moveFile 移文件到目标位：rename 优先（同盘原子）；跨盘退化为
// 拷贝+unlink（运行中二进制仍可安全搬——源 inode 由进程 fd 持有）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// BootFinalize 是托管实例的启动收尾：读 update:status，在途记录按
// 自身版本结算——本进程版本 == to 说明换名+重启已生效，记 done（编排
// 进程轮询到它即退场）；== from 说明还没换就被重启打断，记 failed；
// 两者皆否同样记 failed。编排进程/交接进程不跑（role.duty 门禁在
// 调用侧）——编排进程版本就是 to，它跑这里会在换名前就误报 done。
func BootFinalize(ctx context.Context, st StatusStore, version string) {
	raw, ok, err := st.GetState(ctx, statusKey)
	if err != nil || !ok {
		return
	}
	var cur Status
	if json.Unmarshal([]byte(raw), &cur) != nil || !cur.InFlight() {
		return
	}
	switch version {
	case cur.To:
		cur.Phase, cur.Error = phaseDone, ""
	case cur.From:
		// 编排进程还活着在驱动时别抢写——它几步之内会重启本 unit，
		// 下一轮 boot 的实例才轮得到终局结算。
		if cur.PID != 0 && cur.PID != os.Getpid() && pidAlive(cur.PID) {
			return
		}
		cur.Phase = phaseFailed
		cur.Error = fmt.Sprintf("update interrupted: service restarted into %s before swap", version)
	default:
		cur.Phase = phaseFailed
		cur.Error = fmt.Sprintf("service came up as %s, expected %s", version, cur.To)
	}
	cur.Since = time.Now().UnixMilli()
	out, _ := json.Marshal(cur)
	if err := st.SetState(ctx, statusKey, string(out)); err != nil {
		slog.Warn("selfupdate: boot finalize write failed", "error", err)
	}
	slog.Info("selfupdate: boot finalized", "phase", cur.Phase, "version", version)
}
