// 本文件实现 last-good 配置缓存：每次成功 Load 把生效配置的自包含投影
// 原子写进状态目录，boot 加载失败时兜底服役——配置文件的瞬时破损
// （部署窗口的半截写入、引用的 credentials_file 缺失、编辑中态）不该
// 把服务打成零（Restart=always 会把一次加载失败放大成断供重启循环）。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// lastGoodFile 是状态目录里的缓存文件名。
const lastGoodFile = "last-good-config.yaml"

// LastGood 是 ReadLastGood 返回的缓存：生效配置快照加写入时刻与来源
// 路径两条出处元数据——兜底服役时这些字段进告警与自省视图，回答
// 「当前跑的是什么时候、哪份文件留下的配置」。
type LastGood struct {
	// CachedAt 是缓存写入时刻（即来源配置最后一次成功加载的时刻）。
	CachedAt time.Time `yaml:"cached_at"`
	// SourcePath 是被缓存配置来自的文件路径。
	SourcePath string `yaml:"source_path"`
	// Config 是校验后的生效配置自包含投影（见 WriteLastGood）。
	Config Config `yaml:"config"`
}

// lastGoodPath 返回状态目录里的缓存路径。
func lastGoodPath(stateDir string) string {
	return filepath.Join(stateDir, lastGoodFile)
}

// WriteLastGood 把一次成功加载的生效配置原子写进状态目录
// （CreateTemp 0600 + rename，handoff 与托管实例并发写不互踩——
// 缓存内含解出的明文凭据，与 config.yaml 同信任域）。
//
// 缓存的是校验后的自包含投影：accounts[].token 已被 resolveAccounts
// 填成文件解出的值，CredentialsFile 在投影里摘除——兜底服役的
// Apply 会对生效集再跑 resolveAccounts，留着 credentials_file 等于
// 把「引用文件缺失」这个失败原样带进第二次加载，兜底必死。摘除后
// lane 退化为字面 token 形态：文件恢复前失去 file-watch 自愈通道
// （文件缺席时该通道本来就是死的），下次成功 Load/reload 自动还原。
func WriteLastGood(stateDir, sourcePath string, cfg Config) error {
	projection := cfg
	projection.Devin.Accounts = slices.Clone(cfg.Devin.Accounts)
	for i := range projection.Devin.Accounts {
		projection.Devin.Accounts[i].CredentialsFile = ""
	}
	raw, err := yaml.Marshal(LastGood{
		CachedAt:   time.Now(),
		SourcePath: sourcePath,
		Config:     projection,
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".last-good-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, lastGoodPath(stateDir))
}

// ReadLastGood 读状态目录里的缓存；无缓存、损坏或形态不可服役
// （listen 为空——写出的缓存必然带 listen）都返回错误，调用方按
// 「没有可兜底配置」处理。回读不重跑 Validate：缓存是自家写出的
// 已校验快照，再校验只会让「引用文件缺失」类失败在兜底上重演。
func ReadLastGood(stateDir string) (LastGood, error) {
	raw, err := os.ReadFile(lastGoodPath(stateDir))
	if err != nil {
		return LastGood{}, err
	}
	var cached LastGood
	if err := yaml.Unmarshal(raw, &cached); err != nil {
		return LastGood{}, fmt.Errorf("decode %s: %w", lastGoodFile, err)
	}
	if cached.Config.Server.Listen == "" {
		return LastGood{}, errors.New("cached config has no server.listen")
	}
	return cached, nil
}
