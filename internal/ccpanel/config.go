package ccpanel

// ConfigOps 是面板配置端点的操作面：Reload 重读并热应用配置文件，
// Current 返回脱敏后的生效配置视图。实现由装配层（main）提供——
// 热应用要跨 adapter/应用/日志管理器多方协调，不属于面板自身职责。
type ConfigOps struct {
	Reload  func() (*ConfigReloadReport, error)
	Current func() map[string]any
}

// ConfigReloadReport 是一次热重载的结果：applied 是已生效的变更字段，
// requiresRestart 是改了但要重启才生效的字段（listen/transport 固化项）。
type ConfigReloadReport struct {
	At              string   `json:"at"`
	Applied         []string `json:"applied"`
	RequiresRestart []string `json:"requires_restart,omitempty"`
}
