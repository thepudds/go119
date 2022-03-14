// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect

import (
	"internal/unsafeheader"
	"runtime"
	"sync"
	"unsafe"
)

type XXX_Arena = arena

// Object representing the overall arena
type arena struct {
	chunkSize     uintptr
	chunkListHead *arenaChunkHeader
}

// Header for each arena chunk (which is 64 MB for checked arenas)
type arenaChunkHeader struct {
	off  uintptr // start of allocation area, relative to &arena
	size uintptr // size of allocation area
	// For checked arenas, last process that this batched chunk was used on.
	lastP uintptr
	// Unused
	freeTime uintptr
	// Unused
	numGcs uintptr
	pad1   uintptr
	pad2   uintptr
	// Make sure that we have a pointer field that is last in struct (so Go
	// runtime sets the scan bits up to the point where the chunk allocation
	// memory starts.
	next *arenaChunkHeader // next chunk in list
	// The memory that is being allocated in the chunk immediately follows the
	// last field.
}

// Test option where we batch arenas, but arena chunks are reclaimed normally by
// the GC, rather than being explicitly freed and unmapped.
const nofree = false

// Same size as the GC arena size, so fixed at 64 Mb
const checkedArenaChunkSize = 64 * 1048576

const chunkHeaderSize = unsafe.Sizeof(arenaChunkHeader{})

var (
	arenaChunkIface = interface{}(arenaChunkHeader{})
	arenaChunkType  = (*emptyInterface)(unsafe.Pointer(&arenaChunkIface)).typ

	arenaFreeChunks    *arenaChunkHeader
	arenaFreeChunksEnd *arenaChunkHeader
	arenaFreeCount     int        // Number of chunks on arenaFreeChunks list
	mu                 sync.Mutex // Lock for the arenaFreeChunks list

	arenaNumNews       int
	arenaNumFrees      int
	arenaClearFreeList int
)

// makeNewArenaChunk creates a new arena chunk with the same size as arena a, and adds it
// to the head of the linked list of a.
func makeNewArenaChunk(a *arena) {
	// Copy the type descriptor for arena.
	typ := *arenaChunkType
	// Make it exactly the size of a.chunkSize
	typ.size = a.chunkSize
	typ.str = 0       // tracealloc might want the name of this type; the offset is invalid.
	typ.ptrToThis = 0 // TODO:needed???
	// Get rid of uncommon, extraStar, named.
	typ.tflag = 0

	newChunk := (*arenaChunkHeader)(unsafe_newUserArenaChunk(&typ))
	newChunk.off = chunkHeaderSize
	newChunk.size = a.chunkSize - newChunk.off
	newChunk.next = a.chunkListHead
	a.chunkListHead = newChunk
}

func getNewArenaChunk(a *arena, objAlloc bool) {
	// For safe (checked) arenas, free list is only for the batched first
	// chunk of an arena. Any further chunks needed for the chunk should be
	// new (fully empty) chunks.
	// arenaFreeChunks check without lock is an optimization, but racy.  Can
	// be eliminated to satisfy TSAN.
	if objAlloc || arenaFreeChunks == nil {
		makeNewArenaChunk(a)
		return
	}

	mu.Lock()
	if arenaFreeChunks == nil {
		mu.Unlock()
		makeNewArenaChunk(a)
		return
	}

	var prev *arenaChunkHeader
	// Look for a free chunk from arenaFreeChunks whose lastP
	// is the same as our current P. If there isn't one with a
	// matching P, then just grab the first chunk on the list.
	myP := uintptr(unsafe_myP())
	for p := arenaFreeChunks; p != nil; prev, p = p, p.next {
		if p.lastP == myP {
			if prev == nil {
				break
			}
			arenaFreeCount--
			prev.next = p.next
			p.next = a.chunkListHead
			a.chunkListHead = p
			mu.Unlock()
			return
		}
	}
	arenaFreeCount--
	next := arenaFreeChunks
	arenaFreeChunks = next.next
	next.next = a.chunkListHead // i.e. nil
	a.chunkListHead = next
	mu.Unlock()
}

// NewArena creates an arena that automatically grows in fixed-sized chunks,
// and is freeable.
//go:linkname newArena arena.reflect_newArena
func newArena() *arena {
	bytes := checkedArenaChunkSize
	// Should change to be atomic to satisfy TSAN.
	ct := arenaNumNews
	arenaNumNews++
	if ct%100000 == 0 {
		println("Arena free list size", arenaFreeCount)
	}
	a := &arena{}
	a.chunkSize = uintptr(bytes)
	runtime.SetFinalizer(a, func(a *arena) {
		// If arena handle is dropped without being freed, then call
		// freeArena on the arena, so the arena chunks are never reclaimed
		// by the garbage collector.
		freeArena(a)
	})

	getNewArenaChunk(a, false)
	return a
}

// FreeArena marks all allocations of the arena up to this point as free.
// Any completely full chunks are freed immediately, but the current chunk may not
// be freed until much later (when full).
//go:nocheckptr
//go:linkname freeArena arena.reflect_freeArena
func freeArena(a *arena) {
	runtime.SetFinalizer(a, nil)

	freeAll := !nofree && a.chunkListHead.off >= 1<<23 // 8 MB
	var p *arenaChunkHeader
	switch {
	case freeAll:
		// thepudds: we have used some of the active arena chunk beyond our threshold of 8MB.
		// Free it even if it is not yet full along with any other chunks for this arena.
		// This helps reduce our RSS at a modest cost of CPU compared to waiting for a full 64MB.
		p = a.chunkListHead
	case nofree:
		// Instead of freeing/unmapping the full chunks, just drop a
		// pointer to them so they will get reclaimed by GC.
		p = nil
	default:
		// Skip the active arena chunk, see if there are any completed blocks
		p = a.chunkListHead.next
		a.chunkListHead.next = nil
	}

	for p != nil {
		next := p.next
		p.next = nil
		//println("Unmapping", p, siz-(p.off+p.size))
		off := p.off
		size := p.size
		ap := uintptr(unsafe.Pointer(p))
		p = nil
		unsafe_unmapUserArenaChunk(ap, a.chunkSize, off+size)
		p = next
	}

	if !freeAll {
		mu.Lock()
		// Save the current unfull chunk on the free chunk list
		a.chunkListHead.lastP = uintptr(unsafe_myP())
		a.chunkListHead.next = arenaFreeChunks
		arenaFreeChunks = a.chunkListHead
		arenaFreeCount++
		mu.Unlock()
	}
}

var zeroBase uint64

func (a *arena) reservePointer(size uintptr, align uint8) unsafe.Pointer {
	if size == 0 {
		// Don't return a pointer to a zero-sized object in the arena,
		// since we could be exactly at the end of the arena, in which
		// case we might return an invalid pointer right past the end of
		// the arena data.
		return unsafe.Pointer(&zeroBase)
	}
	chunk := a.chunkListHead
	// TODO: Atomic to make this thread-safe. (Otherwise this is
	// an easy way to escape memory safety, even though
	// technically we could call it a race.)
	off := chunk.off
	s := chunk.size

	// Allocate from the beginning.
	pad := (-off) & (uintptr(align) - 1) // alignment padding
	size += pad
	if size > s {
		return nil
	}
	chunk.off = off + size
	chunk.size = s - size
	alignedOff := off + pad
	p := add(unsafe.Pointer(chunk), alignedOff, "intra-arena")
	if alignedOff&(1<<21-1) == 0 || alignedOff>>21 != (chunk.off-1)>>21 {
		// thepudds: in some cases, we were not getting huge pages
		// backing user arenas on linux. Touching the pages prior
		// to returning individual pointers to the user seems
		// to result in huge pages, which can have material performance
		// impact. To reduce the hit on RSS, we touch in 2MB pieces.
		// Here, we are at the start of a 2MB piece of the 64MB arena chunk,
		// or the ptr we are about to return straddles a 2MB boundary.
		if alignedOff+(1<<21) > 1<<26 {
			panic("memclr beyond arena chunk size")
		}
		memclrNoHeapPointers(p, 1<<21) // 2MB
	}
	return p
}

func (a *arena) reserveScalar(size uintptr, align uint8) unsafe.Pointer {
	if size == 0 {
		return unsafe.Pointer(&zeroBase)
	}
	chunk := a.chunkListHead
	off := chunk.off
	s := chunk.size
	// Allocate from the end.
	pad := (off + s) & (uintptr(align) - 1) // alignment padding
	size += pad
	if size > s {
		return nil
	}
	chunk.size = s - size
	return add(unsafe.Pointer(chunk), off+s-size, "intra-arena")
}

// reserveSpace reserves space in the arena for an item of the specified
// type. If cap is not -1, this is for an array of cap elements of type t.
func (a *arena) reserveSpace(rt *rtype, cap int) unsafe.Pointer {
	size := rt.size
	if cap >= 0 {
		size *= uintptr(cap) // TODO: Check for overflow
	}
	var ptr unsafe.Pointer
	if rt.pointers() {
		ptr = a.reservePointer(size, rt.align)
		if ptr != nil {
			if cap >= 0 {
				unsafe_newArrayAt(rt, cap, ptr)
			} else {
				unsafe_newAt(rt, ptr)
			}
		}
	} else {
		ptr = a.reserveScalar(size, rt.align)
	}
	return ptr
}

//go:linkname arenaNew arena.reflect_arenaNew
func arenaNew(a *arena, typ Type) interface{} {
	return a.New(typ)
}

// arenaNew allocates a value of type typ and returns a pointer to it.  If there
// isn't enough space left in the arena, returns nil.
func (a *arena) New(typ Type) interface{} {
	if typ == nil {
		panic("reflect: arena.New(nil)")
	}
	rt := typ.(*rtype)
	ptr := a.reserveSpace(rt, -1)
	if ptr == nil {
		getNewArenaChunk(a, true)
		ptr = a.reserveSpace(rt, -1)
		if ptr == nil {
			return nil
		}

	}
	i := emptyInterface{
		typ:  rt.ptrTo(),
		word: ptr,
	}
	return *(*interface{})(unsafe.Pointer(&i))
}

//go:linkname arenaSlice arena.reflect_arenaSlice
func arenaSlice(a *arena, res interface{}, cap int) {
	a.Slice(res, cap)
}

// res must be *[]T.  Allocates an array a = [cap]T, assigns *res = a[:].
// The weird argument convention is so the storage for the slice header
// can be allocated in the caller's stack frame instead of on the heap.
// If the array won't fit in the arena, returns without allocating or assigning anything.
func (a *arena) Slice(res interface{}, cap int) {
	if cap < 0 {
		panic("reflect.arena.MakeSlice: negative cap")
	}
	i := (*emptyInterface)(unsafe.Pointer(&res))
	t := i.typ
	if t.Kind() != Ptr {
		panic("reflect.arena.Slice result of non-ptr type")
	}
	t = (*ptrType)(unsafe.Pointer(t)).elem
	if t.Kind() != Slice {
		panic("reflect.arena.Slice of non-ptr-to-slice type")
	}
	t = (*sliceType)(unsafe.Pointer(t)).elem
	// t is now the element type of the slice we want to allocate.

	ptr := a.reserveSpace(t, cap)
	if ptr == nil {
		getNewArenaChunk(a, true)
		ptr = a.reserveSpace(t, cap)
	}
	if ptr != nil {
		*(*unsafeheader.Slice)(i.word) = unsafeheader.Slice{Data: ptr, Len: cap, Cap: cap}
	}
}

//go:linkname heapString arena.reflect_heapString
func heapString(s string) string {
	hdr := (*StringHeader)(unsafe.Pointer(&s))
	if !unsafe_inArena(hdr.Data) {
		return s
	}
	return string([]byte(s))
}

// implemented in package runtime
//go:noescape
func unsafe_newAt(*rtype, unsafe.Pointer)

//go:noescape
func unsafe_newArrayAt(*rtype, int, unsafe.Pointer)

//go:noescape
func unsafe_clearHeapBits(unsafe.Pointer, uintptr)

//go:noescape
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr)

//go:noescape
func unsafe_newUserArenaChunk(*rtype) unsafe.Pointer

//go:noescape
func unsafe_unmapUserArenaChunk(ap uintptr, siz uintptr, bitsSize uintptr)

func unsafe_myP() int32

func unsafe_inArena(ptr uintptr) bool
