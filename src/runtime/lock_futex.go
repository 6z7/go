// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build dragonfly freebsd linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	futexsleep(addr *uint32, val uint32, ns int64)
//		Atomically,
//			if *addr == val { sleep }
//		Might be woken up spuriously; that's allowed.
//		Don't sleep longer than ns; ns < 0 means forever.
//
//	futexwakeup(addr *uint32, cnt uint32)
//		If any procs are sleeping on addr, wake up at most cnt.

const (
	mutex_unlocked = 0
	mutex_locked   = 1
	mutex_sleeping = 2

	active_spin     = 4
	active_spin_cnt = 30
	passive_spin    = 1
)

// Possible lock states are mutex_unlocked, mutex_locked and mutex_sleeping.
// mutex_sleeping means that there is presumably at least one sleeping thread.
// Note that there can be spinning threads during all states - they do not
// affect mutex's state.

// We use the uintptr mutex.key and note.key as a uint32.
//go:nosplit
func key32(p *uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(p))
}

//获取锁
func lock(l *mutex) {
	gp := getg()

	if gp.m.locks < 0 {
		throw("runtime·lock: lock count")
	}
	//执行当前g的m锁数量+1
	gp.m.locks++

	//原子交换，返回旧值
	// Speculative grab for lock.
	v := atomic.Xchg(key32(&l.key), mutex_locked)
	//被其它g唤醒
	if v == mutex_unlocked {
		return
	}

	// wait is either MUTEX_LOCKED or MUTEX_SLEEPING
	// depending on whether there is a thread sleeping
	// on this mutex. If we ever change l->key from
	// MUTEX_SLEEPING to some other value, we must be
	// careful to change it back to MUTEX_SLEEPING before
	// returning, to ensure that the sleeping thread gets
	// its wakeup call.
	wait := v

	// On uniprocessors, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0
	//如果只有一个cpu自旋没意义
	if ncpu > 1 {
		spin = active_spin
	}
	for {
		//尝试自旋获取锁
		// Try for lock, spinning.
		for i := 0; i < spin; i++ {
			for l.key == mutex_unlocked {
				//尝试获取锁
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			//暂停30个clock
			procyield(active_spin_cnt)
		}

		// Try for lock, rescheduling.
		for i := 0; i < passive_spin; i++ {
			for l.key == mutex_unlocked {
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			//yield使用另一个级别等于或高于当前线程的线程先运行。如果没有符合条件的线程，
			//那么这个函数将会立刻返回然后继续执行当前线程的程序
			osyield()
		}

		//通过自旋获取锁失败，则使用系统调用sleep
		// Sleep.
		v = atomic.Xchg(key32(&l.key), mutex_sleeping)
		//被其它g唤醒
		if v == mutex_unlocked {
			return
		}
		wait = mutex_sleeping
		//系统调用进行sleep,如果在futex获取锁时锁的状态不等于期望的状态会直接返回重新获取锁
		futexsleep(key32(&l.key), mutex_sleeping, -1)
	}
}

//释放锁
func unlock(l *mutex) {
	//原子交换，返回旧值
	v := atomic.Xchg(key32(&l.key), mutex_unlocked)
	//锁已经被释放
	if v == mutex_unlocked {
		throw("unlock of unlocked lock")
	}
	//唤醒一个g
	if v == mutex_sleeping {
		futexwakeup(key32(&l.key), 1)
	}

	gp := getg()
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt { // restore the preemption request in case we've cleared it in newstack
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
func noteclear(n *note) {
	n.key = 0
}

func notewakeup(n *note) {
	//设置n.key = 1, 被唤醒的线程通过查看该值是否等于1来确定是被其它线程唤醒还是意外从睡眠中苏醒
	old := atomic.Xchg(key32(&n.key), 1)
	if old != 0 {
		print("notewakeup - double wakeup (", old, ")\n")
		throw("notewakeup - double wakeup")
	}
	futexwakeup(key32(&n.key), 1)
}

func notesleep(n *note) {
	gp := getg()
	if gp != gp.m.g0 {
		throw("notesleep not on g0")
	}
	ns := int64(-1)  //超时时间设置为-1，表示无限期等待
	if *cgo_yield != nil {
		// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
		ns = 10e6
	}
	//使用循环，保证不是意外被唤醒
	for atomic.Load(key32(&n.key)) == 0 {
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
	}
}

// May run with m.p==nil if called from notetsleep, so write barriers
// are not allowed.
//
//go:nosplit
//go:nowritebarrier
func notetsleep_internal(n *note, ns int64) bool {
	gp := getg()

	if ns < 0 {
		if *cgo_yield != nil {
			// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
			ns = 10e6
		}
		for atomic.Load(key32(&n.key)) == 0 {
			gp.m.blocked = true
			futexsleep(key32(&n.key), 0, ns)
			if *cgo_yield != nil {
				asmcgocall(*cgo_yield, nil)
			}
			gp.m.blocked = false
		}
		return true
	}

	if atomic.Load(key32(&n.key)) != 0 {
		return true
	}

	deadline := nanotime() + ns
	for {
		if *cgo_yield != nil && ns > 10e6 {
			ns = 10e6
		}
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
		if atomic.Load(key32(&n.key)) != 0 {
			break
		}
		now := nanotime()
		if now >= deadline {
			break
		}
		ns = deadline - now
	}
	return atomic.Load(key32(&n.key)) != 0
}

func notetsleep(n *note, ns int64) bool {
	gp := getg()
	if gp != gp.m.g0 && gp.m.preemptoff != "" {
		throw("notetsleep not on g0")
	}

	return notetsleep_internal(n, ns)
}

// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}

	entersyscallblock()
	ok := notetsleep_internal(n, ns)
	exitsyscall()
	return ok
}

func beforeIdle() bool {
	return false
}

func checkTimeouts() {}
