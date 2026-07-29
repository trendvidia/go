# The Go Programming Language

Go is an open source programming language that makes it easy to build simple,
reliable, and efficient software.

![Gopher image](https://golang.org/doc/gopher/fiveyears.jpg)
*Gopher image by [Renee French][rf], licensed under [Creative Commons 4.0 Attribution license][cc4-by].*

Our canonical Git repository is located at https://go.googlesource.com/go.
There is a mirror of the repository at https://github.com/golang/go.

Unless otherwise noted, the Go source files are distributed under the
BSD-style license found in the LICENSE file.

### Download and Install

#### Binary Distributions

Official binary distributions are available at https://go.dev/dl/.

After downloading a binary release, visit https://go.dev/doc/install
for installation instructions.

#### Install From Source

If a binary distribution is not available for your combination of
operating system and architecture, visit
https://go.dev/doc/install/source
for source installation instructions.

### Building this fork

This is the trendvidia "plus" fork of Go (`go version` reports
`… (plus <fork release>)`, e.g. `go version go1.26.5 darwin/arm64 (plus
v1.26.5a)` — the leading version stays canonical upstream Go). When building the toolchain from source, run `make.bash`
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
  not just a compiler-version mismatch. See the `plugin` package docs and
  [trendvidia/go#7](https://github.com/trendvidia/go/issues/7).

**Reproducibility.** Because these `go.funcptr` slots are created while
functions are compiled in parallel, an earlier version emitted them in
goroutine-scheduling order, which made builds non-reproducible
(intermittently). This was fixed by emitting the slots sorted by name at
the end of compilation; builds are now bit-for-bit reproducible and the
upstream `cmd/go` `TestScript/repro_build` test passes. See
[trendvidia/go#8](https://github.com/trendvidia/go/issues/8).

### Contributing

Go is the work of thousands of contributors. We appreciate your help!

To contribute, please read the contribution guidelines at https://go.dev/doc/contribute.

Note that the Go project uses the issue tracker for bug reports and
proposals only. See https://go.dev/wiki/Questions for a list of
places to ask questions about the Go language.

[rf]: https://reneefrench.blogspot.com/
[cc4-by]: https://creativecommons.org/licenses/by/4.0/
