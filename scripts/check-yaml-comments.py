#!/usr/bin/env python3
"""yaml 散文注释宽度检查：纯注释行（首个非空白字符是 #）超 80 显示列报错。

East Asian Wide/Fullwidth 字符按 2 列计——终端与 diff 看到的是显示宽度，
不是字符数；yamllint 的 line-length 只数字符，管不了这条。无空格且无
宽字符的单 token 注释行（长 URL 之类不可断情形）豁免。注释断点该落在
标点/从句边界是语义判断，机检只把宽度这半段挂上；宽度重排用编辑器
reflow（vim gq / Emacs M-q / VS Code Rewrap）。

pre-commit 传暂存文件名作参数；CI 裸跑时检查 git ls-files 的全部
*.yaml/*.yml。
"""

import subprocess
import sys
import unicodedata

LIMIT = 80


def display_width(text):
    expanded = text.expandtabs(8)
    return sum(2 if unicodedata.east_asian_width(ch) in "WF" else 1 for ch in expanded)


def unbreakable(text):
    body = text.lstrip().lstrip("#").strip()
    return " " not in body and display_width(body) == len(body)


def main(paths):
    if not paths:
        paths = (
            subprocess.check_output(["git", "ls-files", "*.yaml", "*.yml"])
            .decode()
            .split()
        )
    bad = []
    for path in paths:
        with open(path, encoding="utf-8") as f:
            for n, line in enumerate(f, 1):
                text = line.rstrip("\n")
                if not text.lstrip().startswith("#"):
                    continue
                width = display_width(text)
                if width > LIMIT and not unbreakable(text):
                    bad.append(f"{path}:{n}: {width} display cols: {text.strip()}")
    for b in bad:
        print(b)
    if bad:
        print(
            f"{len(bad)} yaml comment line(s) exceed {LIMIT} display columns",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
