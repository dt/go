// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/pprof/label"
	"unsafe"
)

var labelSync uintptr

// profileEpoch is the current profiling epoch. It is incremented each
// time a CPU or goroutine profile begins. Pooled labelMaps are claimed
// with refs = epoch - 1; they return to the pool when refs reaches the
// current epoch. Bumping the epoch raises the bar, preventing any
// in-flight maps from being pooled until the profiler is done with them.
var profileEpoch atomic.Int64

// profLabelMap is a pooled, ref-counted set of profiling labels.
// It is stored in g.labels as an unsafe.Pointer. The runtime casts
// to *profLabelMap internally.
//
// label.Set is the first field so that (*label.Set)(gp.labels) works
// for both legacy labelMaps (from Do/WithLabels) and pooled profLabelMaps
// (from SetLabel). This is important because traceback.go casts gp.labels
// to *label.Set for goroutine dumps.
//
// profLabelMaps are immutable while referenced — mutation only happens
// during pool transitions (claim/release).
type profLabelMap struct {
	label.Set                  // first field: label data (Key, Value, IntVal per label)
	next      *profLabelMap    // P-local free list linkage
	refs      int64            // ref count (see lifetime tracking below)
}

// profLabelGet claims a profLabelMap from the current P's free list.
// If the free list is empty, it allocates a new one. The returned map
// has refs set to epoch - 1 (one reference held by the caller).
func profLabelGet() *profLabelMap {
	pp := getg().m.p.ptr()
	m := pp.labelCache
	if m != nil {
		pp.labelCache = m.next
		m.next = nil
		pp.labelCacheLen--
	} else {
		// Allocate new. Not on a critical path (first-use only in
		// steady state since the pool recycles).
		m = new(profLabelMap)
	}
	m.refs = profileEpoch.Load() - 1
	m.List = m.List[:0]
	return m
}

// profLabelPut returns a profLabelMap to the current P's free list.
// The caller must ensure no other references exist.
//
//go:nosplit
func profLabelPut(m *profLabelMap) {
	pp := getg().m.p.ptr()
	if pp.labelCacheLen >= 64 {
		// Don't let the cache grow without bound.
		return
	}
	m.next = pp.labelCache
	pp.labelCache = m
	pp.labelCacheLen++
}

// profLabelAddRef records that an additional goroutine now shares m.
// Called from newproc1 when a child goroutine inherits the parent's labels.
//
//go:nosplit
func profLabelAddRef(m *profLabelMap) {
	if m == nil {
		return
	}
	if m.refs == 0 {
		// Non-pooled map (from existing Do/WithLabels API). No-op.
		return
	}
	atomic.Xaddint64(&m.refs, -1) // one more sharer → refs moves away from epoch
}

// profLabelRelease releases one reference to m. If refs reaches the
// current epoch, the map is returned to the P-local pool.
//
//go:nosplit
func profLabelRelease(m *profLabelMap) {
	if m == nil {
		return
	}
	if m.refs == 0 {
		// Non-pooled map (from existing Do/WithLabels API). No-op.
		return
	}
	new := atomic.Xaddint64(&m.refs, 1)
	if new == profileEpoch.Load() {
		profLabelPut(m)
	}
}

// profLabelBumpEpoch increments the profile epoch. This prevents any
// currently in-flight pooled labelMaps from being returned to the pool
// until the profiler has finished with them (via profLabelRelease).
//
//go:linkname profLabelBumpEpoch runtime/pprof.profLabelBumpEpoch
func profLabelBumpEpoch() {
	profileEpoch.Add(1)
}

// runtime_setProfLabel should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/localsession
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname runtime_setProfLabel runtime/pprof.runtime_setProfLabel
func runtime_setProfLabel(labels unsafe.Pointer) {
	// Introduce race edge for read-back via profile.
	// This would more properly use &getg().labels as the sync address,
	// but we do the read in a signal handler and can't call the race runtime then.
	//
	// This uses racereleasemerge rather than just racerelease so
	// the acquire in profBuf.read synchronizes with *all* prior
	// setProfLabel operations, not just the most recent one. This
	// is important because profBuf.read will observe different
	// labels set by different setProfLabel operations on
	// different goroutines, so it needs to synchronize with all
	// of them (this wouldn't be an issue if we could synchronize
	// on &getg().labels since we would synchronize with each
	// most-recent labels write separately.)
	//
	// racereleasemerge is like a full read-modify-write on
	// labelSync, rather than just a store-release, so it carries
	// a dependency on the previous racereleasemerge, which
	// ultimately carries forward to the acquire in profBuf.read.
	if raceenabled {
		racereleasemerge(unsafe.Pointer(&labelSync))
	}
	getg().labels = labels
}

// runtime_getProfLabel should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/localsession
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname runtime_getProfLabel runtime/pprof.runtime_getProfLabel
func runtime_getProfLabel() unsafe.Pointer {
	return getg().labels
}

// runtime_profLabelGet is the runtime/pprof-accessible wrapper for profLabelGet.
//
//go:linkname runtime_profLabelGet runtime/pprof.runtime_profLabelGet
func runtime_profLabelGet() unsafe.Pointer {
	return unsafe.Pointer(profLabelGet())
}

// runtime_profLabelRelease is the runtime/pprof-accessible wrapper for profLabelRelease.
//
//go:linkname runtime_profLabelRelease runtime/pprof.runtime_profLabelRelease
func runtime_profLabelRelease(p unsafe.Pointer) {
	profLabelRelease((*profLabelMap)(p))
}
