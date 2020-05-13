// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build amd64 amd64p32 386

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then did an immediate gosave.
// 调整g开始执行的位置
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	sp := buf.sp //newg的栈顶，目前newg栈上只有fn函数的参数，sp指向的是fn的第一参数
	if sys.RegSize > sys.PtrSize {
		sp -= sys.PtrSize
		*(*uintptr)(unsafe.Pointer(sp)) = 0
	}
	// 为返回地址预留空间，
	sp -= sys.PtrSize
	// 预留的地址空间 放入goexit下一个指令的地址
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	// 重新设置sp
	buf.sp = sp
	// 当g被调度时，从pc寄存器指定的位置开始执行，初始时是runtime.main
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
