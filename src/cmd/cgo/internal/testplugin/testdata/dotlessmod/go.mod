// A module path with no dot and no slash — what `go mod init extensions`
// gives you, and how first-party extension code is laid out when it is
// not published under a domain. objabi.PathToPrefix has nothing to
// escape, so a package at this module's ROOT is spelled `extensions.X`,
// which is indistinguishable from a stdlib top-level name.
//
// isUserPackageName used to read that shape as "stdlib" and refuse to
// canonicalise it, so host and plugins kept separate instances
// (trendvidia/go-archive#115).
//
// go 1.26 matches the main module's go.mod; a replacement module may
// not require a NEWER language version than the main module.
module extensions

go 1.26
