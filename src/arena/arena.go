// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package arena

import (
	"reflect"
	_ "unsafe" // for using linkname
)

// This should be fixed so that we are not adding to the external API of reflect. We
// can do by having 'arena' be a type exported by an internal package that both arena
// and reflect share. Or we can do this by having 'arena' be two
// identically-structured types in arena and reflect, and using linkname to switch
// the reference from one type to the other in the linknamed functions.
type arena = reflect.XXX_Arena

type Arena struct {
	a *arena
}

// New allocates a new arena.
func New() *Arena {
	return &Arena{a: reflect_newArena()}
}

// Free frees the arena (and all objects allocated from the arena) so that
// memory backing the arena can be reused fairly quickly without garbage
// collection overhead.  Applications must not call any method on this
// arena after it has been freed.
func (a *Arena) Free() {
	reflect_freeArena(a.a)
	a.a = nil
}

// New allocates an object from arena a.  If the concrete type of objPtrPtr is
// a pointer to a pointer to type T (**T), New allocates an object of type
// T and stores a pointer to the object in *objPtrPtr.  The object must not
// be accessed after arena a is freed.
func (a *Arena) New(objPtrPtr interface{}) {
	ptrValue := reflect.ValueOf(objPtrPtr).Elem()
	ptrValue.Set(reflect.ValueOf(reflect_arenaNew(a.a, ptrValue.Type().Elem())))
}

// NewReflectType allocates an object of type typ from arena a. It is an alternate
// API taking a reflect.Type argument to specify the type.
func (a *Arena) NewReflectType(typ reflect.Type) interface{} {
	return reflect_arenaNew(a.a, typ)
}

// NewSlice allocates a slice from arena a.  If the concrete type of slicePtr
// is *[]T, NewSlice creates a slice of element type T with the specified
// capacity whose backing store is from the arena a and stores it in
// *slicePtr. The length of the slice is set to the capacity.  The slice must
// not be accessed after arena a is freed.
func (a *Arena) Slice(slicePtr interface{}, cap int) {
	reflect_arenaSlice(a.a, slicePtr, cap)
}

// HeapString returns a copy of the input string, and the returned copy
// is allocated from the heap, not from any arena. If s is already allocated
// from the heap, then the implementation may return exactly s.  This function
// is useful in some situations where the application code is unsure if s
// is allocated from an arena.
func HeapString(s string) string {
	return reflect_heapString(s)
}

//go:linkname reflect_newArena
func reflect_newArena() *arena

//go:linkname reflect_freeArena
func reflect_freeArena(a *arena)

//go:linkname reflect_arenaNew
func reflect_arenaNew(a *arena, typ reflect.Type) interface{}

//go:linkname reflect_arenaSlice
func reflect_arenaSlice(a *arena, res interface{}, cap int)

//go:linkname reflect_heapString
func reflect_heapString(s string) string
