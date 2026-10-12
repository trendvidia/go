# Contributing to go-plus

go-plus is a fork of Go that adds plugin support on Windows, plugin
unloading, and related changes. This file is about the fork. Changes to
Go itself belong upstream: https://go.dev/doc/contribute.

## Filing issues

Use this repository's issue tracker for problems with the fork. The
quickest way is `go bug`, which opens an issue with your environment
filled in. Otherwise include:

1. The `go version` output. It ends in `(plus <release>)` for this fork.
2. The operating system and processor architecture.
3. What you did, ideally as a small program, plus the commands that
   build the host and the plugin.
4. What you expected, and what happened instead.

Check whether the problem reproduces with stock Go of the same version.
If it does, it is an upstream bug: report it at
https://github.com/golang/go/issues.

Security problems go through [SECURITY.md](SECURITY.md), not public
issues.

## Contributing code

Pull requests go to the `trendvidia` branch, which is the default.
`master` tracks the upstream release the fork is based on.

Build the toolchain with `CGO_ENABLED` unset (see the README for why):

```
cd src
env -u CGO_ENABLED ./make.bash
```

Then run the plugin suites:

```
go test -count=1 plugin cmd/link/internal/ld \
    cmd/cgo/internal/testplugin cmd/internal/testplugin
```

A change is ready when:

- It comes with a test that fails without it.
- Changes to the linker or the runtime's plugin path have been run on
  both windows/amd64 and windows/arm64. The two link plugins
  differently, so passing on one says little about the other.
- CI is green: `plugin-matrix` runs the plugin suites on all five
  platforms, and `dist-test` runs the full Go test suite. `dist-test`
  runs tests with `-short`, which skips the plugin suites, so it alone
  is not enough for plugin work.

Commit messages follow upstream Go's style
(https://go.dev/wiki/CommitMessage): `pkg/path: lowercase summary`.

Unless otherwise noted, contributions are distributed under the
BSD-style license in the LICENSE file.
