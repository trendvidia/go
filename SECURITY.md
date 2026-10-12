# Security Policy

go-plus is a fork of Go. This policy covers the fork. Upstream Go has its
own: https://go.dev/security/policy.

## Supported versions

Security fixes go into the latest go-plus release. Each release tracks an
upstream Go point release, so upstream security fixes arrive when the fork
moves to the upstream release that carries them.

## Reporting a vulnerability

Report it privately through GitHub:
[open a security advisory](https://github.com/trendvidia/go/security/advisories/new).
Do not file a public issue.

Please include the `go version` output, the platform, and a way to
reproduce it.

**If it also reproduces with stock Go**, it is an upstream vulnerability.
Report it to the Go team as their policy describes, and tell us here as
well so the fork can pick up the fix.
