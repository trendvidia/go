# v3 Plugin Architecture — Roadmap

This Go fork adds first-class
`-buildmode=plugin` support on Windows and consolidates several
cross-module mechanisms shared by `plugin` / `c-shared` / `c-archive`
/ `shared` build modes. The work is functional today: plugin loading,
host↔plugin and plugin↔plugin symbol sharing, type/itab dedup, and
`Plugin.Close` (with FreeLibrary + funcptr panic-stub rewrite) all work
on Windows; the cgo handshake is signalled from the runtime so plugins
that contain `import "C"` are loadable.

This roadmap tracks the larger items that remain. The open
[issues](https://github.com/trendvidia/go/issues) are the full backlog;
smaller defects and follow-ups live only there. **Correctness and
stability items take priority over performance items.**

---

## Performance items (no correctness impact)

These regressions affect speed/allocation behavior but do **not**
change program output, do not crash, and do not corrupt state.
Programs run correctly; some perf-sensitive tests may need to be
skipped or relaxed until these are addressed.

### P1. `-typetab` slot indirection defeats compiler specialization (Windows-only)

**Tracked in [#16](https://github.com/trendvidia/go/issues/16).**

**Surface**: `crypto/sha256.TestAllocatonsWithTypeAsserts` fails on
Windows (`allocs = 2; want = 0`). Test currently skipped on Windows
with a comment pointing at this entry. POSIX is unaffected (typetab
is force-on only on Windows).

**Affected commits**:
- `3ee14b9511` — *cmd/compile, cmd/link, runtime: cross-module
  type/itab dedup for Windows plugins* introduced the `-typetab`
  flag and the `go.typeptr.<T>` / `go.itabptr.<X>` slot indirection.
- The activation in `src/cmd/go/internal/work/init.go` forces
  `-typetab` for every Windows gc build so host and plugin produce
  matching pkghashes for shared packages.

**Mechanism**: Under `-typetab`, the compiler rewrites
`&type:T` → `MOV go.typeptr.T(SB)` (load through slot) and
`&go.itab.X` → `MOV go.itabptr.X(SB)`. The slot is statically
initialized to the type/itab address; the runtime rewrites it at
plugin load time to canonicalise across modules.

For non-plugin Windows binaries (the common case), the slot's value
never changes — it's effectively a constant. But the compiler can't
prove that, so:

1. Each interface conversion / type assertion / `reflect.TypeOf` adds
   one extra MOV (load through slot vs immediate). ~1 cycle when slot
   is cache-hot. Sub-percent in absolute terms.
2. The slot load is opaque to escape analysis. Statically-provable
   type assertions like `h.(encoding.BinaryMarshaler)` (where `h`'s
   dynamic type is known) can no longer be folded into a constant
   compare. The optimizer keeps the full runtime type-assert
   machinery, including potential boxing → forced heap allocations.

**Why it matters**: The standard library has explicit `allocs == 0`
assertions for these patterns. Production code rarely does.
Estimated real-world cost:

| Workload | Estimated impact |
|---|---|
| CPU-bound numerical (no interfaces) | 0% |
| Typical web service (some interface use) | <0.5% throughput, occasional extra alloc per request |
| Reflect-heavy / encoding/json-heavy | 2–5% throughput, measurable extra GC pressure |

**Why we can't simply disable typetab on Windows**: Without typetab,
plugin and host pkghashes differ for shared packages, so plugin
loading fails with *"different version of package X"*. Without
typetab + plugins working, pointer identity of itabs/types breaks
across modules — TestUniqueTypesAndItabs, TestIssue18584,
TestIssue22175 all fail.

So typetab is **load-bearing for Windows plugin correctness** but
pays a per-binary cost on every Windows build.

**Proposed fixes (best to worst architectural fit)**:

1. **Move itab canonicalisation to runtime (post-load patching).**
   Walk plugin's compiled code at LoadLibrary time and patch the
   immediate operands of MOV instructions referencing `type:T` /
   `go.itab.X` to point at canonical addresses. Mirrors mingw's
   `_pei386_runtime_relocator` approach. Eliminates compile-time cost
   entirely; cost is paid once at plugin load. Multi-day work — needs
   amd64/arm64 instruction-encoding awareness.
2. **Teach the optimizer about slots.** Mark `go.typeptr.*` /
   `go.itabptr.*` slots with a special attribute the compiler can
   recognize as "value can change at most once at plugin load."
   Compiler-internal change; needs SSA-level reasoning.
3. **Make non-plugin builds opt out.** Add a `-notypetab` build flag.
   Default for `-buildmode=exe` / `-buildmode=pie` on Windows. Plugin
   and plugin-host builds opt in. Pkghash mismatches remain a problem
   — would need a separate mechanism (perhaps making typetab-affected
   typelinks emit DUPOK so they don't change package fingerprint).
4. **Accept the cost.** Document the perf delta. Skip the affected
   stdlib tests on Windows. Current state.

Recommendation: pursue (1) when there's a multi-day window. Until
then the skip stands.

---

## Runtime API for the govm JIT (package `goplus`)

A fork-only standard-library package, `goplus`, exposes runtime hooks
that a virtual machine whose guest values are ordinary Go memory needs:
finding an object's extent from an interior pointer, opting a goroutine
out of the tiny allocator, and (planned) write barriers, preemption
polling and stack maps for generated code, allocation domains, and
collectable layout-only types. `goplus.JIT` is the API level.
**Tracked in [#14](https://github.com/trendvidia/go/issues/14).**

---

## Open architectural items (not regressions, just gaps)

### A1. cgo `runtime/cgo` C-side state per-plugin duplication

**Tracked in [#17](https://github.com/trendvidia/go/issues/17).**

**Status**: workaround in place — `plugin.Open` invokes plugin's own
`x_cgo_notify_runtime_init_done` after `LoadLibrary` (commit
`cfeafd9cdf`). Each plugin DLL has its own copy of the
`runtime/cgo` C-side handshake state (`runtime_init_done` flag,
`runtime_init_wait` Event, `runtime_init_cs` CRITICAL_SECTION).

**Architectural improvement (not blocking)**: IAT-bridge the plain-C
runtime/cgo statics so all DLLs share host's instances. Requires
extending `peSharedRuntimeData`-style infrastructure to handle
C-defined symbols (currently Go-symbol-keyed only). No precedent in
the linker today. Multi-day. The existing per-DLL workaround is
correct and cheap; this is "nice to have."

### A2. Stress test the multi-plugin lifecycle — DONE

**trendvidia/go-archive#3 (test) and trendvidia/go-archive#5 (bugs
it found) are both resolved.** `cmd/internal/testplugin.TestStress` (added
2026-06-11) loads N generated plugin variants in parallel, hammers a
host-owned shared counter through all of them concurrently, closes a
random subset per round (asserting the Close/HasDependents
contract), and reopens — repeat, ending with a dependency-ordered
drain.

On its first run it found that multi-round close→reopen churn broke
on every platform — one root cause, three symptoms: closing the
most-recently-loaded plugin left `lastmoduledatap` dangling into the
unmapped image, so the next load faulted (darwin SIGSEGV in dlopen,
windows AV in addmoduledata) or silently linked the new module onto
a detached node (linux "plugin already loaded"). Fixed in
`runtime: fix dangling lastmoduledatap` plus a follow-up that
serialises Plugin.Close with plugin.Open's constructor-time chain
append (`plugin: serialise Plugin.Close with plugin.Open`). The
stress test now runs unskipped on all platforms.

### A3. Cross-platform validation cadence — DONE

Tracked in trendvidia/go-archive#4. Two workflows run on every pull
request into `trendvidia` and every push to it, on GitHub-hosted runners
for darwin/arm64, linux/{amd64,arm64} and windows/{amd64,arm64}:
`plugin-matrix` (the plugin suites) and `dist-test` (`all.bash` on Unix,
a core-stdlib smoke test on Windows). No CI leg covers an AVX-only x86
CPU; that is checked by hand.

---

## Done in v3 (reference)

For context on what this fork has already shipped (commits on
`trendvidia`):

- Bug A — Windows plugin-mismatch detection (commit `0716fb8629`)
- Bug B — DWARF for Windows plugin DLLs (commit `6a0714f5d9`)
- Bug C — broader ABIInternal conversion (commit `f49d499252`)
- Bug D — remove globalptr-for-data (commit `af6e2382b7`)
- Plugin.Close on Windows (commit `b1a81fa9ca`)
- TestRunHost flake fix via GC-anchored dependents (`7074fa8c0b`)
- Peer-plugin DATA share via post-load IAT rewriting (`36e3506245`)
- cgo runtime_init_done per-plugin signal (`cfeafd9cdf`)

Cross-platform matrix at `c15af031e4`:
- Windows amd64: 27 PASS / 0 FAIL / 3 SKIP on `cmd/cgo/internal/testplugin`
- darwin/arm64: 28 PASS / 0 FAIL / 0 SKIP
- linux/arm64: 27 PASS / 0 FAIL / 1 SKIP (TestTextSectionSplit darwin-arm64-only)
- Full Go test suite: ALL TESTS PASSED on linux/arm64; Windows passes everything except the one P1-tracked alloc-budget test on `crypto/sha256`.

windows/arm64 brought to parity (2026-06-10), after rebase onto
go1.26.4: three arm64-only gaps fixed — internal-link selection for
cgo plugin DLLs (`afe83ba741`), peer-plugin DATA canonicalisation
(`d91f984059`), and the DllMain DLL_PROCESS_ATTACH gate
(`3d2873dc6e`). `cmd/link/internal/ld`, `cmd/cgo/internal/testplugin`
(CGO_ENABLED=1, CC=clang from MSYS2 clangarm64), and
`cmd/internal/testplugin` all pass on windows/arm64, darwin/arm64,
and linux/arm64. windows/amd64 re-validated on native hardware at
the same commit (CGO_ENABLED=1, CC=gcc from MSYS2 mingw64): all
three suites pass — 29 PASS / 0 FAIL / 1 SKIP on the cgo testplugin
suite, confirming the arm64 fixes leave the amd64 external-link path
intact. (The earlier "27/0/3" figure predated the TestCloseAPI and
TestIssue62430 unskips, `d43d5155af`/`d2db014b0f`; the one remaining
skip is TestTextSectionSplit, by design — text-section splitting is
darwin/arm64-only.) linux/amd64 also validated on native hardware at
the same commit: all three suites pass (CGO_ENABLED=1, gcc 12.3),
completing the matrix — darwin/arm64, linux/{amd64,arm64},
windows/{amd64,arm64} all green. Note: windows/arm64 plugin DLLs always
use the internal linker; external linking of arm64 -dynlink code is
not expressible in COFF (no 64-bit PC-relative relocation for the
paired ADRP+LDR GOT loads).
