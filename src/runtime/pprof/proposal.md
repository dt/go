# Proposal: Zero-Allocation pprof Label API

### Proposal Details

## Abstract

This proposal introduces new functions for setting pprof labels on goroutines that reach zero heap allocations in steady state. The existing `pprof.Do` API incurs 3–5 heap allocations per call in typical usage, making it too expensive for always-on labeling. The proposed API changes the shape to eliminate allocations forced by the current design (closure, context wrapping) and pools label storage in the runtime to eliminate the rest.

## Background and Motivation

Profiling labels (`pprof.Do`, `pprof.Labels`) attach key/value metadata to goroutines so that CPU profiles and goroutine dumps can attribute work to logical operations like request IDs or job names. Today CockroachDB, and likely other performance-sensitive Go users, only applies labels when a profile is actively being collected, gated by an atomic check. This means operations that passed the labeling point before profiling started are unlabeled, goroutine dumps are always unlabeled, and when labeling is on the elevated overhead is not ideal.

The goal is to make labeling cheap enough to perform unconditionally and liberally.

A typical labeling call like `pprof.Do(ctx, Labels("req", strconv.Itoa(id)), func(ctx) { ... })` triggers 3–5 heap allocations: the `strconv.Itoa` string (since `Labels` only accepts strings), the closure (the inliner doesn't reliably inline `Do` — the setup/teardown calls cost 57 each against an 80-point budget, so without PGO the closure escapes), the `context.WithValue` wrapping, the `&labelMap{}`, and if parent labels exist, the merged `[]Label` slice.

The actual runtime operation is a single pointer write to `getg().labels`. The cost is entirely in the ceremony that precedes it. While some of that ceremony — producing a merged label map — is intrinsic, that part is relatively small. The heap allocations and their potential GC-assist pauses are where the overhead becomes significant enough to deter labeling.

## Proposed Change

### Package

The new API lives in a new package `runtime/pprof/pproflabel`. Several natural names conflict with existing exported symbols in `runtime/pprof` (`Labels` is already a function, `Label` is already a function), so a separate package avoids awkward naming. Alternatively new types/functions could be added to the existing pprof if non-conflicting names were chosen.

### API

```go
package pproflabel // import "runtime/pprof/pproflabel"

// Value is an opaque value for use with Set. It holds either a string or
// an integer. The zero value represents an absent label.
type Value struct { /* unexported: string + int64, 24 bytes */ }

// Str returns a Value holding the string s.
func Str(s string) Value

// Int returns a Value holding the integer n. The value is serialized as a
// pprof Label.num field, avoiding string formatting at labeling time.
func Int(n int64) Value

// Set sets a single profiling label on the current goroutine and returns an
// opaque representation of the previous value for that key. Labels persist
// until replaced or the goroutine exits. Child goroutines inherit the parent's
// labels.
func Set(key string, val Value) Value
```

The returned previous value enables scoped restoration via `defer`:

```go
defer pproflabel.Set("job", pproflabel.Set("job", pproflabel.Int(123)))
```

The inner call sets the label and returns the old value. The deferred outer call restores it. All values are passed and returned by value — no closures, no heap allocations.

`pproflabel.Value` has unexported fields so that callers can receive previous values back for the purpose of passing them to a deferred restoration Set, but cannot inspect what was returned to prevent the API from becoming a form of goroutine-local storage.

A batch API could avoid the overhead of producing intermediate merged label maps when setting multiple labels at once:

```go
type Multi struct { /* unexported */ }

func NewMulti() Multi
func (m Multi) Str(key string, val string) Multi
func (m Multi) Int(key string, val int64) Multi
func SetMulti(m Multi) Multi
```

The exact shape of the batch API is still open for discussion and could be deferred to a follow-up proposal.

### Integer label values

The internal `Label` struct gains an `IntVal int64` field alongside the existing `Value string`. Integer labels are serialized using the existing pprof protobuf `Label.num` field, which already supports integer values. No proto format changes are needed.

### No context-based label readback

The `Do` API stores labels in a context value to allow reading them via `pprof.Label(ctx, key)`. The new API does not do this.

Maintaining a copy of the label map in a context value costs an extra allocation and makes the context value chain deeper. It also introduces a correctness issue: the copy in context can skew from the actual `g.labels` that the profiler reads, for example if a context crosses a goroutine boundary via a channel. Eliminating the copy into context cuts both costs. Callers who want to propagate label information across goroutine boundaries can still do so with their own `context.WithValue`.

## Implementation

`Set` is a thin wrapper around assigning `g.labels` to a new label map containing the updated value for the passed label. Making this cost zero allocations comes down to being able to produce a merged `*labelMap` without allocating, which it can do in steady-state by pooling and reusing previously allocated label maps and their backing storage. The first call allocates; subsequent calls reuse capacity from the pool.

Label maps are pooled in P-local free lists (like `sudogcache`/`deferpool`), providing `sync.Pool`-level performance without importing `sync` from the runtime. Each P holds a capped free list of `profLabelMap` structs.

A pooled label map can be referenced by multiple parties: the goroutine that created it, child goroutines that inherited it via `newproc1`, and the profiler which copies the pointer during CPU sampling. We cannot return a map to the pool until all references are gone. Ref-counting tracks this. Each map has an atomic `refs` field that is decremented when a new sharer appears and incremented when a sharer releases.

The profiler complicates ref counting because the CPU sampling signal handler cannot do per-sample atomic operations. This is handled with a global `profileEpoch` counter. Maps are claimed from the pool with `refs = epoch - 1`. When a profile starts, the epoch increments, raising the bar and preventing any in-flight maps from being pooled until the profiler finishes with them. After serialization, the profiler releases its claim by incrementing refs; if refs reaches the current epoch, the map returns to the pool.

| Event | Operation | Who |
|-------|-----------|-----|
| Claim from pool | `refs = epoch - 1` | `runtime/pprof/pproflabel` |
| Child goroutine spawned | `atomic refs--` | `runtime.newproc1` |
| Child goroutine exits | `atomic refs++`; pool if `refs == epoch` | `runtime.gdestroy` |
| Creator replaces labels | `atomic refs++`; pool if `refs == epoch` | `runtime/pprof/pproflabel` |
| Profile starts | `epoch++` | `runtime` |
| Profile serialization done | `atomic refs++`; pool if `refs == epoch` | `runtime/pprof` |

Non-pooled label maps from the existing `Do`/`WithLabels` API have `refs = 0` and are GC'd as before. The ref counting is a no-op on them.

The existing `Do`, `Labels`, `WithLabels`, `SetGoroutineLabels`, `Label`, and `ForLabels` functions in `runtime/pprof` continue to work exactly as today. `profLabelMap` embeds `label.Set` as its first field, so `(*label.Set)(gp.labels)` works for both old and new label maps. Goroutine dumps and profile serialization handle both transparently. The CPU profiler's signal handler is unchanged — it copies `gp.labels` into `profBuf.tags` as before, and the epoch bump prevents premature pooling.

## Performance

Benchmarked on Apple M4 Pro (darwin/arm64):

```
BenchmarkSetLabel/string          14.95 ns/op    0 B/op    0 allocs/op
BenchmarkSetLabel/int             14.88 ns/op    0 B/op    0 allocs/op
BenchmarkSetLabel/defer-restore   16.79 ns/op    0 B/op    0 allocs/op
BenchmarkSetLabel/compare-Do      62.54 ns/op  144 B/op    3 allocs/op
```

`Set` is roughly 4x faster than `Do` and reports zero allocations in steady state after pool warmup.

## Compatibility

This proposal only adds new API surface in a new package. All existing `runtime/pprof` behavior is preserved. The `Do` API is unchanged and not deprecated.

## Alternatives Considered

*Scope/token with auto-reset*

A `scope := labels.Set(...); defer scope.Close()` API would save a snapshot of the old label map and restore it on Close. This works when scopes are strictly LIFO, which `defer` guarantees within a single function. But unlike `Do`, which enforces LIFO through its `func()` argument, a scope object can be passed to other functions or goroutines. If scopes are closed out of order, the snapshot restore silently corrupts labels: restoring an old snapshot discards any labels set by intervening scopes. The `defer f(f())` pattern with per-key `Set` avoids this — each call operates on a single key and returns that key's old value, so out-of-order restores on different keys are harmless.

*Keeping context.Context in the API*

A `Do`-style API that takes a `func()` argument would enforce LIFO scoping, but the closure allocates — Go's inliner doesn't reliably inline the wrapper without PGO. Wrapping labels in context also allocates (`context.WithValue`) and introduces the skew problem described above where context-stored labels diverge from `g.labels` across goroutine boundaries. Removing context from the labeling path eliminates both costs.

*Putting the API in runtime/pprof*

The new API could live in the existing `runtime/pprof` package, but `Labels` and `Label` are already exported there. This would force names like `SetProfLabel` or `ProfLabelValue` which are awkward. A subpackage gives clean names that read well at call sites: `pproflabel.Set(...)`, `pproflabel.Str(...)`, `pproflabel.Int(...)`.

## Open Questions

The batch API (`Multi`/`SetMulti`) uses a builder pattern that allocates a `[]Label` slice internally. Fixed-arity variants (`Set2`, `Set3`) would avoid this but are less ergonomic. Whether to include the batch API in this proposal or defer it is open for discussion.

The P-local label caches are currently capped at 64 entries per P. Whether to clear them during GC (like `sync.Pool`) to bound memory retention is worth considering.
