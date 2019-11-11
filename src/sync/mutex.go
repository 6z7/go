// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//互斥锁
//g通过竞争获取mutex状态的修改所有权，所有g阻塞在信号量的队列上，
//当unlock后排在队列头部的g会与新的g竞争所有权(即唤醒的g不一定能够获取到mutex的所有权)，
//当g被唤醒后如果等待时间大于1ms,则mutex状态会被标记为饥饿状态,
//如果当前获取锁的g处于饥饿状态,则新的g不会自旋,并将当前g放到队列的首位，下次唤醒直接执行该g
//
type Mutex struct {
	//互斥锁状态
	//向移动3位置表示等待获取锁的goroutine数量
	state int32
	//信号量
	sema uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	//已获取锁1
	mutexLocked = 1 << iota // mutex is locked
	//已释放锁2
	mutexWoken
	//饥饿模式(排在前边的go协程一直未获取到锁)2^2
	mutexStarving
	//3 表示mutex.state右移3位后即为等待的goroutine的数量
	mutexWaiterShift = iota

	//互斥锁2种模式：正常模式，饥饿模式
	//正常模式下waiter按照FIFO顺序排队，但是唤醒时会与新的goroutine竞争mutex,
	//新的goroutine应为已经在CPU上运行会比新唤醒的goroutine更有优势获取到mutex,
	//在这种情况下，如果waiter等待获取mutex超过1ms，则将该waiter放到队列的前面，同时锁状态切换到饥饿模式。
	//
	// 饥饿模式下，mutex的所有权直接从unlock goruntine交到队列头部的waiter。新的goroutine直接排到队列的尾部，
	//不会尝试获mutex。
	//
	// 如果waiter获取到mutex的后满足以下情况，则恢复到正常模式：
	// 1.队列中最后一个waiter
	// 2.获取Mutex的时间小于1ms
	//
	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	//切换到饥饿模式的阀值1ms
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	//开始等待的时间
	var waitStartTime int64
	//是否进入了饥饿模式
	starving := false
	//是否唤醒了当前的goroutine
	awoke := false
	//自旋次数
	iter := 0
	//当前状态
	old := m.state
	for {
		//
		// 如果是饥饿情况，无需自旋
		// 如果其它g获取到了锁，则当前g尝试自旋获取锁
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			//old>>mutexWaiterShift 锁上等待的goroutine数量
			//锁的状态设置为唤醒，这样当Unlock的时候就不会去唤醒其它被阻塞的goroutine了
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			//自旋转30次数
			runtime_doSpin()
			//统计当前goroutine自旋次数
			iter++
			//更新锁的状态(有可能在自旋的这段时间之内锁的状态已经被其它goroutine改变)
			old = m.state
			continue
		}
		//自选完了还未获取到锁，则开始竞争锁

		//复制一份最新锁状态，用来存放期望的锁状态
		new := old

		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {
			//非饥饿模式下，可以抢锁
			new |= mutexLocked
		}

		if old&(mutexLocked|mutexStarving) != 0 {
			//其它g已获取到锁或处于饥饿模式下，则会阻塞当前g，等待g的数量+1
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		//当前goroutine的starving=true是饥饿状态，并且锁被其它goroutine获取了，
		// 那么将期望的锁的状态设置为饥饿状态
		//
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		//当前g被唤醒
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			//new设置为非唤醒状态
			new &^= mutexWoken
		}
		// 通过CAS来尝试设置锁的状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 如果说old状态不是饥饿状态也不是被获取状态
			// 那么代表当前goroutine已经通过自旋成功获取了锁
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			//是否放到队列的最前面，如果之前已经等待过直接放到队列最前面
			queueLifo := waitStartTime != 0
			//如果说之前没有等待过，就初始化设置现在的等待时间
			if waitStartTime == 0 {
				//获取当前时间(单位ns)
				waitStartTime = runtime_nanotime()
			}
			// 通过信号量来排队获取锁
			// 如果是新来的goroutine，就放到队列尾部
			// 如果是被唤醒的等待锁的goroutine，就放到队列头部
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)  // runtime/sema.go

			//获取到信号量进入下面的步骤(唤醒了当前的waiter)

			//等待时间超过阀值进入饥饿模式
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			//获取锁的最新状态
			old = m.state
			// 如果说锁现在是饥饿状态，就代表现在锁是被释放的状态(unlock释放队列最前面的goroutine)，
			// 当前goroutine是被信号量所唤醒的
			// 也就是说，锁被直接交给了当前goroutine
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				//饥饿状态下不会有其它G获取到了锁或被唤醒
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				//当前goroutine已获取锁，则等待获取锁的goroutine数量-1
				//最终状态atomic.AddInt32(&m.state, delta)
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 如果当前goroutine非饥饿状态，或者说当前goroutine是队列中最后一个goroutine
				// 那么就退出饥饿模式，把状态设置为正常
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				//原子性地加上改动的状态
				atomic.AddInt32(&m.state, delta)
				break
			}
			// 如果锁不是饥饿模式，就把当前的goroutine设为被唤醒，由unlock操作导致的唤醒
			// 并且重置自旋计数器
			awoke = true
			iter = 0
		} else {
			//mutex状态已经被修改，刷新一遍重新计算
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// 这里获取到锁的状态，然后将状态减去被获取的状态(也就是解锁)，称为new(期望)状态
	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	//有其它g需要唤醒
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	//unlock调用多次触发panic
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	//非饥饿状态
	if new&mutexStarving == 0 {
		old := new
		for {
			// 如果说锁没有等待拿锁的goroutine
			// 或者锁被获取了(在循环的过程中被其它goroutine获取了)
			// 或者锁是被唤醒状态(表示有goroutine被唤醒，不需要再去尝试唤醒其它goroutine)
			// 或者锁是饥饿模式(会直接转交给队列头的goroutine)
			// 那么就直接返回
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// 走到这一步的时候，说明锁目前还是空闲状态，并且没有goroutine被唤醒且队列中有goroutine等待拿锁
			// 那么我们就要把锁的状态设置为被唤醒，等待队列-1
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				//通过信号量去唤醒goroutine
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// 如果是饥饿状态下，那么我们就直接把锁的所有权通过信号量移交给队列头的goroutine就好了
		// handoff = true表示直接把锁交给队列头部的goroutine
		// 注意：在这个时候，锁被获取的状态没有被设置，会由被唤醒的goroutine在唤醒后设置
		// 但是当锁处于饥饿状态的时候，我们也认为锁是被获取的(因为我们手动指定了获取的goroutine)
		// 所以说新来的goroutine不会尝试去获取锁(在Lock中有体现)
		// Starving mode: handoff mutex ownership to the next waiter.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1)
	}
}

//https://purewhite.io/2019/03/28/golang-mutex-source/

