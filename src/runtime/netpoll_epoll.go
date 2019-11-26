// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import "unsafe"

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32  //sys_linux_amd64.s

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32  //sys_linux_amd64.s

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
//epoll设置标志closeexec
func closeonexec(fd int32)   //sys_linux_amd64.s

var (
	//epoll fd
	epfd int32 = -1 // epoll descriptor
)
//创建epoll fd
func netpollinit() {
	//创建epoll fd
	epfd = epollcreate1(_EPOLL_CLOEXEC)
	if epfd >= 0 {
		return
	}
	epfd = epollcreate(1024)
	if epfd >= 0 {
		closeonexec(epfd)
		return
	}
	println("runtime: epollcreate failed with", -epfd)
	throw("runtime: netpollinit failed")
}

func netpolldescriptor() uintptr {
	return uintptr(epfd)
}

//socket fd关注epoll fd上指定的事件
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	// LT模式时，事件就绪时，假设对事件没做处理，内核会反复通知事件就绪(Level-triggered)
	// ET模式时，事件就绪时，假设对事件没做处理，内核不会反复通知事件就绪(edge-triggered)
	// EPOLLIN: 连接到达；有数据来临
	// EPOLLOUT:有数据要写
	// EPOLLRDHUP:断开连接
	// EPOLLET: ET模式
		ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	//socket fd关注epoll fd上指定的事件
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

//删除fd已注册的event事件
func netpollclose(fd uintptr) int32 {
	var ev epollevent
	//删除fd已注册的event事件
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// polls for ready network connections
// returns list of goroutines that become runnable
func netpoll(block bool) gList {
	if epfd == -1 {
		return gList{}
	}
	waitms := int32(-1)
	if !block {
		waitms = 0
	}
	var events [128]epollevent
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms)
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		goto retry
	}
	var toRun gList
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}
		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
			pd.everr = false
			if ev.events == _EPOLLERR {
				pd.everr = true
			}
			netpollready(&toRun, pd, mode)
		}
	}
	if block && toRun.empty() {
		goto retry
	}
	return toRun
}
