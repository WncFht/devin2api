// 本文件是事务构造函数闸门：BUSY_SNAPSHOT（517）类事故的回归防线。
//
// 背景：deferred 事务（sql.DB.Begin/BeginTx 或手工 BEGIN）先读后写时，
// 读快照与写锁升级之间被并发写挤入会吃 SQLITE_BUSY_SNAPSHOT 快败，
// busy_timeout 救不了过期快照。修法是把写事务收口到 helper——writeTx
// （deferred + 慢占用告警）与 immediateTx（BEGIN IMMEDIATE）——但语法
// 上拦不住新补丁重新引入裸 db.BeginTx + SELECT 把整类事故放回来。
// 本测试用 AST 扫描把「能开事务的代码点」收窄到白名单：新事务想进场
// 必须经过一次有意识的名单决策。
//
// 扫描规则（txCtorGuardDirs 内全部非测试 .go 文件；测试文件豁免——
// 竞争模拟需要手工 BEGIN IMMEDIATE 持锁）：
//   - 任何 .Begin/.BeginTx 选择子表达式（调用与方法值都算）；
//   - 任何以大写 BEGIN 开头的字符串字面量（手工 BEGIN/BEGIN
//     IMMEDIATE；仓内 SQL 关键字一律大写，小写 "begin ..." 是
//     fmt.Errorf 的错误文案，不敏感化才不会误伤它）。
//
// 命中按宿主名判定——外层函数名；包级 var/const 字面量按声明名。
// 不在 txCtorAllowlist 即失败；名单条目必须写清为何不能走 helper。
//
// 边界（本闸门管不到的一半，诚实声明）：writeTx 是 deferred 事务，其
// 函数体「先 SELECT 再 INSERT」在语法上完全合法，同样能踩
// BUSY_SNAPSHOT——PutDebugFile 读旧尺寸再写就是活例。读序（tx 上第一
// 个操作是读是写）依赖数据流与 helper 内部行为，这个层级的静态扫描
// 抓不到；此类风险靠 review 把关，根治是把先读后写的路径改走
// immediateTx。本闸门只保证「构造函数绕不过名单」，不保证「构造出来
// 的事务读序正确」。
package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// txCtorGuardDirs 是被扫描的目录（相对本测试所在包）：store 本体 +
// debuglog。debuglog 目前无直接 *sql.DB 使用，扫它是防未来绕开
// store 自立连接——闸门多扫一个空目录的成本是零。
var txCtorGuardDirs = []string{".", "../debuglog"}

// txCtorAllowlist 收录允许含裸事务构造的宿主名（函数名；包级
// var/const 字面量为其声明名）。每条必须给出为何不能走 writeTx /
// immediateTx 的理由。宿主内构造点消失（改走 helper、函数被删）时
// 条目 stale，测试会点名要求删除——白名单只进不出等于没有闸门。
var txCtorAllowlist = map[string]string{
	"writeTx":               "deferred 写事务 helper 本体：全包写事务的单一构造点（BeginTx + 慢占用告警）",
	"seedPayloadBytesAsync": "conn.BeginTx(ReadOnly) 只读快照：后台聚合不升级写锁，BUSY_SNAPSHOT 类不适用",
	"CellsAudit":            "s.ro 读池上手工 BEGIN 只读事务：水位与计数须共享同一读快照，非写路径",
	"BackfillCells":         "手工 BEGIN IMMEDIATE：committed 标记与守卫/审计/重写交错步骤，helper 形态装不下",
	"immediateTx":           "BEGIN IMMEDIATE helper 本体：抢占写锁事务的单一构造点",
}

// TestTxConstructorGuard 扫描全部受闸目录：任何不在白名单的裸事务
// 构造、以及任何已无覆盖点的 stale 白名单条目，都使本测试失败。
func TestTxConstructorGuard(t *testing.T) {
	live := map[string]int{}
	var problems []string
	for _, dir := range txCtorGuardDirs {
		if err := scanDirTxCtors(dir, txCtorAllowlist, live, &problems); err != nil {
			t.Fatal(err)
		}
	}
	problems = append(problems, staleAllowlistProblems(txCtorAllowlist, live)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("事务构造闸门拦截 %d 项：\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// TestTxConstructorGuardDetectsPlant 证明闸门不是摆设：对种下三类
// 违规的临时目录，必须逐一报出且不误伤白名单宿主与非事务字符串。
func TestTxConstructorGuardDetectsPlant(t *testing.T) {
	dir := t.TempDir()
	src := `package fake

var pkgStmt = "BEGIN IMMEDIATE"

func sneaky() {
	tx, _ := db.BeginTx(ctx, nil)
	_ = tx
}

func helper() {
	db.BeginTx(ctx, nil)
}

func prose() string {
	return "begin immediate: not a tx literal"
}

const notBoundary = "BEGINNING of stream"
`
	if err := os.WriteFile(filepath.Join(dir, "fake.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := map[string]string{"helper": "test fixture"}
	live := map[string]int{}
	var problems []string
	if err := scanDirTxCtors(dir, allow, live, &problems); err != nil {
		t.Fatal(err)
	}
	problems = append(problems, staleAllowlistProblems(allow, live)...)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"sneaky", "pkgStmt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("闸门漏报 %q：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "prose") || strings.Contains(joined, "notBoundary") {
		t.Errorf("闸门误伤非事务字符串：\n%s", joined)
	}
	if live["helper"] != 1 {
		t.Errorf("白名单宿主 helper 的 live 计数 = %d, want 1", live["helper"])
	}
}

// staleAllowlistProblems 把已无覆盖点的白名单条目转成点名删除的
// problem——覆盖点消失说明宿主已改走 helper 或被删，留着条目等于
// 给未来的裸构造预留通行证。
func staleAllowlistProblems(allow map[string]string, live map[string]int) []string {
	var out []string
	for host := range allow {
		if live[host] == 0 {
			out = append(out, fmt.Sprintf(
				"白名单条目 %q 已无覆盖点：其宿主的裸事务构造已移除——请删除该条目保持名单最小", host))
		}
	}
	return out
}

// scanDirTxCtors 解析 dir 下全部非测试 .go 文件，把裸事务构造按宿主名
// 分流：白名单宿主记 live，其余记 problems。
func scanDirTxCtors(dir string, allow map[string]string, live map[string]int, problems *[]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scanTxCtors(fset, path, d.Name.Name, d.Body, allow, live, problems)
				}
			case *ast.GenDecl:
				// 包级 var/const 里的 BEGIN 字面量同样算构造点——
				// 「先声明语句常量再 exec」不能成为绕行通道。
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						host := "<package>"
						if len(vs.Names) > 0 {
							host = vs.Names[min(i, len(vs.Names)-1)].Name
						}
						scanTxCtors(fset, path, host, v, allow, live, problems)
					}
				}
			}
		}
	}
	return nil
}

// scanTxCtors 在 node 内找两类裸事务构造：
//   - .Begin/.BeginTx 选择子（database/sql 事务入口；方法值也算）；
//   - 大写 BEGIN 开头的字符串字面量（手工 BEGIN/BEGIN IMMEDIATE，
//     无论内联进 ExecContext 还是先落在 var/const 里）。
func scanTxCtors(fset *token.FileSet, path, host string, node ast.Node, allow map[string]string, live map[string]int, problems *[]string) {
	ast.Inspect(node, func(n ast.Node) bool {
		var what string
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "Begin" || x.Sel.Name == "BeginTx" {
				what = "." + x.Sel.Name
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && isBeginStmt(s) {
					what = "BEGIN literal " + x.Value
				}
			}
		}
		if what == "" {
			return true
		}
		if _, ok := allow[host]; ok {
			live[host]++
			return true
		}
		pos := fset.Position(n.Pos())
		msg := fmt.Sprintf("%s:%d:%d %s 裸事务构造 %s：写事务走 writeTx/immediateTx，手工事务加 txCtorAllowlist 并写理由",
			path, pos.Line, pos.Column, host, what)
		*problems = append(*problems, msg)
		return true
	})
}

// isBeginStmt 判字符串是否以 SQL 的 BEGIN 开头（BEGIN、BEGIN
// IMMEDIATE/DEFERRED/EXCLUSIVE）。大小写敏感是故意的：仓内 SQL
// 关键字全大写，而小写 "begin immediate: %w" 是 immediateTx 的
// 错误文案，不敏感化会把错误文案误报成事务构造。
func isBeginStmt(s string) bool {
	s = strings.TrimLeft(s, " \t\n")
	if !strings.HasPrefix(s, "BEGIN") {
		return false
	}
	rest := s[len("BEGIN"):]
	return rest == "" || strings.ContainsRune(" \t\n;", rune(rest[0]))
}
