// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pprof

import (
	"context"
	"fmt"
	"internal/runtime/pprof/label"
	"slices"
	"strings"
	"unsafe"
)

// LabelSet is a set of labels.
type LabelSet struct {
	list []label.Label
}

// labelContextKey is the type of contextKeys used for profiler labels.
type labelContextKey struct{}

func labelValue(ctx context.Context) labelMap {
	labels, _ := ctx.Value(labelContextKey{}).(*labelMap)
	if labels == nil {
		return labelMap{}
	}
	return *labels
}

// labelMap is the representation of the label set held in the context type.
// This is an initial implementation, but it will be replaced with something
// that admits incremental immutable modification more efficiently.
//
// The layout must be compatible with profLabelMap in runtime/proflabel.go:
// label.Set first, then next pointer, then refs. For non-pooled labelMaps,
// next is nil and refs is 0, which tells profLabelRelease to skip them.
type labelMap struct {
	label.Set
	next unsafe.Pointer // unused; layout compat with profLabelMap
	refs int64          // always 0 for non-pooled maps
}

// String satisfies Stringer and returns key, value pairs in a consistent
// order.
func (l *labelMap) String() string {
	if l == nil {
		return ""
	}
	keyVals := make([]string, 0, len(l.Set.List))

	for _, lbl := range l.Set.List {
		keyVals = append(keyVals, fmt.Sprintf("%q:%q", lbl.Key, lbl.Value))
	}

	slices.Sort(keyVals)
	return "{" + strings.Join(keyVals, ", ") + "}"
}

// WithLabels returns a new [context.Context] with the given labels added.
// A label overwrites a prior label with the same key.
func WithLabels(ctx context.Context, labels LabelSet) context.Context {
	parentLabels := labelValue(ctx)
	return context.WithValue(ctx, labelContextKey{}, &labelMap{Set: mergeLabelSets(parentLabels.Set, labels)})
}

func mergeLabelSets(left label.Set, right LabelSet) label.Set {
	if len(left.List) == 0 {
		return label.NewSet(right.list)
	} else if len(right.list) == 0 {
		return left
	}

	lList, rList := left.List, right.list
	l, r := 0, 0
	result := make([]label.Label, 0, len(rList))
	for l < len(lList) && r < len(rList) {
		switch strings.Compare(lList[l].Key, rList[r].Key) {
		case -1: // left key < right key
			result = append(result, lList[l])
			l++
		case 1: // right key < left key
			result = append(result, rList[r])
			r++
		case 0: // keys are equal, right value overwrites left value
			result = append(result, rList[r])
			l++
			r++
		}
	}

	// Append the remaining elements
	result = append(result, lList[l:]...)
	result = append(result, rList[r:]...)

	return label.NewSet(result)
}

// Labels takes an even number of strings representing key-value pairs
// and makes a [LabelSet] containing them.
// A label overwrites a prior label with the same key.
// Currently only the CPU and goroutine profiles utilize any labels
// information.
// See https://golang.org/issue/23458 for details.
func Labels(args ...string) LabelSet {
	if len(args)%2 != 0 {
		panic("uneven number of arguments to pprof.Labels")
	}
	list := make([]label.Label, 0, len(args)/2)
	sortedNoDupes := true
	for i := 0; i+1 < len(args); i += 2 {
		list = append(list, label.Label{Key: args[i], Value: args[i+1]})
		sortedNoDupes = sortedNoDupes && (i < 2 || args[i] > args[i-2])
	}
	if !sortedNoDupes {
		// slow path: keys are unsorted, contain duplicates, or both
		slices.SortStableFunc(list, func(a, b label.Label) int {
			return strings.Compare(a.Key, b.Key)
		})
		deduped := make([]label.Label, 0, len(list))
		for i, lbl := range list {
			if i == 0 || lbl.Key != list[i-1].Key {
				deduped = append(deduped, lbl)
			} else {
				deduped[len(deduped)-1] = lbl
			}
		}
		list = deduped
	}
	return LabelSet{list: list}
}

// Label returns the value of the label with the given key on ctx, and a boolean indicating
// whether that label exists.
func Label(ctx context.Context, key string) (string, bool) {
	ctxLabels := labelValue(ctx)
	for _, lbl := range ctxLabels.Set.List {
		if lbl.Key == key {
			return lbl.Value, true
		}
	}
	return "", false
}

// ForLabels invokes f with each label set on the context.
// The function f should return true to continue iteration or false to stop iteration early.
func ForLabels(ctx context.Context, f func(key, value string) bool) {
	ctxLabels := labelValue(ctx)
	for _, lbl := range ctxLabels.Set.List {
		if !f(lbl.Key, lbl.Value) {
			break
		}
	}
}

// LabelValue is an opaque value for use with [SetLabel]. It can hold either
// a string or an integer. Use [Str] to create a string value and [Int] to
// create an integer value. The zero value represents an empty/absent label.
//
// LabelValue is a value type (24 bytes) with unexported fields. Callers can
// create values and receive old ones back from [SetLabel], but cannot inspect
// what was returned — they can only pass it back to [SetLabel] for restore.
type LabelValue struct {
	s string
	n int64
}

// Str returns a [LabelValue] holding the string s.
func Str(s string) LabelValue {
	return LabelValue{s: s}
}

// Int returns a [LabelValue] holding the integer n. The value will be
// serialized as a pprof Label.num field, avoiding the cost of formatting
// the integer to a string at labeling time.
func Int(n int64) LabelValue {
	return LabelValue{n: n}
}

// SetLabel sets a single profiling label on the current goroutine and returns
// the previous value for that key. Labels persist until replaced or the
// goroutine exits. Child goroutines inherit the parent's labels.
//
// The returned previous value enables scoped label restore via defer:
//
//	defer pprof.SetLabel("req", pprof.SetLabel("req", pprof.Str(reqID)))
//
// The inner call sets the label and returns the old value. The deferred outer
// call restores it. All values are passed and returned by value — no closures,
// no heap allocations (after pool warmup).
func SetLabel(key string, val LabelValue) LabelValue {
	cur := (*label.Set)(runtime_getProfLabel())

	// Find old value for this key.
	var old LabelValue
	if cur != nil {
		for _, lbl := range cur.List {
			if lbl.Key == key {
				old = LabelValue{s: lbl.Value, n: lbl.IntVal}
				break
			}
		}
	}

	// Claim a new pooled labelMap and populate it.
	newMap := (*label.Set)(runtime_profLabelGet())
	if cur != nil {
		// Copy existing labels, replacing the key if found.
		found := false
		for _, lbl := range cur.List {
			if lbl.Key == key {
				found = true
				if val.s != "" || val.n != 0 {
					newMap.List = append(newMap.List, label.Label{Key: key, Value: val.s, IntVal: val.n})
				}
				// else: zero LabelValue means remove the label
			} else {
				newMap.List = append(newMap.List, lbl)
			}
		}
		if !found && (val.s != "" || val.n != 0) {
			newMap.List = append(newMap.List, label.Label{Key: key, Value: val.s, IntVal: val.n})
		}
	} else if val.s != "" || val.n != 0 {
		newMap.List = append(newMap.List, label.Label{Key: key, Value: val.s, IntVal: val.n})
	}

	// Release the old map and install the new one.
	runtime_profLabelRelease(unsafe.Pointer(cur))
	runtime_setProfLabel(unsafe.Pointer(newMap))

	return old
}

// LabelsBuilder accumulates labels for a batch [SetLabels] call.
// Use [NewLabels] to create a builder.
type LabelsBuilder struct {
	labels []label.Label
}

// NewLabels returns a new [LabelsBuilder] for constructing a batch label set.
func NewLabels() LabelsBuilder {
	return LabelsBuilder{}
}

// Str adds a string label to the builder and returns the builder for chaining.
func (b LabelsBuilder) Str(key, val string) LabelsBuilder {
	b.labels = append(b.labels, label.Label{Key: key, Value: val})
	return b
}

// Int adds an integer label to the builder and returns the builder for chaining.
func (b LabelsBuilder) Int(key string, val int64) LabelsBuilder {
	b.labels = append(b.labels, label.Label{Key: key, IntVal: val})
	return b
}

// SetLabels sets all labels in the builder on the current goroutine in a
// single operation, and returns a builder containing the old values for
// all keys that were set. This enables scoped batch restore via defer:
//
//	defer pprof.SetLabels(pprof.SetLabels(
//	    pprof.NewLabels().Str("req", reqID).Int("job", 123),
//	))
func SetLabels(b LabelsBuilder) LabelsBuilder {
	cur := (*label.Set)(runtime_getProfLabel())

	// Build the restore builder with old values for each key.
	var restore LabelsBuilder
	for _, blbl := range b.labels {
		var found bool
		if cur != nil {
			for _, lbl := range cur.List {
				if lbl.Key == blbl.Key {
					restore.labels = append(restore.labels, lbl)
					found = true
					break
				}
			}
		}
		if !found {
			// Key was not previously set; restore to zero (removal).
			restore.labels = append(restore.labels, label.Label{Key: blbl.Key})
		}
	}

	// Claim a new pooled labelMap and populate it.
	newMap := (*label.Set)(runtime_profLabelGet())

	// Copy existing labels, replacing any that match new keys.
	if cur != nil {
		for _, lbl := range cur.List {
			if newLbl, found := builderFind(b, lbl.Key); found {
				if newLbl.Value != "" || newLbl.IntVal != 0 {
					newMap.List = append(newMap.List, newLbl)
				}
				// else: zero value means remove
			} else {
				newMap.List = append(newMap.List, lbl)
			}
		}
	}
	// Add any new keys that weren't in the existing set.
	for _, lbl := range b.labels {
		if lbl.Value == "" && lbl.IntVal == 0 {
			continue // removal, already handled above
		}
		if cur == nil || !setHasKey(cur, lbl.Key) {
			newMap.List = append(newMap.List, lbl)
		}
	}

	// Release the old map and install the new one.
	runtime_profLabelRelease(unsafe.Pointer(cur))
	runtime_setProfLabel(unsafe.Pointer(newMap))

	return restore
}

// builderFind searches b for a label with the given key. Linear scan;
// label sets are typically small (< 10 keys).
func builderFind(b LabelsBuilder, key string) (label.Label, bool) {
	for _, lbl := range b.labels {
		if lbl.Key == key {
			return lbl, true
		}
	}
	return label.Label{}, false
}

// setHasKey reports whether s contains a label with the given key.
func setHasKey(s *label.Set, key string) bool {
	for _, lbl := range s.List {
		if lbl.Key == key {
			return true
		}
	}
	return false
}
