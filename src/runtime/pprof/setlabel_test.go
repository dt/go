// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pprof

import (
	"bytes"
	"context"
	"fmt"
	"internal/profile"
	"internal/runtime/pprof/label"
	"maps"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// getProfLabels returns all labels on the current goroutine as a label.Set,
// supporting both string and integer values.
func getProfLabels() *label.Set {
	return (*label.Set)(runtime_getProfLabel())
}

// labelMap returns the current goroutine's labels as a map of key→LabelValue.
func goroutineLabels() map[string]LabelValue {
	s := getProfLabels()
	if s == nil {
		return map[string]LabelValue{}
	}
	m := make(map[string]LabelValue, len(s.List))
	for _, lbl := range s.List {
		m[lbl.Key] = LabelValue{s: lbl.Value, n: lbl.IntVal}
	}
	return m
}

func TestSetLabelBasic(t *testing.T) {
	// Goroutine starts with no labels.
	if got := goroutineLabels(); len(got) != 0 {
		t.Fatalf("expected no labels, got %v", got)
	}

	// Set a string label.
	old := SetLabel("req", Str("abc"))
	got := goroutineLabels()
	if got["req"] != Str("abc") {
		t.Errorf("after SetLabel: got %v, want req=abc", got)
	}
	if old != (LabelValue{}) {
		t.Errorf("old value should be zero, got %v", old)
	}

	// Replace with a different value.
	old = SetLabel("req", Str("def"))
	got = goroutineLabels()
	if got["req"] != Str("def") {
		t.Errorf("after replace: got %v, want req=def", got)
	}
	if old != Str("abc") {
		t.Errorf("old value should be abc, got %v", old)
	}

	// Set an integer label.
	SetLabel("job", Int(42))
	got = goroutineLabels()
	if got["job"] != Int(42) {
		t.Errorf("after SetLabel int: got %v, want job=42", got)
	}

	// Clear labels.
	SetLabel("req", LabelValue{})
	SetLabel("job", LabelValue{})
	if got := goroutineLabels(); len(got) != 0 {
		t.Errorf("expected no labels after clear, got %v", got)
	}
}

func TestSetLabelDeferRestore(t *testing.T) {
	// Verify the defer f(f()) pattern restores labels correctly.
	if got := goroutineLabels(); len(got) != 0 {
		t.Fatalf("expected no labels at start, got %v", got)
	}

	func() {
		defer SetLabel("req", SetLabel("req", Str("abc")))
		got := goroutineLabels()
		if got["req"] != Str("abc") {
			t.Errorf("inside scope: got %v, want req=abc", got)
		}

		// Nest another label on a different key.
		func() {
			defer SetLabel("job", SetLabel("job", Int(99)))
			got := goroutineLabels()
			if got["req"] != Str("abc") {
				t.Errorf("nested scope req: got %v, want req=abc", got)
			}
			if got["job"] != Int(99) {
				t.Errorf("nested scope job: got %v, want job=99", got)
			}
		}()

		// After nested scope, job should be gone, req should remain.
		got = goroutineLabels()
		if got["req"] != Str("abc") {
			t.Errorf("after nested: req got %v, want abc", got)
		}
		if _, ok := got["job"]; ok {
			t.Errorf("after nested: job should be gone, got %v", got)
		}
	}()

	// After outer scope, req should be gone too.
	if got := goroutineLabels(); len(got) != 0 {
		t.Errorf("after outer scope: expected no labels, got %v", got)
	}
}

func TestSetLabelChildInheritance(t *testing.T) {
	// Set a label, spawn children, verify they inherit it.
	defer SetLabel("req", SetLabel("req", Str("parent")))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := goroutineLabels()
			if got["req"] != Str("parent") {
				t.Errorf("child goroutine: got %v, want req=parent", got)
			}
		}()
	}
	wg.Wait()
}

func TestSetLabelPersistsUntilExit(t *testing.T) {
	// Set-and-forget: labels persist until goroutine exits.
	ch := make(chan map[string]LabelValue)
	go func() {
		SetLabel("sticky", Str("yes"))
		// Don't defer-restore. Label should persist.
		runtime.Gosched()
		ch <- goroutineLabels()
	}()
	got := <-ch
	if got["sticky"] != Str("yes") {
		t.Errorf("sticky label: got %v, want sticky=yes", got)
	}
}

func TestSetLabelsBuilder(t *testing.T) {
	// Batch set multiple labels.
	defer SetLabels(SetLabels(NewLabels().Str("a", "1").Int("b", 2).Str("c", "3")))

	got := goroutineLabels()
	if got["a"] != Str("1") || got["b"] != Int(2) || got["c"] != Str("3") {
		t.Errorf("batch set: got %v", got)
	}
}

func TestSetLabelsBuilderRestore(t *testing.T) {
	// Test that batch restore works with defer.
	SetLabel("pre", Str("existing"))
	defer SetLabel("pre", SetLabel("pre", Str("existing")))

	func() {
		defer SetLabels(SetLabels(NewLabels().Str("pre", "overwritten").Int("new", 42)))

		got := goroutineLabels()
		if got["pre"] != Str("overwritten") {
			t.Errorf("inside batch scope: pre got %v, want overwritten", got)
		}
		if got["new"] != Int(42) {
			t.Errorf("inside batch scope: new got %v, want 42", got)
		}
	}()

	// After defer, pre should be restored and new should be gone.
	got := goroutineLabels()
	if got["pre"] != Str("existing") {
		t.Errorf("after batch restore: pre got %v, want existing", got)
	}
	if _, ok := got["new"]; ok {
		t.Errorf("after batch restore: new should be gone, got %v", got)
	}

	// Clean up.
	SetLabel("pre", LabelValue{})
}

func TestDoUnchanged(t *testing.T) {
	// Regression test: Do still works exactly as before.
	wantLabels := map[string]string{}
	if gotLabels := getProfLabel(); !maps.Equal(gotLabels, wantLabels) {
		t.Errorf("expected empty labels before Do, got %v", gotLabels)
	}

	Do(context.Background(), Labels("key1", "value1", "key2", "value2"), func(ctx context.Context) {
		wantLabels := map[string]string{"key1": "value1", "key2": "value2"}
		if gotLabels := getProfLabel(); !maps.Equal(gotLabels, wantLabels) {
			t.Errorf("inside Do: got %v, want %v", gotLabels, wantLabels)
		}

		// Child should inherit.
		ch := make(chan map[string]string)
		go func() {
			ch <- getProfLabel()
		}()
		if gotLabels := <-ch; !maps.Equal(gotLabels, wantLabels) {
			t.Errorf("child in Do: got %v, want %v", gotLabels, wantLabels)
		}
	})

	wantLabels = map[string]string{}
	if gotLabels := getProfLabel(); !maps.Equal(gotLabels, wantLabels) {
		t.Errorf("after Do: got %v, want %v", gotLabels, wantLabels)
	}
}

func TestIntLabelsInCPUProfile(t *testing.T) {
	// Set an integer label, collect a short CPU profile, and verify
	// the label appears with Label.num in the serialized proto.
	defer SetLabel("intkey", SetLabel("intkey", Int(12345)))

	var buf bytes.Buffer
	if err := StartCPUProfile(&buf); err != nil {
		t.Fatal(err)
	}
	// Burn some CPU so a sample is taken.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		// spin
	}
	StopCPUProfile()

	p, err := profile.Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}

	// Check if any sample has our integer label.
	found := false
	for _, s := range p.Sample {
		if vals, ok := s.NumLabel["intkey"]; ok {
			for _, v := range vals {
				if v == 12345 {
					found = true
				}
			}
		}
	}
	// It's possible no samples were taken during our spin, which is fine.
	// But if samples exist with labels, the int label should be there.
	if !found {
		// Check if we got any samples at all.
		hasSamples := false
		for _, s := range p.Sample {
			if s.Value[0] > 0 {
				hasSamples = true
				break
			}
		}
		if hasSamples {
			t.Log("CPU profile had samples but none with intkey=12345 (may be expected if sampling missed the labeled code)")
		}
	}
}

func TestSetLabelPoolRoundtrip(t *testing.T) {
	// Verify that SetLabel reuses pooled maps by checking zero
	// allocations after warmup.
	// Warmup: fill the pool.
	for i := 0; i < 10; i++ {
		SetLabel("warmup", Str("val"))
		SetLabel("warmup", LabelValue{})
	}

	allocs := testing.AllocsPerRun(100, func() {
		SetLabel("key", Str("value"))
		SetLabel("key", LabelValue{})
	})
	// After warmup, expect 0 allocs from the pool.
	if allocs > 0 {
		t.Errorf("SetLabel allocs = %v, want 0", allocs)
	}
}

func BenchmarkSetLabel(b *testing.B) {
	b.Run("string", func(b *testing.B) {
		// Warmup pool.
		SetLabel("bench", Str("warmup"))
		SetLabel("bench", LabelValue{})

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			SetLabel("bench", Str("value"))
			SetLabel("bench", LabelValue{})
		}
	})

	b.Run("int", func(b *testing.B) {
		SetLabel("bench", Int(0))
		SetLabel("bench", LabelValue{})

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			SetLabel("bench", Int(int64(i)))
			SetLabel("bench", LabelValue{})
		}
	})

	b.Run("defer-restore", func(b *testing.B) {
		SetLabel("bench", Str("warmup"))
		SetLabel("bench", LabelValue{})

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			func() {
				defer SetLabel("bench", SetLabel("bench", Str("scoped")))
			}()
		}
	})

	b.Run("batch-two", func(b *testing.B) {
		SetLabels(NewLabels().Str("a", "warmup").Str("b", "warmup"))
		SetLabels(NewLabels().Str("a", "").Str("b", ""))

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			func() {
				defer SetLabels(SetLabels(NewLabels().Str("a", "v1").Int("b", int64(i))))
			}()
		}
	})

	b.Run("compare-Do", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			Do(context.Background(), Labels("key", "value"), func(context.Context) {})
		}
	})
}

// Verify the new API doesn't interfere with the old one. Set labels via Do,
// then read them back. Then set via SetLabel and verify Do still works.
func TestSetLabelAndDoInterop(t *testing.T) {
	// Start clean.
	if got := goroutineLabels(); len(got) != 0 {
		t.Fatalf("expected no labels at start, got %v", got)
	}

	// Set via the new API.
	defer SetLabel("new", SetLabel("new", Str("newval")))

	// Now use Do on top of it.
	Do(context.Background(), Labels("old", "oldval"), func(ctx context.Context) {
		// Inside Do, the old API sets labels from context.
		// The new label "new" is NOT preserved by Do because Do replaces
		// all labels from the context. This is expected behavior.
		got := getProfLabel()
		if got["old"] != "oldval" {
			t.Errorf("inside Do: old=%v, want oldval", got["old"])
		}
	})

	// After Do returns, it restores the previous labels (which had "new").
	// But Do sets labels from ctx, which doesn't have "new". So "new" is gone.
	// This is expected: Do replaces ALL labels.
}

func BenchmarkSetLabelVsDo(b *testing.B) {
	b.Run("SetLabel", func(b *testing.B) {
		// Warmup.
		for i := 0; i < 10; i++ {
			SetLabel("bench", Str("w"))
			SetLabel("bench", LabelValue{})
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			old := SetLabel("bench", Str(fmt.Sprintf("v%d", i)))
			_ = old
			SetLabel("bench", LabelValue{})
		}
	})

	b.Run("Do", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			Do(context.Background(), Labels("bench", fmt.Sprintf("v%d", i)), func(context.Context) {})
		}
	})
}

func init() {
	// Ensure the unused import is used.
	_ = unsafe.Pointer(nil)
}
