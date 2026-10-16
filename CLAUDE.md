# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## What this is

A fork of Go that adds first-class `-buildmode=plugin` support on
Windows, where upstream has none. It is consumed as a **toolchain**: a
downstream project builds with it and gets working plugins on
windows/{amd64,arm64} alongside the platforms upstream already supports.

`ROADMAP.md` covers the larger items that remain; the GitHub issues are
the full backlog. Upstream Go's own
conventions govern everything this fork does not change — when in doubt,
match what the surrounding upstream code does.

The fork's marker is the version string: `go1.27.2 (plus <release>)`.

## Layout that matters here

The fork's weight sits in four places:

- `src/cmd/link/internal/ld/pe*.go` — the PE writer, the plugin export
  table, the host import library (`-pluginimplib`), the `.def` handed to
  an external linker.
- `src/cmd/link/internal/ld/canonlink.go` — which symbols may be
  canonicalised across modules, and which static data words get rewritten
  at load. (Named typelink.go until go1.27 deleted upstream's typelink
  emission, which is all that file ever shared with the name.)
- `src/runtime/canonical.go`, `src/runtime/plugin.go` — the load-time
  half: slot canonicalisation, package ownership, dependency tracking,
  `Close`.
- `src/plugin/plugin_windows.go` — `Open`/`Close`/`Lookup` on Windows,
  the ABI fence, and the cgo runtime-init handshake.

Separate from plugins: `src/goplus` is the fork's runtime API for the
govm JIT (issues #7–#12 and #15; tracker #14), with its runtime half in
`src/runtime/goplus.go`. `goplus.JIT` is the API level; bump it with
each addition, and register new API in `api/go1.27.txt` (cmd/api's
TestCheck fails otherwise).

## Build & test

```bash
cd src && env -u CGO_ENABLED ./make.bash      # Unix
cd src && set "CGO_ENABLED=" && make.bat      # Windows (cmd)
```

**`CGO_ENABLED` must be unset for `make.bash`/`make.bat`.** `cmd/dist`
bakes it into `internal/buildcfg.DefaultCGO_ENABLED`, and a baked `1`
makes the shipped toolchain default cgo on unconditionally, skipping
upstream's "no C compiler -> cgo off" autodetection (trendvidia/go-archive#9). A dev shell
that exports `CGO_ENABLED=1` will silently produce a divergent
toolchain. `go test internal/buildcfg` is the check, and
release-binaries runs it before packaging anything.

The plugin surface:

```bash
go test -count=1 plugin cmd/link/internal/ld \
    cmd/cgo/internal/testplugin cmd/internal/testplugin
```

A single test: `go test -count=1 -run TestName -v cmd/cgo/internal/testplugin`

## Definition of done

Depth follows the change class.

- **Linker or runtime plugin path** — the four packages above, on
  **both** Windows arches. They disagree about who writes the export
  table: amd64 links plugins externally (mingw ld auto-exports what it
  can), arm64 links them internally (`cmd/link` writes the table
  itself, and external linking of a cgo plugin there fails outright,
  #29). A change verified on one arch is not verified.
- **Anything else** — the same suites on one platform, plus CI.
- **A fix ships with a test that fails without it.** For a defect whose
  failure mode is a hang or silent corruption, the test needs a deadline
  or a positive marker; "it didn't crash" is not a result.

Two traps this repo has hit repeatedly, both of which produce confident
wrong conclusions:

- **`cmd/dist` runs every package with `-short`**, and
  `cmd/cgo/internal/testplugin` skips itself wholesale under it. So
  `all.bash` green says nothing about the plugin suites; `plugin-matrix`
  is what exercises them.
- **A toolchain older than #20 does not invalidate `GOCACHE` on
  rebuild.** Its release-style `VERSION` makes the tool ID version-only,
  so a link can be served from cache and appear to prove a linker change
  it never ran. The fork now keys on the tools' content IDs, but an A/B
  against an older fork release, or a `-toolexec` stand-in that reports
  the same `-V=full`, still needs `-a` or a fresh `GOCACHE`. Either way,
  check a positive marker in the artifact (an export, a symbol, a
  string) before drawing a conclusion from it.

## CI

Two workflows, on GitHub-hosted runners (free: the repo is public) —
`macos-15`, `ubuntu-24.04`, `ubuntu-24.04-arm`, `windows-2025`,
`windows-11-arm`. No self-hosted runners remain (removed 2026-10-12).
Jobs are named `<workflow> <goos>/<goarch>`; branch protection on
`trendvidia` requires all 10 by that name. Admins (only the `trendvidia`
account, which agent sessions also use) may bypass; merging stays the
owner's call.

- **`plugin-matrix`** — the plugin suites on all five legs. This is the
  gate for plugin work.
- **`dist-test`** — `all.bash` per Unix platform, a core-stdlib smoke on
  Windows. Broad regression cover; short-skips the plugin suites, see
  above.

Hosted x64 machines all have AVX2, so nothing in CI covers an AVX-only
CPU; run on an AVX-only machine by hand for simd/runtime work. windows/arm64 builds
with MSYS2 clangarm64 (the image's `gcc` is x64 mingw).

Each run gets its own `GOCACHE`, computed in a step rather than job-level
`env` — `runner.temp` does not resolve in `jobs.<id>.env` and a workflow
that uses it there fails with **zero jobs and no annotation** (#20).

## Merge policy

PR into `trendvidia`, the default branch. `master` tracks the pinned
upstream base tag and is not where work lands. Both workflows green
before merge; the owner merges.

**Releases are cut by the owner**, and in this order:

1. Bump `forkVersionMarker` in `src/cmd/go/internal/version/version.go`
   to the new tag. It is a hardcoded constant — the release letter cannot
   live in `VERSION`, because Go's version grammar rejects `go1.26.5a`
   and would compare it as malformed, below plain `go1.26.5`. Forgetting
   it ships a toolchain that reports the previous release for as long as
   someone runs it; `release-binaries` now fails before packaging if the
   built toolchain does not name the tag being built.
2. Tag a commit whose `plugin-matrix` and `dist-test` runs are green **on
   the default branch**, not merely on the PR that merged into it.
3. Create the GitHub release, then dispatch `release-binaries` with the
   tag. It builds each platform natively, asserts the baked cgo default
   (trendvidia/go-archive#9), asserts the version marker, packages, and attaches archives to
   the existing release — it adds assets and does not alter notes, so a
   failed leg can be re-dispatched.
4. Record known limitations in the README rather than only in the release
   notes: a consumer hits them months later, when the notes are three
   releases back.

## Filing

Issues carry a **done-when** that someone else could check, and the
evidence behind the claim — the command, the output, the host it ran on.
Numbers get the platform and the toolchain commit; a measurement without
those cannot be reproduced or challenged.

`priority:P1`/`P2` for urgency, `post-mvp` for scope, `wont-autofix` to
keep automation off an issue that needs a person. Reachability is a
separate axis from scope: how rare a defect is affects its priority,
never whether it belongs in a milestone.

An issue whose mechanism shipped but whose decision is still open stays
open and says which. Follow-up containers ("Follow-ups from PR #N") are
how residuals from a merged change are kept; close them when their
contents are done, not when the PR merges.
