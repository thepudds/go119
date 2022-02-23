// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

//go:linkname reflect_unsafe_newAt reflect.unsafe_newAt
func reflect_unsafe_newAt(typ *_type, ptr unsafe.Pointer) {
	h := heapBitsForAddr(uintptr(ptr))
	p := typ.gcdata // start of 1-bit pointer mask (or GC program)
	if typ.kind&kindGCProg != 0 {
		// Expand gc program, using the object itself for storage.
		runGCProg(p, nil, (*byte)(ptr), int(typ.size))
		p = (*byte)(ptr)
	}
	nw := typ.size / goarch.PtrSize
	nb := typ.ptrdata / goarch.PtrSize

	// The following code is an unrolled version of this:
	//
	// for i := uintptr(0); i < nb; i++ {
	//   h.setBits(*addb(p, i/8)>>(i%8)&1 != 0)
	//   h = h.next()
	// }

	i := uintptr(0)
	if nb >= 10 {
		for h.shift != 0 {
			h.setBits(*addb(p, i/8)>>(i%8)&1 != 0)
			i++
			h = h.next()
		}
		for nb-i >= 4 {
			var nextBits byte
			var mod = i % 8
			if mod < 5 {
				nextBits = (*addb(p, i/8) >> mod) & 15
			} else {
				nextBits = byte(((int32(*addb(p, i/8)) + (int32(*addb(p, i/8+1)) << 8)) >> mod) & 15)
			}
			*h.bitp = bitScanAll | nextBits
			h.shift = 3
			i += 4
			h = h.next()
		}
	}
	for i < nb {
		h.setBits(*addb(p, i/8)>>(i%8)&1 != 0)
		i++
		if i < nw {
			// Avoid calling h.next() if we don't have any more bits to set
			// Optimizes the case of 1 or a few pointers.
			h = h.next()
		}
	}

	// Set the rest of the scan bits since we may place more
	// pointer-ful data after this.
	if nw > nb {
		i := nb
		for {
			h.setBits(false)
			i++
			if i >= nw {
				break
			}
			h = h.next()
		}
	}

	if typ.kind&kindGCProg != 0 {
		// Zero out temporary ptrmask buffer inside object.
		memclrNoHeapPointers(ptr, nw/wordsPerBitmapByte)
	}
	mp := acquirem()
	mp.p.ptr().mcache.scanAlloc += typ.size
	releasem(mp)
}

//go:linkname reflect_unsafe_clearHeapBits reflect.unsafe_clearHeapBits
func reflect_unsafe_clearHeapBits(ptr unsafe.Pointer, size uintptr) {
	hb := heapBitsForAddr(uintptr(ptr))
	if hb.shift != 0 {
		panic("hb.shift != 0")
	}
	if size%(goarch.PtrSize*wordsPerBitmapByte) != 0 {
		panic("size%(goarch.PtrSize*wordsPerBitmapByte) != 0")
	}
	nb := (size / goarch.PtrSize) / wordsPerBitmapByte
	for nb > 0 {
		len := uintptr(unsafe.Pointer(hb.last)) + 1 - uintptr(unsafe.Pointer(hb.bitp))
		if nb <= len {
			memclrNoHeapPointers(unsafe.Pointer(hb.bitp), nb)
			break
		}
		memclrNoHeapPointers(unsafe.Pointer(hb.bitp), len)
		hb.bitp = hb.last
		hb.shift = 3
		hb = hb.next()
		nb -= len
	}
}

//go:linkname reflect_unsafe_newArrayAt reflect.unsafe_newArrayAt
func reflect_unsafe_newArrayAt(typ *_type, n int, ptr unsafe.Pointer) {
	mem, overflow := math.MulUintptr(typ.size, uintptr(n))
	if overflow || n < 0 || mem > maxAlloc {
		panic(plainError("runtime: allocation size out of range"))
	}
	for i := 0; i < n; i++ {
		reflect_unsafe_newAt(typ, add(ptr, uintptr(i)*typ.size))
	}
}

// Sets scan bit, and maybe ptr bit also.
func (h heapBits) setBits(ptr bool) {
	b := *h.bitp
	shft := h.shift & 7
	if ptr {
		b |= uint8(bitPointer|bitScan) << shft
	} else {
		// TODO: how bad is it that we're potentially clearing ptr bits?
		b &^= uint8(bitPointer) << shft
		b |= uint8(bitScan) << shft
	}
	*h.bitp = b
}

// unsafe_myP returns the id of the current P
//go:linkname reflect_unsafe_myP reflect.unsafe_myP
func reflect_unsafe_myP() int32 {
	_g_ := getg()
	mp := _g_.m
	return mp.p.ptr().id
}

// unsafe_newUserArenaChunk allocates a user arena chunk, which is 64M and exactly
// maps to a single heap arena and single span.
//go:linkname reflect_unsafe_newUserArenaChunk reflect.unsafe_newUserArenaChunk
func reflect_unsafe_newUserArenaChunk(typ *_type) unsafe.Pointer {
	// Derived from mallocgc()
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}
	size := typ.size
	if size == 0 || size&262143 != 0 || typ.ptrdata == 0 || size+_PageSize < size {
		throw("hi")
	}

	// Set mp.mallocing to keep from being preempted by GC.
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	c := getMCache(mp)
	if c == nil {
		throw("mallocgc called without a P or outside bootstrapping")
	}

	var span *mspan
	systemstack(func() {
		// expansion of:  s = c.allocLarge(size, true, false)
		npages := size >> _PageShift

		// Deduct credit for this span allocation and sweep if
		// necessary. mHeap_Alloc will also sweep npages, so this only
		// pays the debt down to npage pages.
		//deductSweepCredit(npages*_PageSize, npages)

		// expansion of: s = mheap_.alloc(npages, makeSpanClass(0, false), true)
		spc := makeSpanClass(0, false)
		span = mheap_.allocSpan(npages, spanAllocUserArena, spc)
		if span == nil {
			throw("out of memory")
		}
		if span.needzero != 0 {
			memclrNoHeapPointers(unsafe.Pointer(span.base()), span.npages<<_PageShift)
			span.needzero = 0
		}

		// Update heap_live and revise pacing if needed.
		atomic.Xadd64(&gcController.heapLive, int64(npages*pageSize))
		if gcBlackenEnabled != 0 {
			gcController.revise()
		}
		// Put the large span in the mcentral swept list so that it's
		// visible to the background sweeper.
		mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(span)

		span.limit = span.base() + size
		heapBitsForAddr(span.base()).initSpan(span)
	})
	span.freeindex = 1
	span.allocCount = 1
	x := unsafe.Pointer(span.base())
	size = span.elemsize
	heapBitsSetType(uintptr(x), size, size, typ)
	scanSize := typ.ptrdata
	c.scanAlloc += scanSize
	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	publicationBarrier()

	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	if gcphase != _GCoff {
		gcmarknewobject(span, uintptr(x), size, scanSize)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	mp.mallocing = 0
	releasem(mp)

	// XXX Not doing debug.allocfreetrace
	// XXX Not doing MemProfile
	// XXX Not charging gcAssistBytes
	// XXX Not helping GC

	return x

}

// We keep the chunk on this list after unmapUserArenaChunk has been called if the
// chunk can't immediately be unmapped because GC marking is happening. In this
// case, we want scanning of arena objects to continue normally, even though the
// user program has said that they should not be used anymore. We don't want any
// dangling pointers until we've been able to unmap. We put the chunk on this list
// so that it can't be reclaimed by the normal GC process before it is unmapped
var tempChunkList *chunkHeader

// These are heapArena structs (including large GC bitmap) that are available for
// recycling -- those associated user arena chunk has already been unmapped.
var heapArenaCheckList *heapArena

// These are mSpan structs from user arena chunks that are waiting to get
// recycled. We wait two GC cycles to make sure that the spans have been dropped
// from the sweep lists.
var spanReuseList *mspan

var freeCost int64
var freeCount int64
var freeTime int64

// checkArenaUnmap iterates through tempChunkList and unmaps the heap arena region
// associated with each user arena chunk. It also removes the arena from
// mheap_.arenas.
//
// mheap_.lock must be held.
func checkArenaUnmap() {
	assertLockHeld(&mheap_.lock)

	var next *chunkHeader
	var cycles uint32

	for ch := tempChunkList; ch != nil; ch = next {
		if cycles == 0 {
			cycles = atomic.Load(&work.cycles) + 1
		}
		next = ch.next
		ch.next = nil
		ri := arenaIndex(uintptr(unsafe.Pointer(ch)))
		l1 := ri.l1()
		l2 := ri.l2()
		ha := mheap_.arenas[l1][l2]
		s := ha.spans[0]
		if !ha.userArena || ha.didUnmap || !s.userArena || s.didUnmap || s.elemsize != heapArenaBytes {
			throw("Bad chunk for checkArenaUnmap")
		}
		// GC will now ignore pointers into this arena range.
		// Atomic equivalent of: mheap_.arenas[l1][l2] = nil
		atomic.StorepNoWB(unsafe.Pointer(&(mheap_.arenas[l1][l2])), nil)
		// Actually unmap the arena, so we'll get dangling pointer errors.
		start := nanotime()
		sysFree(unsafe.Pointer(s.base()), s.elemsize, nil)
		freeCost += nanotime() - start
		freeCount++
		freeTime += nanotime() - ha.freetime
		if freeCount%10000 == 0 {
			println("Unmap cost", freeCost/freeCount, freeCount)
			println("Unmap time", freeTime/freeCount, freeCount)
		}
		ha.didUnmap = true
		s.freecycle = cycles
		// Prevent user arena chunk from being swept once we unmap it
		s.didUnmap = true
		ha.next = heapArenaCheckList
		heapArenaCheckList = ha
		if s.next != nil || s.prev != nil {
			throw("Span for user arena chunk is already on a list")
		}
		s.next = spanReuseList
		spanReuseList = s
		// Not needed but just in case, to catch uses of span after the unmap
		ha.spans[0] = nil

	}
	tempChunkList = nil
}

// Must match arenaChunkHeader in reflect/arena.go
type chunkHeader struct {
	off      uintptr // start of allocation area, relative to &arena
	size     uintptr // size of allocation area
	lastP    uintptr
	freeTime uintptr
	numGcs   uintptr
	pad1     uintptr
	pad2     uintptr
	next     *chunkHeader // next chunk in list
}

// Number of bytes of memory covered by one byte of GC bits
const gcByteRatio = wordsPerBitmapByte * goarch.PtrSize

var listLen uint64
var listLenAfterUnmap uint64
var spanListLen uint64
var listLenCount uint64
var delayCount uint64

// unsafe_unmapUserArenaChunk unmaps the specified user arena chunk. bitsSize
// specifies how many bytes at the start of the chunk has pointers, so the GC bits
// must be cleared to this point. Unmapping must be delayed if the GC in the
// marking phase (since GC may have remaining pointers on the mark queue that
// point into the chunk. Once the chunk is unmapped, the arena meta-data and
// associated span struct can immediately be reused.
//go:linkname reflect_unsafe_unmapUserArenaChunk reflect.unsafe_unmapUserArenaChunk
func reflect_unsafe_unmapUserArenaChunk(ptr uintptr, size uintptr, bitsSize uintptr) {
	hb := heapBitsForAddr(uintptr(ptr))
	l1 := arenaIdx(hb.arena).l1()
	l2 := arenaIdx(hb.arena).l2()
	ha := mheap_.arenas[l1][l2]
	s := spanOfHeap(ptr)
	if !ha.userArena || hb.shift != 0 || size != heapArenaBytes || ha.didUnmap ||
		s == nil || !s.userArena || s.elemsize != size ||
		s.npages*pageSize != size || s.didUnmap {
		panic("Bad chunk arg for unsafe_unmapUserArenaChunk")
	}

	ha.freetime = nanotime()
	ha.bitsSize = uint32((bitsSize + (gcByteRatio - 1)) / gcByteRatio)

	chunk := (*chunkHeader)(unsafe.Pointer(ptr))

	// Must run on systemstack, since we acquire mheap_.lock
	systemstack(func() {
		tot := uint64(0)
		totAfterUnmap := uint64(0)
		totSpan := uint64(0)
		lock(&mheap_.lock)
		chunk.next = tempChunkList
		tempChunkList = chunk
		for p := tempChunkList; p != nil; p = p.next {
			tot++
		}
		for p := heapArenaCheckList; p != nil; p = p.next {
			totAfterUnmap++
		}
		for p := spanReuseList; p != nil; p = p.next {
			totSpan++
		}
		// Put the arena on the check list.
		if gcphase == _GCoff {
			// We can only unmap if we're in the _GCoff phase. Otherwise,
			// we'll keep checking each time we unmap or allocate a user arena
			// chunk.
			checkArenaUnmap()
		} else {
			delayCount++
		}
		unlock(&mheap_.lock)
		listLen += tot
		listLenAfterUnmap += totAfterUnmap
		spanListLen += totSpan

		// Important stats update, since this chunk memory is now not managed by GC
		atomic.Xadd64(&memstats.heap_released, int64(s.npages*pageSize))
		atomic.Xadd64(&memstats.heap_inuse, -int64(s.npages*pageSize))
		atomic.Xadd64(&gcController.heapLive, -int64(s.npages*pageSize))

		// Update consistent stats on the system stack so our P doesn't
		// change out from under us.
		stats := memstats.heapStats.acquire()
		atomic.Xaddint64(&stats.committed, -int64(s.npages*pageSize))
		atomic.Xaddint64(&stats.released, int64(s.npages*pageSize))
		atomic.Xaddint64(&stats.inHeap, -int64(s.npages*pageSize))
		memstats.heapStats.release()
	})
	listLenCount++
	if listLenCount%10000 == 0 {
		println("listLen", listLen/listLenCount, listLenCount)
		println("listLenAfterUnmap", listLenAfterUnmap/listLenCount, listLenCount)
		println("spanListLen", spanListLen/listLenCount, listLenCount)
		println("delayFraction", 1000*delayCount/listLenCount)
	}
}

var reuseTime int64
var reuseCount uint64

// reuseUnmappedArena checks if there is an existing heapArena struct from a freed
// user arena chunk that is ready to be reused. A heapArena struct can't be reused
// until the address range associated with its previous use has been unmapped.
//
// h.lock must be held.
func (h *mheap) reuseUnmappedArena() *heapArena {
	assertLockHeld(&h.lock)

	var r *heapArena
	if heapArenaCheckList != nil {
		r = heapArenaCheckList
		heapArenaCheckList = r.next
	}
	if r != nil {
		unlock(&h.lock)
		// Reuse the arena and free the span.
		reuseTime += nanotime() - r.freetime
		reuseCount++
		if reuseCount%10000 == 0 {
			println("Reuse time", uint64(reuseTime)/reuseCount, reuseCount)
		}
		if r.bitsSize > 0 {
			// Clear the GC bits now if they haven't already been cleared
			memclrNoHeapPointers(unsafe.Pointer(&r.bitmap[0]), uintptr(r.bitsSize))
		}
		// Zero out the entire arena struct, except for the bitmap
		// (already cleared) and spans (will be completely overwritten)
		memclrNoHeapPointers(unsafe.Pointer(&r.pageInUse[0]), unsafe.Sizeof(*r)-unsafe.Offsetof(r.pageInUse))
		r.userArena = true
		lock(&h.lock)
	}
	var prev *mspan
	var next *mspan
	var cycles uint32
	for s := spanReuseList; s != nil; s = next {
		next = s.next
		if cycles == 0 {
			cycles = atomic.Load(&work.cycles) + 1
		}
		// Make sure we have finished the sweeping when the arena chunk
		// was unmapped, and the next sweeping cycle. This will make sure
		// that the span has been removed from the sweep list.
		if cycles <= s.freecycle+1 {
			prev = s
		} else {
			if prev == nil {
				spanReuseList = s.next
			} else {
				prev.next = s.next
			}
			s.allocCount = 0
			s.didUnmap = false
			// Free the span that was associated with last use of the heapArena
			h.freeSpanLocked(s, spanAllocUserArena)
		}
	}
	return r
}

// Allocate npages pages from address range for a large, unmappable user arena.
// Derived from (*mheap).grow.
//
// h.lock must be held.
//
// Must run on the system stack because it requires the heaplock to be held,
// like (*mheap).grow, and our P must not change as we access the P's mcache.
//go:systemstack
func (h *mheap) userArenaGrow(npages uintptr) uintptr {
	assertLockHeld(&h.lock)

	ask := alignUp(npages, pallocChunkPages) * pageSize
	nBase := alignUp(h.userArena.base+ask, physPageSize)
	if nBase > h.userArena.end {
		av, asize := h.sysAlloc(ask, true)
		// sysAlloc returns Reserved address space, so transition
		// it to Prepared.
		// Unlike (*mheap).grow, just map in everything that we
		// asked for. We're likely going to use it all.
		sysMap(av, asize, &memstats.heap_sys)
		if uintptr(av) == h.userArena.end {
			h.userArena.end = uintptr(av) + asize
		} else {
			h.userArena.base = uintptr(av)
			h.userArena.end = uintptr(av) + asize
		}
		// The memory just allocated counts as released,
		// even though it's not yet backed by spans.
		//
		// The allocation is always aligned to the heap arena
		// size which is always > physPageSize, so its safe to
		// just add directly to heap_released.
		atomic.Xadd64(&memstats.heap_released, int64(asize))
		stats := memstats.heapStats.acquire()
		atomic.Xaddint64(&stats.released, int64(asize))
		memstats.heapStats.release()

		// Recalculate nBase
		nBase = alignUp(h.userArena.base+ask, physPageSize)
	}
	v := h.userArena.base
	h.userArena.base = nBase
	return v
}

// Arenas are not supported for 32-bit machines, but these constants are zero for
// 32-bit machines, which allows the code to compile.
const arenaStart = (goarch.PtrSize/4 - 1) * userArenaHintStartAddress
const arenaSize = (goarch.PtrSize/4 - 1) * (1 << 40)

//go:linkname reflect_unsafe_inArena reflect.unsafe_inArena
func reflect_unsafe_inArena(ptr uintptr) bool {
	// Compare at the top end with the top of the highest hint used so far,
	// since static data can sometimes occur in the middle of the hint range.
	return ptr > arenaStart && ptr < (mheap_.userArenaHints.addr+arenaSize)
}
