// 本文件内嵌管理面板的静态页面资源。
//
// 页面是独立 HTML 文档（login.html / panel.html），go:embed 编译期打包进
// 二进制——单文件部署不引入静态文件目录。拆出独立文件后 JS 可用模板字面量，
// 编辑器也有正常语法高亮。
package dashboard

import (
	"embed"
	_ "embed"
)

//go:embed static/login.html
var loginPage string

//go:embed static/panel.html
var dashboardPage string

// staticFS 打包面板的全部静态资源（css/js/第三方库），由 /panel/static/* 下发。
//
//go:embed static
var staticFS embed.FS
