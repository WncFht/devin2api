package ccpanel

import "embed"

// webFS 内嵌移植自 ccLoad 的管理面板资源（MIT，作者 caidaoli）。
// 页面 HTML 与 assets 全部以 /web/ 绝对路径互链，故嵌入根即站点根。
//
//go:embed all:web
var webFS embed.FS
