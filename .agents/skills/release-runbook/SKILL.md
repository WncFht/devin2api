---
name: release-runbook
description: Run a devin-2api release end to end via scripts/release.sh — dry-run review, --publish mechanics, post-release verification of workflow/assets/checksums/GHCR tags, and manual repair of a broken release. Use when asked to cut/publish a release, when checking whether a release landed correctly, or when a tag or release body went wrong and needs repair.
---

# Release Runbook

`scripts/release.sh` is the only supported way to cut a release. It computes
the next version from Conventional Commits, gates on a green CI run for the
exact commit being tagged, and pushes an annotated tag whose body becomes the
GitHub Release body (release.yml extracts `%(contents)` — the tag annotation
is the single source of truth for release notes).

## Pre-flight

- Working tree must be clean and `HEAD` must equal `origin/main`; the script
  refuses otherwise. Commit or stash everything first.
- Run `bash scripts/release-selftest.sh` after touching `release.sh` itself —
  it exercises the whole flow offline (bare origin + stub curl) and catches
  regressions like the `##` header-stripping bug before a real release hits
  them.
- Confirm the version bump class is right: 0.x stage bumps minor on `feat`/breaking
  and patch otherwise; `--version vX.Y.Z` overrides the computed value.

## Flow

```sh
scripts/release.sh             # dry-run: prints next version + categorized notes
scripts/release.sh --publish   # VERSION bump commit -> wait for green CI -> tag -> push
```

`--publish` mechanics, in order:

1. Refuses on a dirty worktree or unpushed `HEAD`.
2. Writes `NEXT` into `cmd/devin-2api/VERSION`, commits
   `chore(release): bump VERSION to NEXT`, pushes to `origin/main`. If the file
   already equals `NEXT` this step is skipped (a re-run after a failed attempt
   reuses the earlier bookkeeping commit).
3. Polls `workflow_runs?head_sha=<bump sha>` for the workflow named `CI` until
   `conclusion == success` (`CI_WAIT_SECONDS` deadline, `CI_POLL_INTERVAL`
   between polls). `FAILED` or timeout refuses.
4. Re-fetches and re-verifies `HEAD == origin/main` — if someone pushed during
   the wait, refuses (TOCTOU guard).
5. `git tag -a NEXT -F notes --cleanup=verbatim` then `git push origin NEXT`.

Known acceptable residue: if CI fails after step 2, the VERSION bookkeeping
commit stays on main without a tag. Harmless — a later publish detects
`VERSION == NEXT` and skips re-committing.

## Post-release verification

- `release.yml` run for the tag must be green: it runs tests, builds the 6
  matrix assets (linux asserts static via readelf), publishes Docker images
  from the same artifacts, and creates the GitHub Release.
- Release page must have 7 assets: `devin-2api-{darwin,linux,windows}-{amd64,arm64}`
  (windows as `.zip`) plus `checksums.txt`.
- Release body must show the categorized notes (`## Features` etc.), not a bare
  commit subject — if it doesn't, the tag annotation was lost (see below).
- GHCR tags: `ghcr.io/wncfht/devin2api:<version>`, `:<minor>`, `:<major>`,
  `:latest` (stable only — prereleases don't move `latest`).

## Repair rules (learned the hard way)

- **Never retag a pushed tag.** Re-pushing a tag does not retrigger
  `release.yml` cleanly and rewrites public history others may have fetched.
- If the release body is wrong but the tag is right: extract the annotation
  and fix the body in place —
  `git for-each-ref refs/tags/vX.Y.Z --format='%(contents)' > notes.md` then
  `gh release edit vX.Y.Z --notes-file notes.md`.
- If the tag annotation itself lost `##` headers: the cause is
  `git tag -a -F` defaulting to `cleanup=strip` (comment lines eaten). The fix
  is `--cleanup=verbatim`; repair the body via `gh release edit` as above.
- If `release.yml` produced a release body equal to a commit subject: checkout
  on tag push may leave a lightweight tag ref; the workflow force-fetches
  `refs/tags/X` before reading `%(contents)`. Don't remove that fetch.
