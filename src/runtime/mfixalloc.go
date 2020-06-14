// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import "unsafe"

// FixAlloc是一个基于空闲列表的用于固定大小对象的分配器
// FixAlloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
//
// Consider marking fixalloc'd types go:notinheap.
type fixalloc struct {
	// 分配的对象大小
	size   uintptr
	// 第一次分配内存时的回调函数
	first  func(arg, p unsafe.Pointer) // called first time p is returned
	// 回调函数的参数
	arg    unsafe.Pointer
	// 释放的对象构成的空闲链表
	list   *mlink
	// 分配内存的起始地址
	// 每次分配过都会调整指定大小
	chunk  uintptr // use uintptr instead of unsafe.Pointer to avoid write barriers
	// 从os上分配的内存大小
	nchunk uint32
	// 使用的字节数
	inuse  uintptr // in-use bytes now
	// 统计分配的内存
	// 不通的分配器对应不同的统计字段
	stat   *uint64
	// 是否需要清零
	zero   bool // zero allocations
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
//
//go:notinheap
type mlink struct {
	next *mlink
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
// 准备分配内存所需的参数
// size:所需内存大小
// first:内存分配成功后的回调方法
// arg:传给回调方法的参数
// stat:统计分配的内存
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size
	f.first = first
	f.arg = arg
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat
	f.zero = true
}

// 从分配器上分配内存
// 分配器知道分配内存所需的参数
func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	// 存在被释放的空闲内存
	if f.list != nil {
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		if f.zero {
			// 清理数据
			// 调用者知道 指针指向的结构不包含堆指针时
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}
	// 分配器上当前可用的内存不满足实际需要的
	if uintptr(f.nchunk) < f.size {
		// 从os上申请16kb
		f.chunk = uintptr(persistentalloc(_FixAllocChunk, 0, f.stat))
		f.nchunk = _FixAllocChunk
	}

	v := unsafe.Pointer(f.chunk)
	// 先回调
	if f.first != nil {
		f.first(f.arg, v)
	}
	f.chunk = f.chunk + f.size
	f.nchunk -= uint32(f.size)
	f.inuse += f.size
	return v
}

// 释放的内存放入空闲链表的头部
func (f *fixalloc) free(p unsafe.Pointer) {
	f.inuse -= f.size
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
