// Package selfupdate 实现面板内零停机自更新。分两个半身在同一二进制里：
//
// 托管侧（Service）：面板端点调 Start/Rollback，负责下载校验 release
// 二进制到状态目录、发现托管 unit、并把编排进程拉起到 unit 控制域之外
// （systemd-run 瞬态 service 同时逃出 KillMode=control-group 的 cgroup
// 连坐与 ProtectSystem=strict 的挂载命名空间；macOS 无 sandbox，setsid
// 脱离进程组即可）。随后编排侧接管，本进程在 restart 中被排空替换。
//
// 编排侧（Drive，见 driver.go）：新二进制作为完整实例带 REUSEPORT 入组
// 接住排空窗口的连接，做原子换名（当前→.backup、自身→当前）、按平台
// 重启托管 unit、轮询 update:status 直到新托管实例落 done，再自我
// SIGTERM 走常规排空退出；超时则记 failed 并保留服役（同 deploy 交接
// 降级语义——它跑的就是新版本，留着比端口无人接强）。
//
// 进度事实源是 runtime_state 的 update:status 键：写入者沿交接链轮换
// （托管实例记下载阶段 → 编排进程记换名/重启 → 新托管实例 boot 收尾记
// done/failed），面板轮询在重启前后都能读到同一条记录。
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// statusKey 是更新进度在 runtime_state 的键；跨进程读写走同一记录。
const statusKey = "update:status"

// 阶段词表：downloading/verifying/spawning 由旧托管实例写；
// swapping/restarting 由编排进程写；done/failed 由新托管实例的 boot
// 收尾（或编排进程超时/失败）写。inFlight 集之外的阶段允许发起新更新。
const (
	phaseDownloading = "downloading"
	phaseVerifying   = "verifying"
	phaseSpawning    = "spawning"
	phaseSwapping    = "swapping"
	phaseRestarting  = "restarting"
	phaseDone        = "done"
	phaseFailed      = "failed"
)

// Status 是一次更新尝试的可持久化进度记录。PID 记录当前阶段推进者：
// 下载期是旧托管实例 pid，交接期是编排进程 pid——staleness 判定与
// 残留回收都以它为准。
type Status struct {
	Phase    string `json:"phase"`
	From     string `json:"from"`
	To       string `json:"to"`
	Error    string `json:"error,omitempty"`
	Since    int64  `json:"since"` // unix 毫秒，当前阶段开始时刻
	PID      int    `json:"pid"`
	Rollback bool   `json:"rollback,omitempty"`
}

// InFlight 报告本次尝试是否仍在推进（done/failed 之外皆在途）。
func (s Status) InFlight() bool {
	return s.Phase != "" && s.Phase != phaseDone && s.Phase != phaseFailed
}

// StatusStore 是 runtime_state 读写需要的最小面——*store.Store 满足。
type StatusStore interface {
	GetState(ctx context.Context, key string) (string, bool, error)
	SetState(ctx context.Context, key, value string) error
}

// errBusy/errUnsupported 由面板端点映射为 409/501；errStale 不外泄。
var (
	ErrBusy        = errors.New("an update is already in progress")
	ErrUnsupported = errors.New("self-update requires a managed instance (systemd --user or launchd) on Linux/macOS")
	ErrNoBackup    = errors.New("no .backup binary to roll back to")
	ErrBadTag      = errors.New("invalid release tag")
)

// staleAfter 是在途记录的存活上限：推进者 pid 已死或超过该时长视为
// 中断残留，允许新尝试接管（下载超时上限远低于它；编排进程超时后
// 已自行记 failed）。
const staleAfter = 15 * time.Minute

// checkCacheTTL 是「检查新版本」结果的进程内缓存：latest tag 解析打
// GitHub，面板轮询/重复点击不该每次都付一趟外网。
const checkCacheTTL = 20 * time.Minute

// Service 是托管侧自更新服务：装配在面板后面，生命周期同托管进程。
type Service struct {
	store      StatusStore
	version    string
	configPath string
	stateDir   string
	install    string // 托管二进制绝对路径（os.Executable 解析）
	repo       string

	mu        sync.Mutex
	inflight  bool // 本进程内「下载管线在跑」的门禁；跨进程互斥看 status 记录
	unit      string
	unitErr   error
	unitOnce  bool
	checkAt   time.Time
	checkTag  string
	checkErr  error
	checkSeen bool
	// checkInflight 非空表示有 latestTag 在途：并发 Check（含 force）
	// 收敛成一趟 GitHub 调用，等待者收 done 后读共享结果。
	checkInflight chan struct{}
}

// New 装配托管侧服务。install 是托管二进制路径：普通实例取
// os.Executable，编排进程优先取 DEVIN2API_UPDATE_INSTALL（自身
// exe 是 stateDir 的 .new，拿它算 .backup 会失真）。裸跑实例
// discoverUnit 判 unsupported，端点 501。
func New(st StatusStore, version, configPath, stateDir string) (*Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if install := os.Getenv(envInstall); install != "" {
		exe = install
	}
	repo := os.Getenv("DEVIN2API_REPO")
	if repo == "" {
		repo = "WncFht/devin2api"
	}
	return &Service{
		store:      st,
		version:    version,
		configPath: configPath,
		stateDir:   stateDir,
		install:    exe,
		repo:       repo,
	}, nil
}

// tagRe 从 /releases/latest 的 302 Location 里抠 tag。
var tagRe = regexp.MustCompile(`/releases/tag/([^/\s]+)`)

// assetName 映射本平台的 release 资产名；Windows 等不在发布矩阵内
// 的平台报 unsupported（端点层译成 501）。
func assetName() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "devin-2api-linux-amd64", nil
	case "linux/arm64":
		return "devin-2api-linux-arm64", nil
	case "darwin/amd64":
		return "devin-2api-darwin-amd64", nil
	case "darwin/arm64":
		return "devin-2api-darwin-arm64", nil
	}
	return "", ErrUnsupported
}

// ghClient 是打 GitHub 的 http.Client：重定向只放行 github.com 与
// *.githubusercontent.com（release 资产经 302 落到 S3 预签名域），
// 别的域一律拒绝——下载地址由 tag 拼出而非用户输入，allowlist 是
// 对异常重定向的兜底闸门。
var ghClient = &http.Client{
	Timeout: 180 * time.Second,
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		h := req.URL.Hostname()
		if h == "github.com" || strings.HasSuffix(h, ".githubusercontent.com") {
			return nil
		}
		return fmt.Errorf("redirect to disallowed host %q", req.URL.Host)
	},
}

// ghGet 拉一个 GitHub URL；GH_TOKEN 存在时带上（repo 转私有或限流
// 紧张的兼容路径，公开 repo 下为空也无妨）。跨域重定向时 Go 客户端
// 自动剥 Authorization，不会泄到 S3。
func ghGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return ghClient.Do(req)
}

// latestTag 解析仓库最新 release tag：先走 /releases/latest 的 302
// （不吃 api.github.com 匿名限流），失败再退 REST API——与
// lib-deploy.sh latest_tag_of 同序。
func (s *Service) latestTag(ctx context.Context) (string, error) {
	noRedirect := *ghClient
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"https://github.com/"+s.repo+"/releases/latest", nil)
	if err == nil {
		if resp, err := noRedirect.Do(req); err == nil {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if m := tagRe.FindStringSubmatch(loc); m != nil {
				return m[1], nil
			}
		}
	}
	resp, err := ghGet(ctx, "https://api.github.com/repos/"+s.repo+"/releases/latest")
	if err != nil {
		return "", fmt.Errorf("resolve latest tag: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		TagName string `json:"tag_name"`
	}
	if resp.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil ||
		body.TagName == "" {
		return "", fmt.Errorf("resolve latest tag: github api status %s", resp.Status)
	}
	return body.TagName, nil
}

// Check 返回面板「检查更新」视图：当前版本 + 最新 tag + 是否需要更新。
// 结果按 checkCacheTTL 缓存；force 跳过缓存。并发 Check（含 force）
// 经 checkInflight 收敛成一趟 latestTag——面板连点/轮询扇出不该各发
// 一次 GitHub 调用。
func (s *Service) Check(ctx context.Context, force bool) map[string]any {
	s.mu.Lock()
	fresh := s.checkSeen && time.Since(s.checkAt) < checkCacheTTL
	if !force && fresh {
		tag, err := s.checkTag, s.checkErr
		s.mu.Unlock()
		return s.checkView(tag, err)
	}
	if s.checkInflight != nil {
		done := s.checkInflight
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			tag, err := s.checkTag, s.checkErr
			s.mu.Unlock()
			return s.checkView(tag, err)
		case <-ctx.Done():
			return s.checkView("", ctx.Err())
		}
	}
	done := make(chan struct{})
	s.checkInflight = done
	s.mu.Unlock()

	tag, err := s.latestTag(ctx)

	s.mu.Lock()
	s.checkSeen, s.checkAt, s.checkTag, s.checkErr = true, time.Now(), tag, err
	s.checkInflight = nil
	close(done)
	s.mu.Unlock()
	return s.checkView(tag, err)
}

func (s *Service) checkView(tag string, err error) map[string]any {
	view := map[string]any{
		"current": s.version,
		"latest":  tag,
	}
	if err != nil {
		view["error"] = err.Error()
	} else {
		view["update_available"] = tag != "" && tag != s.version
	}
	return view
}

// readStatus 读 update:status；缺席返回 nil。db 读失败按无记录处理——
// 状态缺失比误报「在途」更安全（在途判定的另一道锁是本进程 inflight）。
func (s *Service) readStatus(ctx context.Context) *Status {
	raw, ok, err := s.store.GetState(ctx, statusKey)
	if err != nil || !ok {
		return nil
	}
	var st Status
	if json.Unmarshal([]byte(raw), &st) != nil {
		return nil
	}
	return &st
}

// writeStatus 落一条阶段记录；Error 置空沿用 st.Error。写失败只记
// 日志——编排链上每一步都会重写，单步丢失不留假象。返回打好时间戳
// 的记录供 202 应答（st.Since 由这里盖章）。
func (s *Service) writeStatus(ctx context.Context, st Status) Status {
	st.Since = time.Now().UnixMilli()
	raw, _ := json.Marshal(st)
	if err := s.store.SetState(ctx, statusKey, string(raw)); err != nil {
		slog.Warn("selfupdate: status write failed", "phase", st.Phase, "error", err)
	}
	return st
}

// fail 记 failed 阶段并回错误给调用侧 goroutine 记日志。
func (s *Service) fail(ctx context.Context, st Status, err error) error {
	st.Phase, st.Error = phaseFailed, err.Error()
	s.writeStatus(ctx, st)
	return err
}

// claim 校验「可发起新尝试」：在途且推进者健在→ErrBusy；在途但已陈
// （推进者 pid 消失或超时）→回收残留编排进程后放行。返回当前记录。
func (s *Service) claim(ctx context.Context) (*Status, error) {
	st := s.readStatus(ctx)
	if st == nil || !st.InFlight() {
		return st, nil
	}
	// 推进者存活判定：记录者是本进程时看 inflight 门禁（db 写在先，
	// 管线若意外中断本函数是唯一回收入口），其它进程按 pid 存活判。
	alive := st.PID == 0 || pidAlive(st.PID)
	if st.PID == os.Getpid() {
		alive = s.inflightLocked()
	}
	if alive && time.Since(time.UnixMilli(st.Since)) < staleAfter {
		return st, ErrBusy
	}
	// 陈旧记录：若登记 pid 仍活着且是本服务进程（编排进程超时保活场景），
	// SIGTERM 回收——它跑的是过期待替换版本，占着 reuseport 组没意义。
	if st.PID != 0 && st.PID != os.Getpid() && pidAlive(st.PID) && isDevin2API(st.PID) {
		slog.Info("selfupdate: retiring stale orchestrator", "pid", st.PID)
		_ = terminateProcess(st.PID)
	}
	// 停在 spawning 的陈旧记录另有一种残留：编排进程是不含编排逻辑的
	// 先行版二进制（更新到 pre-feature release），它以 selfupdate 瞬态
	// unit 残存并占着 reuseport 组，但登记 pid 仍是发起方——pid 覆盖
	// 不到，按 unit 名回收。
	reapOrphanOrchestrators()
	return st, nil
}

// reapOrphanOrchestrators 停掉残留的 devin-2api-selfupdate-* 瞬态
// unit。只在陈旧记录回收路径调用（健康编排进程推进的记录是新鲜的，
// 走不到这里）。darwin 编排进程无 unit 可枚举，残留靠记录 pid 回收。
func reapOrphanOrchestrators() {
	if runtime.GOOS != "linux" {
		return
	}
	uid := strconv.Itoa(os.Getuid())
	env := append(os.Environ(),
		"XDG_RUNTIME_DIR=/run/user/"+uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+uid+"/bus",
	)
	list := exec.Command("systemctl", "--user", "list-units", "--no-legend", "--no-page",
		"--output=json", "devin-2api-selfupdate-*")
	list.Env = env
	out, err := list.Output()
	if err != nil {
		return
	}
	var units []struct {
		Unit string `json:"unit"`
	}
	if json.Unmarshal(out, &units) != nil {
		return
	}
	for _, u := range units {
		if u.Unit == "" {
			continue
		}
		stop := exec.Command("systemctl", "--user", "stop", u.Unit)
		stop.Env = env
		if err := stop.Run(); err != nil {
			slog.Warn("selfupdate: orphan orchestrator stop failed", "unit", u.Unit, "error", err)
		} else {
			slog.Info("selfupdate: reaped orphan orchestrator", "unit", u.Unit)
		}
	}
}

func (s *Service) inflightLocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// discoverUnit 判定本进程的托管身份：Linux 读 /proc/self/cgroup 末段
// （user unit 下是 <name>.service）；macOS 在 launchctl 域打印的
// services 表按 pid 查 label。非托管/不支持平台返回 ErrUnsupported。
// 结果只算一次并缓存——托管身份在进程期内不变。
func (s *Service) discoverUnit() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unitOnce {
		return s.unit, s.unitErr
	}
	s.unitOnce = true
	s.unit, s.unitErr = s.probeUnit()
	return s.unit, s.unitErr
}

func (s *Service) probeUnit() (string, error) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return "", err
		}
		for line := range strings.Lines(string(data)) {
			last := line[strings.LastIndex(line, "/")+1:]
			last = strings.TrimSpace(last)
			if strings.HasSuffix(last, ".service") {
				return last, nil
			}
		}
		return "", ErrUnsupported
	case "darwin":
		out, err := exec.Command("launchctl", "print",
			fmt.Sprintf("gui/%d", os.Getuid())).Output()
		if err != nil {
			return "", err
		}
		pid := strconv.Itoa(os.Getpid())
		for line := range strings.Lines(string(out)) {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == pid {
				return f[2], nil
			}
		}
		return "", ErrUnsupported
	}
	return "", ErrUnsupported
}

// Supported 供面板状态端点透出——按平台+托管发现判定，非托管部署
// （裸跑、容器、Windows）按钮直接禁用。
func (s *Service) Supported() bool {
	_, err := s.discoverUnit()
	return err == nil
}

// StatusView 组装 GET /admin/update/status 的响应体。
func (s *Service) StatusView(ctx context.Context) map[string]any {
	unit, err := s.discoverUnit()
	view := map[string]any{
		"supported":          err == nil,
		"current":            s.version,
		"rollback_available": fileExists(s.install + ".backup"),
	}
	if err != nil {
		view["unsupported_reason"] = ErrUnsupported.Error()
	} else {
		view["unit"] = unit
	}
	if st := s.readStatus(ctx); st != nil {
		view["update"] = st
		if st.InFlight() {
			view["stale"] = time.Since(time.UnixMilli(st.Since)) >= staleAfter ||
				(st.PID != 0 && st.PID != os.Getpid() && !pidAlive(st.PID))
		}
	}
	return view
}

var tagShape = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// Start 发起一次前向更新：tag 空取最新 release。202 语义——校验
// 准入后立即返回，下载/校验/拉起编排进程在后台 goroutine 推进，
// 进度经 update:status 供轮询。
func (s *Service) Start(ctx context.Context, tag string) (*Status, error) {
	if _, err := s.discoverUnit(); err != nil {
		return nil, ErrUnsupported
	}
	if tag != "" && !tagShape.MatchString(tag) {
		return nil, ErrBadTag
	}
	if _, err := s.claim(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.inflight = true
	s.mu.Unlock()

	st := Status{Phase: phaseDownloading, From: s.version, To: tag, PID: os.Getpid()}
	st = s.writeStatus(ctx, st)
	go s.pipeline(st)
	return &st, nil
}

// pipeline 是后台推进体：tag 解析→下载校验→写 .new→拉起编排进程。
// 每一步失败都落 failed 再解锁 inflight；成功后 PID 字段被编排
// 进程的 swapping 记录自然顶替。
func (s *Service) pipeline(st Status) {
	ctx := context.Background()
	defer func() {
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
	}()

	if st.To == "" {
		tag, err := s.latestTag(ctx)
		if err != nil {
			_ = s.fail(ctx, st, fmt.Errorf("resolve latest tag: %w", err))
			return
		}
		st.To = tag
	}
	if err := s.download(ctx, st.To); err != nil {
		_ = s.fail(ctx, st, fmt.Errorf("download %s: %w", st.To, err))
		return
	}
	st.Phase = phaseSpawning
	s.writeStatus(ctx, st)
	if err := s.spawn(ctx, filepath.Join(s.stateDir, "devin-2api.new"), st.To, false); err != nil {
		_ = s.fail(ctx, st, fmt.Errorf("spawn orchestrator: %w", err))
		return
	}
	slog.Info("selfupdate: orchestrator spawned", "from", st.From, "to", st.To)
}

// Rollback 拉起 .backup 二进制做对称换回：不下载，spawn 目标换成
// install.backup，编排侧按 ROLLBACK 分支做三段换名。返回记录供 202。
func (s *Service) Rollback(ctx context.Context) (*Status, error) {
	if _, err := s.discoverUnit(); err != nil {
		return nil, ErrUnsupported
	}
	backup := s.install + ".backup"
	if !fileExists(backup) {
		return nil, ErrNoBackup
	}
	if _, err := s.claim(ctx); err != nil {
		return nil, err
	}
	// .backup 的版本号决定 status.to 与接管判据——直接问二进制自己。
	out, err := exec.CommandContext(ctx, backup, "-version").Output()
	if err != nil {
		return nil, fmt.Errorf("probe backup version: %w", err)
	}
	to := strings.TrimSpace(string(out))
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.inflight = true
	s.mu.Unlock()

	st := Status{Phase: phaseSpawning, From: s.version, To: to, PID: os.Getpid(), Rollback: true}
	st = s.writeStatus(ctx, st)
	go func() {
		defer func() {
			s.mu.Lock()
			s.inflight = false
			s.mu.Unlock()
		}()
		if err := s.spawn(context.Background(), backup, to, true); err != nil {
			_ = s.fail(context.Background(), st, fmt.Errorf("spawn orchestrator: %w", err))
			return
		}
		slog.Info("selfupdate: rollback orchestrator spawned", "from", st.From, "to", to)
	}()
	return &st, nil
}

// download 拉 tag 的平台资产并按 checksums.txt 校验 sha256，落
// stateDir/devin-2api.new（0755）。写盘先 .tmp 再 rename——半成品
// 不会以 .new 名出现。体积上限 256MiB，超限即失败（release 二进制
// 当前 ~20-40MB，上限给的是异常流量/挂马响应的闸门）。
func (s *Service) download(ctx context.Context, tag string) error {
	asset, err := assetName()
	if err != nil {
		return err
	}
	base := "https://github.com/" + s.repo + "/releases/download/" + tag + "/"

	resp, err := ghGet(ctx, base+"checksums.txt")
	if err != nil {
		return fmt.Errorf("checksums.txt: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksums.txt: status %s", resp.Status)
	}
	expected := ""
	for line := range strings.Lines(string(body)) {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			expected = f[0]
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}

	tmp := filepath.Join(s.stateDir, "devin-2api.new.tmp")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	resp, err = ghGet(ctx, base+asset)
	if err != nil {
		_ = out.Close()
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, 256<<20))
	_ = resp.Body.Close()
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil || resp.StatusCode != http.StatusOK {
		_ = os.Remove(tmp)
		return fmt.Errorf("asset download: status %s", resp.Status)
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch for %s", asset)
	}
	slog.Info("selfupdate: asset verified", "asset", asset, "tag", tag, "bytes", n)
	return os.Rename(tmp, filepath.Join(s.stateDir, "devin-2api.new"))
}

// spawn 把编排进程拉到 unit 控制域之外：Linux 走 systemd-run 瞬态
// service（管理器 fork+exec——全新挂载命名空间，不受本 unit
// ProtectSystem 只读挂载约束，也不被 restart 的 cgroup 连坐带走）；
// macOS 无 sandbox，setsid 脱离 launchd 追踪的作业进程组即可。
// exe 是要执行的二进制（前向=stateDir/devin-2api.new，回滚=install.backup）。
func (s *Service) spawn(ctx context.Context, exe, to string, rollback bool) error {
	unit, err := s.discoverUnit()
	if err != nil {
		return err
	}
	envPairs := []string{
		"DEVIN2API_REUSEPORT=1",
		"DEVIN2API_HANDOFF=1",
		"DEVIN2API_SELF_UPDATE=" + to,
		"DEVIN2API_UPDATE_INSTALL=" + s.install,
		"DEVIN2API_UPDATE_UNIT=" + unit,
		"DEVIN2API_UPDATE_OLD_PID=" + strconv.Itoa(os.Getpid()),
	}
	if rollback {
		envPairs = append(envPairs, "DEVIN2API_UPDATE_ROLLBACK=1")
	}
	args := []string{"-config", s.configPath, "-state-dir", s.stateDir}

	switch runtime.GOOS {
	case "linux":
		runArgs := []string{
			"--user",
			"--unit=devin-2api-selfupdate-" + strconv.FormatInt(time.Now().Unix(), 10),
			"--collect",
			"-p", "WorkingDirectory=" + s.stateDir,
			"-p", "StandardOutput=append:" + filepath.Join(s.stateDir, "logs", "stdout.log"),
			"-p", "StandardError=append:" + filepath.Join(s.stateDir, "logs", "stderr.log"),
			"-p", "TimeoutStopSec=660",
		}
		for _, kv := range envPairs {
			runArgs = append(runArgs, "-E", kv)
		}
		runArgs = append(runArgs, "--", exe)
		runArgs = append(runArgs, args...)
		cmd := exec.CommandContext(ctx, "systemd-run", runArgs...)
		uid := strconv.Itoa(os.Getuid())
		cmd.Env = append(os.Environ(),
			"XDG_RUNTIME_DIR=/run/user/"+uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+uid+"/bus",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		cmd := exec.Command(exe, args...)
		cmd.Dir = s.stateDir
		env := make([]string, 0, len(os.Environ())+len(envPairs))
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "TZ=") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, envPairs...)
		cmd.SysProcAttr = detachSysProcAttr()
		stdout, err := os.OpenFile(filepath.Join(s.stateDir, "logs", "stdout.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = stdout.Close() }()
		stderr, err := os.OpenFile(filepath.Join(s.stateDir, "logs", "stderr.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = stderr.Close() }()
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		// 收割子进程：托管实例很快就被 restart 带走，但拉起前的窗口内
		// 编排进程早夭会变僵尸——Wait 在后台 goroutine 里顺手收掉。
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return ErrUnsupported
}

// isDevin2API 粗查 pid 是否是本服务进程：回收残留编排进程前的
// 身份确认，防 pid 复用误杀（Linux 读 /proc comm，macOS ps）。
func isDevin2API(pid int) bool {
	if runtime.GOOS == "linux" {
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(comm)), "devin-2api")
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(filepath.Base(strings.TrimSpace(string(out))), "devin-2api")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
