# go-plus: Go with plugins on Windows

go-plus is a fork of the Go toolchain, maintained by TrendVidia. Its main
addition is `go build -buildmode=plugin` on Windows, which upstream Go
does not support.

**This is not the official Go distribution.** It is not affiliated with or
endorsed by Google or the Go project. For stock Go, see https://go.dev and
https://github.com/golang/go. Go is a trademark of Google LLC.

Each release tracks an upstream Go release and keeps its version number.
`go version` names both:

```
go version go1.27.2 windows/amd64 (plus v1.27.2)
```

### What the fork adds

- **Plugins on Windows.** `-buildmode=plugin` works on windows/amd64 and
  windows/arm64. Upstream supports plugins only on Linux, FreeBSD and
  macOS. A plugin is a DLL. The host and its plugins share packages,
  types and interface tables, so type assertions and type switches work
  across the boundary.
- **No C toolchain or external linker for Windows plugins.** Upstream
  plugins need cgo and an external linker (gcc or clang) wherever they
  are supported. On Windows, go-plus links plugin DLLs with Go's own linker
  and loads them without cgo, so a pure-Go host and its plugins build
  with nothing but this toolchain, including cross-compiled from macOS or
  Linux. Plugins that use cgo still need a C compiler: mingw-w64 gcc on
  windows/amd64, clang (for example MSYS2's clangarm64) on windows/arm64.
- **Unloading plugins.** `(*plugin.Plugin).Close` unloads a plugin, on
  every platform that supports plugins. Func values that `Lookup`
  returned panic after `Close` instead of jumping into unmapped code.
  Upstream plugins can never be unloaded.
- **Hot-swapping.** A program can replace a plugin with a new build while
  it runs: quiesce, `Close` the old plugin, `Open` the new one. See
  "Replacing a plugin at run time" below for the cost and the rules.
- **Version checks on every shared package.** `plugin.Open` rejects a
  plugin built against a different version of any package it shares
  with the host, not only a different toolchain.

Everything else is upstream Go. The fork changes the compiler, linker,
runtime and `plugin` package; see "Binary/object format differences"
below for what that means for programs that never load a plugin.

### Install

Prebuilt toolchains for darwin/arm64, linux/amd64, linux/arm64,
windows/amd64 and windows/arm64 are attached to each
[release](https://github.com/trendvidia/go/releases). Each archive has
the same layout as an upstream Go archive: unpack it and put `go/bin` on
your `PATH`.

To build from source, see "Building this fork" below.

### Building a plugin on Windows

A plugin is linked against its host's import library. `go build` writes
that library next to any executable that imports `plugin`:

```
go build -o host.exe ./host                     # also writes host.exe.a
go build -buildmode=plugin -ldflags=-pluginimplib=host.exe.a -o plug.dll ./plug
```

Without `-pluginimplib` the plugin build fails and says what is missing.
Build plugins against the host they will be loaded into: `plugin.Open`
rejects a plugin built with a different version of any package it
shares with the host.

Neither step needs a C compiler unless the code uses cgo. Both work when
cross-compiling, for example from macOS:

```
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o host.exe ./host
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -buildmode=plugin \
    -ldflags=-pluginimplib=host.exe.a -o plug.dll ./plug
```

### Reporting problems

- Bugs in the fork: [open an issue](https://github.com/trendvidia/go/issues/new/choose)
  here, or run `go bug`, which opens one with your environment filled in.
- Bugs that also reproduce with stock Go belong upstream, at
  https://github.com/golang/go/issues.
- Security vulnerabilities: see [SECURITY.md](SECURITY.md). Do not file
  them as public issues.

### Building this fork

This is the trendvidia "plus" fork of Go (`go version` reports
`… (plus <fork release>)`, e.g. `go version go1.27.2 darwin/arm64 (plus
v1.27.2)` — the leading version stays canonical upstream Go). When building the toolchain from source, run `make.bash`
with **`CGO_ENABLED` unset** in the environment:

```
cd src
env -u CGO_ENABLED ./make.bash
```

`cmd/dist` bakes the value of `$CGO_ENABLED` at build time into the
toolchain's `DefaultCGO_ENABLED` (`internal/buildcfg`). If `CGO_ENABLED=1`
is exported when `make.bash` runs, the built toolchain defaults cgo to
*on* unconditionally and skips the upstream "no C compiler in `PATH` →
default cgo off" autodetection — which diverges from stock Go and fails
the `cmd/go` `TestScript/autocgo` test. Leaving `CGO_ENABLED` unset makes
the fork match upstream's default cgo behavior. (cgo itself still works;
this only controls the *default* when `CGO_ENABLED` is not set at use
time.)

### Binary/object format differences (plugin object consistency)

To make every package loadable as, and shareable with, a
`-buildmode=plugin` object, this fork changes the compiled object and
final binary format relative to upstream Go. These differences are
deliberate and exist so that a host and its plugins can share code and
data safely and so that `Plugin.Close` can tear a plugin down without
crashing the host:

- **Dynamic-linking codegen by default.** Packages are compiled in
  shared/dynlink mode (`-shared`) so their code and globals are
  position-independent and can be referenced across the host↔plugin
  boundary, instead of upstream's static default.
- **Writable function-pointer descriptors.** The compiler emits
  `go.funcptr.<name>` / `<name>·f` descriptor symbols and, under
  dynlink, places them in writable `.noptrdata` (see
  `cmd/compile/internal/staticdata.WriteFuncSyms`). This lets
  `Plugin.Close` rewrite held func values to a panic-on-call stub
  before the plugin's text is unmapped. Standard `-buildmode=exe`
  builds keep these in read-only data, matching upstream.
- **Per-package version-lock hashes.** Objects/binaries carry per-package
  build fingerprints so `plugin.Open` can reject a plugin built against a
  divergent version of *any* shared package (runtime, protobuf, etc.),
  not just a compiler-version mismatch. See the `plugin` package docs.

**Reproducibility.** Builds with one installed toolchain are bit-for-bit
reproducible: the `go.funcptr` slots are emitted sorted by name, and the
upstream `cmd/go` `TestScript/repro_build` test passes. One upstream
property is given up. Upstream keys a release's build cache on its
version string, so that toolchains built for different hosts (a Linux and
a macOS one cross-compiling for Windows, say) give identical outputs.
This fork keys it on the tools' own build IDs instead, so that a rebuilt
toolchain, or stock Go of the same version, is not served this
toolchain's objects (#20). Tools built for different hosts have different
build IDs, so the same program built with each differs in the action IDs
of its Go build ID, and on ELF and Mach-O in the GNU build-id note and
UUID the linker derives from it; the content IDs in the build ID still
match. Hosts that install the same release archive run identical tools,
and this does not arise between them. Where the tools themselves must be
linked externally (android except arm64, ios), they have no build ID and
are keyed on their version as upstream's are, so #20 is still open there
(#56).

### Known limitations of the plugin support

None of these prevents loading a plugin on any supported platform. The
measurements below were taken per platform on the hosts CI uses.

**A plugin cannot link a third-party C or C++ library on windows/arm64.**
Measured on both arches with a static archive built by clang/gcc:
windows/amd64 links and runs it (C and C++ alike, STL included),
windows/arm64 fails. Plugins there are linked internally, which resolves
no `-l` archive, and external linking of a cgo plugin on that arch does
not work yet. Self-contained cgo and calls into system DLLs do work on
arm64. Keeping native dependencies in the host, which has no such
restriction, avoids the question on both arches.

**Replacing a plugin at run time requires closing the old one first.**
The first module carrying a `(package, link-time hash)` owns it, and
later modules carrying that package depend on the owner, so a newly
loaded generation pins the one it replaces and only the newest is ever
closable. An upgrade must quiesce, `Close`, then `Open` the replacement —
which leaves a window where nothing is serving. Median over runs of 100,
200 and 400 cycles with 4 workers (the Windows fixtures built with
`CGO_ENABLED=0`):

| platform | window (p50) |
|---|---|
| linux/arm64 | 1.4 to 1.5 ms |
| windows/arm64 | 21 to 26 ms |
| windows/amd64 | 28 to 30 ms |
| darwin/arm64 | 108 to 109 ms |

linux/amd64 was not measured. The spread is the platform's dynamic
loader, not this fork: two orders of magnitude separate `dlopen`/`dlclose`
on linux from Darwin's. Budget for the platform you ship on rather than
for the worst row. Each cycle also costs 439 to 678 KiB of reserved
address space on windows/amd64; the fixture's counter for that reads zero
on linux and Darwin, so it has only ever been a Windows figure.
`cmd/cgo/internal/testplugin/testdata/hotswap` is both the test and a
worked example of the supervisor. Closing the window is not currently
planned, so treat it as part of the upgrade contract rather than as a
defect awaiting a fix.

**`plugin.Open` keys its cache on the path as given**, so the same DLL
opened under two spellings of a Windows drive letter (`C:\` vs `c:\`)
loads twice. Canonicalise paths before calling `Open`.

**The upgrade contract is the caller's to keep.** `Plugin.Close`
neutralises the func values `Lookup` returned and the static `·f`
descriptors; it cannot protect a pointer into the image, a
plugin-defined type, or an interface whose dynamic type lives there. See
`Plugin.Close`'s "Upgrading a plugin" section.

### Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Changes to Go itself, as opposed
to the fork's additions, belong upstream: https://go.dev/doc/contribute.

### License

Unless otherwise noted, the source files are distributed under the
BSD-style license found in the LICENSE file, the same license as
upstream Go. PATENTS is upstream Go's patent grant for the code that
came from it.
