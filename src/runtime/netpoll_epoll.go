// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/syscall"
	"unsafe"
)

var (
	epfd           int32         = -1 // epoll descriptor
	netpollEventFd uintptr            // eventfd for netpollBreak
	netpollWakeSig atomic.Uint32      // used to avoid duplicate calls of netpollBreak
)

// netpollinit 初始化 Linux 的 epoll 网络轮询器
//
// 该函数在 Linux 系统上创建并设置 epoll 实例，用于高效地监控多个文件描述符的 I/O 事件
func netpollinit() {
	var errno uintptr

	// 创建 epoll 实例
	// EPOLL_CLOEXEC: 在执行 exec 时自动关闭 epoll 文件描述符
	epfd, errno = syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if errno != 0 {
		println("runtime: epollcreate failed with", errno)
		throw("runtime: netpollinit failed")
	}

	// 创建 eventfd，用于唤醒 epoll_wait
	// EFD_CLOEXEC: 在执行 exec 时自动关闭
	// EFD_NONBLOCK: 设置为非阻塞模式
	efd, errno := syscall.Eventfd(0, syscall.EFD_CLOEXEC|syscall.EFD_NONBLOCK)
	if errno != 0 {
		println("runtime: eventfd failed with", -errno)
		throw("runtime: eventfd failed")
	}

	// 创建 epoll 事件，用于监控 eventfd
	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN, // 监听读事件
	}
	// 设置事件的用户数据指针，指向 netpollEventFd
	*(**uintptr)(unsafe.Pointer(&ev.Data)) = &netpollEventFd

	// 将 eventfd 添加到 epoll 实例中
	errno = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, efd, &ev)
	if errno != 0 {
		println("runtime: epollctl failed with", errno)
		throw("runtime: epollctl failed")
	}

	// 保存 eventfd 供后续使用
	netpollEventFd = uintptr(efd)
}

func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollEventFd
}

// netpollopen 将文件描述符注册到 epoll 实例中
//
// 参数：
// - fd: 要监控的文件描述符
// - pd: 与文件描述符关联的轮询描述符
//
// 返回值：
// - uintptr: 系统调用的错误码，0 表示成功
func netpollopen(fd uintptr, pd *pollDesc) uintptr {
	// 创建并初始化 epoll 事件结构
	var ev syscall.EpollEvent

	// 设置要监听的事件类型：
	// - EPOLLIN: 可读事件
	// - EPOLLOUT: 可写事件
	// - EPOLLRDHUP: TCP 连接远端关闭或者半关闭事件
	// - EPOLLET: 边缘触发模式
	/*
		每个事件标志都有特定用途：
		EPOLLIN：表示关注读就绪事件
		---socket 有数据可读
		---对端关闭连接（会触发可读事件，读取返回 EOF）
		EPOLLOUT：表示关注写就绪事件
		---socket 可以写入数据
		---连接建立完成时也会触发
		EPOLLRDHUP：表示关注对端关闭事件
		---对端关闭连接或半关闭
		---帮助更精确地检测连接状态
		EPOLLET：表示使用边缘触发模式
		---只在状态变化时触发一次
		---相对于水平触发模式更高效
	*/
	ev.Events = syscall.EPOLLIN | syscall.EPOLLOUT | syscall.EPOLLRDHUP | syscall.EPOLLET

	// 将轮询描述符和序列号打包成带标签的指针
	// 用于在事件触发时识别正确的轮询描述符
	tp := taggedPointerPack(unsafe.Pointer(pd), pd.fdseq.Load())

	// 将带标签的指针存储在事件的数据字段中
	*(*taggedPointer)(unsafe.Pointer(&ev.Data)) = tp

	// 将文件描述符添加到 epoll 实例中
	// - epfd: epoll 实例的文件描述符
	// - EPOLL_CTL_ADD: 添加新的文件描述符到实例
	// - fd: 要监控的文件描述符
	// - &ev: 事件结构的指针
	return syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, int32(fd), &ev)
}

func netpollclose(fd uintptr) uintptr {
	var ev syscall.EpollEvent
	return syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak interrupts an epollwait.
func netpollBreak() {
	// Failing to cas indicates there is an in-flight wakeup, so we're done here.
	if !netpollWakeSig.CompareAndSwap(0, 1) {
		return
	}

	var one uint64 = 1
	oneSize := int32(unsafe.Sizeof(one))
	for {
		n := write(netpollEventFd, noescape(unsafe.Pointer(&one)), oneSize)
		if n == oneSize {
			break
		}
		if n == -_EINTR {
			continue
		}
		if n == -_EAGAIN {
			return
		}
		println("runtime: netpollBreak write failed with", -n)
		throw("runtime: netpollBreak write failed")
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
//
// netpoll 检查就绪的网络连接
// 返回变为可运行状态的 goroutine 列表
func netpoll(delay int64) (gList, int32) {
	// 检查 epoll 实例是否已初始化
	if epfd == -1 {
		return gList{}, 0
	}

	// 将纳秒级延迟转换为毫秒
	var waitms int32
	if delay < 0 {
		waitms = -1 // 永久阻塞
	} else if delay == 0 {
		waitms = 0 // 非阻塞轮询
	} else if delay < 1e6 {
		waitms = 1 // 小于1ms，至少等待1ms
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6) // 转换为毫秒
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9 // 最大等待时间约11.5天
	}

	// 事件缓冲区
	var events [128]syscall.EpollEvent

retry:
	// 调用 epoll_wait 等待事件
	n, errno := syscall.EpollWait(epfd, events[:], int32(len(events)), waitms)
	if errno != 0 {
		if errno != _EINTR {
			// 发生严重错误
			println("runtime: epollwait on fd", epfd, "failed with", errno)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{}, 0
		}
		goto retry
	}

	// 准备返回的可运行 goroutine 列表
	var toRun gList
	delta := int32(0)

	// 处理所有就绪的事件
	for i := int32(0); i < n; i++ {
		ev := events[i]
		if ev.Events == 0 {
			continue
		}

		// 处理 eventfd 的特殊情况
		if *(**uintptr)(unsafe.Pointer(&ev.Data)) == &netpollEventFd {
			if ev.Events != syscall.EPOLLIN {
				println("runtime: netpoll: eventfd ready for", ev.Events)
				throw("runtime: netpoll: eventfd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the 8-byte
				// integer if blocking.
				// Since EFD_SEMAPHORE was not specified,
				// the eventfd counter will be reset to 0.
				// 读取并清除 eventfd 的计数
				var one uint64
				read(int32(netpollEventFd), noescape(unsafe.Pointer(&one)), int32(unsafe.Sizeof(one)))
				netpollWakeSig.Store(0)
			}
			continue
		}

		// 确定就绪的事件类型（读/写）
		var mode int32
		if ev.Events&(syscall.EPOLLIN|syscall.EPOLLRDHUP|syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
			mode += 'r' // 可读事件
		}
		if ev.Events&(syscall.EPOLLOUT|syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
			mode += 'w' // 可写事件
		}

		// 如果有事件就绪，唤醒对应的 goroutine
		if mode != 0 {

			/*
							// 1. 注册阶段
				ev := syscall.EpollEvent{
				    Events: syscall.EPOLLIN,
				    Data: {
				        // 存储 pollDesc 指针和序列号
				        ptr: &pd,        // pollDesc 指针
				        tag: fdseq      // 文件描述符序列号
				    }
				}
			*/
			// 从 epoll 事件中解析出 pollDesc 和标记信息
			tp := *(*taggedPointer)(unsafe.Pointer(&ev.Data)) // ev.Data 包含了文件描述符相关的用户数据
			// taggedPointer 包含指针和版本标记

			// 获取 pollDesc 指针
			// pollDesc 包含了文件描述符的完整轮询信息（如等待的goroutine、错误状态等）
			pd := (*pollDesc)(tp.pointer())

			// 获取文件描述符的版本标记
			// 用于检测文件描述符是否已被重用
			tag := tp.tag()

			// 检查文件描述符的序列号是否匹配
			// 防止文件描述符被关闭后重用导致的错误操作
			if pd.fdseq.Load() == tag {
				// 设置事件的错误状态
				// ev.Events == syscall.EPOLLERR 表示发生了错误事件
				pd.setEventErr(ev.Events == syscall.EPOLLERR, tag)

				// 唤醒等待在这个文件描述符上的 goroutine
				// toRun: 要恢复运行的 goroutine 列表
				// pd: 轮询描述符
				// mode: 就绪的 I/O 模式（读/写）
				// delta: 累计等待者数量的变化
				delta += netpollready(&toRun, pd, mode)
			}
		}
	}

	return toRun, delta
}
