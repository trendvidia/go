module go.example.org/fakemod

// Pinned to the same language version the enclosing testplugin module
// gets: that module's go.mod (written by testMain) carries no `go`
// directive, so it is go1.16, and a replacement module may not require
// a NEWER version than the main module — with `go 1.24` here the build
// fails with "go: updates to go.mod needed; to update it: go mod tidy"
// (measured 2026-08-28, go1.26). So the packages below are compiled at
// -lang=go1.16: no generics, no `any`. This is the same trap the
// ./mangle/plugin.go fixture documents in plugin_test.go; stating the
// version explicitly is what keeps the error legible.
go 1.16
