// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package label provides common declarations used by both the [runtime] and [runtime/pprof] packages.
// The [Set] type is used for goroutine labels, and is duplicated as
// [runtime/pprof.LabelSet]. The type is duplicated due to go.dev/issue/65437
// preventing the use of a type-alias in an existing public interface.
package label

// Label is a key/value pair. A label holds either a string value (Value)
// or an integer value (IntVal). When IntVal is non-zero and Value is empty,
// the label is treated as an integer label.
type Label struct {
	Key    string
	Value  string
	IntVal int64
}

// Set is a set of labels.
type Set struct {
	List []Label
}

// NewSet constructs a LabelSet that wraps the provided labels.
func NewSet(list []Label) Set {
	return Set{List: list}
}
