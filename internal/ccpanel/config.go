package ccpanel

import (
	"context"

	"github.com/WncFht/devin2api/internal/accounts"
)

// ConfigOps 是面板配置端点的操作面：Reload 重读并热应用配置文件，
// Current 返回脱敏后的生效配置视图。实现由装配层（main）提供——
// 热应用要跨 adapter/应用/日志管理器多方协调，不属于面板自身职责。
// Provenance 返回三层合并的来源投影（settings 覆盖行/账号 overlay/
// 密码来源），挂进 /admin/config 响应；nil 时该段缺席。
type ConfigOps struct {
	Reload     func() (*accounts.ReloadReport, error)
	Current    func() map[string]any
	Provenance func(ctx context.Context) map[string]any
}
