package selfupdate

import (
	"regexp"
	"strconv"
	"strings"
)

// describeRe 剥 git describe 的超前量后缀：<tag>-<N>-g<sha>。
var describeRe = regexp.MustCompile(`^(.+)-\d+-g[0-9a-fA-F]+$`)

// semverRe 解析 v?X.Y.Z(-prerelease)?(+build)。
var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// runningBase 取运行版本对应的基座 tag：describe 超前形
// （v0.16.0-19-g8590aec 即 v0.16.0+19 提交）归到基座 v0.16.0，-dirty
// 尾缀剥除；其它形态（确切 tag、dev、裸 sha）原样返回。
func runningBase(v string) string {
	v = strings.TrimSuffix(v, "-dirty")
	if m := describeRe.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

type semver struct {
	num [3]int
	pre string // 空 = 无预发布段；同号下无预发布 > 有预发布
}

func parseSemver(v string) (semver, bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return semver{}, false
	}
	var s semver
	for i := range 3 {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false
		}
		s.num[i] = n
	}
	s.pre = m[4]
	return s, true
}

// numericID 按 semver §11 判别数字标识符：前导零的非单字符数字串
// 不算数字（"01" 按字母序参与比较）。
func numericID(s string) (int, bool) {
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// compareSemver 按 semver 2.0 优先序比较：数值段先行，预发布段
// 逐标识符比（数字 < 字母，数字按值、字母按 ASCII），标识符更短者小。
func compareSemver(a, b semver) int {
	for i := range 3 {
		if a.num[i] != b.num[i] {
			if a.num[i] < b.num[i] {
				return -1
			}
			return 1
		}
	}
	if a.pre == b.pre {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	as, bs := strings.Split(a.pre, "."), strings.Split(b.pre, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aok := numericID(as[i])
		bn, bok := numericID(bs[i])
		switch {
		case aok && bok:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aok:
			return -1
		case bok:
			return 1
		case as[i] != bs[i]:
			if as[i] < bs[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// updateAvailable 判定 latest release tag 是否严格新于运行版本。
// 运行版本是 git describe 形：先归基座再比优先序——v0.16.0-19-g8590aec
// 领先 v0.16.0 而不是落后，「发现新版本 v0.16.0」是误报。任一侧解析
// 不了（dev、无 tag 仓库的裸 sha、非标 tag）退回不等式判定。
func updateAvailable(latest, current string) bool {
	if latest == "" {
		return false
	}
	lv, lok := parseSemver(latest)
	cv, cok := parseSemver(runningBase(current))
	if !lok || !cok {
		return latest != current
	}
	return compareSemver(lv, cv) > 0
}
