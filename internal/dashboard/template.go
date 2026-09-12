// 本文件内嵌管理面板的静态页面资源。
//
// 页面是独立 HTML 文档（login.html / panel.html），go:embed 编译期打包进
// 二进制——单文件部署不引入静态文件目录。拆出独立文件后 JS 可用模板字面量，
// 编辑器也有正常语法高亮。
package dashboard

import _ "embed"

//go:embed static/login.html
var loginPage string

//go:embed static/panel.html
var dashboardPage string

//go:embed static/uplot.iife.min.js
var uplotJS []byte

//go:embed static/uplot.min.css
var uplotCSS []byte
