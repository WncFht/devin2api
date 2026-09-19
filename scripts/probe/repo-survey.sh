#!/usr/bin/env bash
# repo-survey.sh [host] [src-glob]
#
# 扫一台机器上所有 git 仓出体检表：origin / branch / dirty / ahead / behind /
# stash / 最后提交时间。不给 host 扫本机，给了走 `ssh host bash -s`
#（远端是 fish 也别想直接发命令串）。
#
#   repo-survey.sh                本机 ~/src/*
#   repo-survey.sh <host>         <host> 上 ~/src/*
#   repo-survey.sh <host> '~/src/*'   自定义 glob
set -u

HOST="${1:-}"
GLOB="${2:-\$HOME/src/*}"

read -r -d '' PROBE <<'EOS' || true
for d in @GLOB@; do
	[ -d "$d/.git" ] || [ -f "$d/.git" ] || continue
	(
		cd "$d" || exit
		branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
		dirty=$(git status --porcelain 2>/dev/null | grep -c .)
		up=$(git rev-parse --abbrev-ref '@{u}' 2>/dev/null || echo '-')
		if [ "$up" != "-" ]; then
			counts=$(git rev-list --left-right --count '@{u}...HEAD' 2>/dev/null)
			behind=${counts%%	*}; ahead=${counts##*	}
		else
			ahead=-; behind=-
		fi
		stash=$(git stash list 2>/dev/null | grep -c .)
		last=$(git log -1 --format='%cs %h' 2>/dev/null)
		origin=$(git remote get-url origin 2>/dev/null | sed 's|.*/||;s|\.git$||')
		printf '%-28s %-24s dirty=%-3s ahead=%-3s behind=%-3s stash=%-2s %-22s %s\n' \
			"$(basename "$d")" "$branch" "$dirty" "$ahead" "$behind" "$stash" "$last" "$origin"
	)
done
EOS

PROBE="${PROBE//@GLOB@/$GLOB}"

printf '%-28s %-24s %-9s %-8s %-9s %-8s %-22s %s\n' \
	REPO BRANCH DIRTY AHEAD BEHIND STASH LASTCOMMIT ORIGIN
if [[ -z "$HOST" ]]; then
	bash -c "$PROBE"
else
	ssh "$HOST" bash -s <<<"$PROBE"
fi
