// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package arena_test

import (
	"arena"
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

type T struct {
	n int
}

func TestSmoke(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("Only supporting linux-amd64")
	}
	a := arena.New()
	defer a.Free()

	var tt *T
	a.New(&tt)
	//tt := a.NewReflectType(reflect.TypeOf(T{})).(*T)
	tt.n = 1

	var ts []T
	a.Slice(&ts, 100)
	if len(ts) != 100 {
		t.Errorf("Slice() len = %d, want 100", len(ts))
	}
	if cap(ts) != 100 {
		t.Errorf("Slice() cap = %d, want 100", cap(ts))
	}
	ts[1].n = 42
}

func TestLarge(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("Only supporting linux-amd64")
	}

	type T [1 << 20]byte // 1MiB

	a := arena.New()
	for i := 0; i < 10*64; i++ {
		var q *T
		a.New(&q)
		//_ = a.NewReflectType(reflect.TypeOf(T{})).(*T)
	}
	a.Free()
}

func TestHeapString(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("Only supporting linux-amd64")
	}

	a := arena.New()

	// A static string (not on heap or arena)
	var s = "abcdefghij"

	// Create a byte slice in the arena, initialize it with s
	var b []byte
	a.Slice(&b, len(s))
	copy(b, s)

	// Create a string as using the same memory as the byte slice, hence in
	// the arena. This could be an arena API, but hasn't really been needed
	// yet.
	var as string
	asHeader := (*reflect.StringHeader)(unsafe.Pointer(&as))
	asHeader.Data = (*reflect.SliceHeader)(unsafe.Pointer(&b)).Data
	asHeader.Len = len(b)

	// HeapString should make a copy of as, since it is in the arena.
	asCopy := arena.HeapString(as)
	if (*reflect.StringHeader)(unsafe.Pointer(&as)).Data == (*reflect.StringHeader)(unsafe.Pointer(&asCopy)).Data {
		t.Fatal("HeapString did not make a copy")
	}

	// HeapString should make a copy of subAs, since subAs is just part of as and so is in the arena.
	subAs := as[1:3]
	subAsCopy := arena.HeapString(subAs)
	if (*reflect.StringHeader)(unsafe.Pointer(&subAs)).Data == (*reflect.StringHeader)(unsafe.Pointer(&subAsCopy)).Data {
		t.Fatal("HeapString did not make a copy")
	}

	// HeapString should not make a copy of doubleAs, since doubleAs will be on the heap.
	doubleAs := as + as
	doubleAsCopy := arena.HeapString(doubleAs)
	if (*reflect.StringHeader)(unsafe.Pointer(&doubleAs)).Data != (*reflect.StringHeader)(unsafe.Pointer(&doubleAsCopy)).Data {
		t.Fatal("HeapString should not have made a copy")
	}

	// HeapString should not make a copy of s, since s is a static string.
	sCopy := arena.HeapString(s)
	if (*reflect.StringHeader)(unsafe.Pointer(&s)).Data != (*reflect.StringHeader)(unsafe.Pointer(&sCopy)).Data {
		t.Fatal("HeapString should not have made a copy")
	}

	a.Free()
}
