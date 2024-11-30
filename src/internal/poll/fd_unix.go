// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || wasip1

package poll

import (
	"internal/itoa"
	"internal/syscall/unix"
	"io"
	"sync/atomic"
	"syscall"
)

// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
type FD struct {
	// Lock sysfd and serialize access to Read and Write methods.
	fdmu fdMutex // 文件描述符的互斥锁，用于保护对 Read 和 Write 方法的访问

	// System file descriptor. Immutable until Close.
	Sysfd int // 系统文件描述符，关闭之前不可变

	// Platform dependent state of the file descriptor.
	SysFile // 平台相关的文件描述符状态

	// I/O poller.
	pd pollDesc // I/O 轮询器

	// Semaphore signaled when file is closed.
	csema uint32 // 文件关闭时信号量

	// Non-zero if this file has been set to blocking mode.
	isBlocking uint32 // 如果文件被设置为阻塞模式，则为非零

	// Whether this is a streaming descriptor, as opposed to a
	// packet-based descriptor like a UDP socket. Immutable.
	IsStream bool // 是否为流式描述符（如 TCP），与基于数据包的描述符（如 UDP）相对

	// Whether a zero byte read indicates EOF. This is false for a
	// message based socket connection.
	ZeroReadIsEOF bool // 读取零字节是否表示 EOF，对于基于消息的套接字连接为假

	// Whether this is a file rather than a network socket.
	isFile bool // 是否为文件而非网络套接字
}

// Init initializes the FD. The Sysfd field should already be set.
// This can be called multiple times on a single FD.
// The net argument is a network name from the net package (e.g., "tcp"),
// or "file".
// Set pollable to true if fd should be managed by runtime netpoll.
func (fd *FD) Init(net string, pollable bool) error {
	fd.SysFile.init() // 初始化平台相关的文件描述符状态

	// We don't actually care about the various network types.
	if net == "file" {
		fd.isFile = true // 如果是文件类型，设置 isFile 为 true
	}
	if !pollable {
		fd.isBlocking = 1 // 如果不可轮询，设置为阻塞模式
		return nil
	}
	err := fd.pd.init(fd) // 初始化 I/O 轮询器
	if err != nil {
		// If we could not initialize the runtime poller,
		// assume we are using blocking mode.
		fd.isBlocking = 1 // 如果初始化失败，设置为阻塞模式
	}
	return err
}

// Destroy closes the file descriptor. This is called when there are
// no remaining references.
func (fd *FD) destroy() error {
	// Poller may want to unregister fd in readiness notification mechanism,
	// so this must be executed before CloseFunc.
	fd.pd.close() // 关闭 I/O 轮询器

	err := fd.SysFile.destroy(fd.Sysfd) // 关闭系统文件描述符

	fd.Sysfd = -1                 // 将 Sysfd 设置为无效值
	runtime_Semrelease(&fd.csema) // 释放信号量
	return err
}

// Close closes the FD. The underlying file descriptor is closed by the
// destroy method when there are no remaining references.
func (fd *FD) Close() error {
	if !fd.fdmu.increfAndClose() {
		return errClosing(fd.isFile) // 如果无法增加引用，返回关闭错误
	}

	// Unblock any I/O.  Once it all unblocks and returns,
	// so that it cannot be referring to fd.sysfd anymore,
	// the final decref will close fd.sysfd. This should happen
	// fairly quickly, since all the I/O is non-blocking, and any
	// attempts to block in the pollDesc will return errClosing(fd.isFile).
	fd.pd.evict() // 使所有 I/O 操作解除阻塞

	// The call to decref will call destroy if there are no other
	// references.
	err := fd.decref() // 减少引用计数

	// Wait until the descriptor is closed. If this was the only
	// reference, it is already closed. Only wait if the file has
	// not been set to blocking mode, as otherwise any current I/O
	// may be blocking, and that would block the Close.
	// No need for an atomic read of isBlocking, increfAndClose means
	// we have exclusive access to fd.
	if fd.isBlocking == 0 {
		runtime_Semacquire(&fd.csema) // 等待直到文件描述符关闭
	}

	return err
}

// SetBlocking puts the file into blocking mode.
func (fd *FD) SetBlocking() error {
	if err := fd.incref(); err != nil {
		return err // 增加引用计数失败
	}
	defer fd.decref() // 确保在函数结束时减少引用计数
	// Atomic store so that concurrent calls to SetBlocking
	// do not cause a race condition. isBlocking only ever goes
	// from 0 to 1 so there is no real race here.
	atomic.StoreUint32(&fd.isBlocking, 1)       // 设置为阻塞模式
	return syscall.SetNonblock(fd.Sysfd, false) // 设置系统文件描述符为阻塞
}

// Darwin and FreeBSD can't read or write 2GB+ files at a time,
// even on 64-bit systems.
// The same is true of socket implementations on many systems.
// See golang.org/issue/7812 and golang.org/issue/16266.
// Use 1GB instead of, say, 2GB-1, to keep subsequent reads aligned.
const maxRW = 1 << 30 // 最大读写大小为 1GB

// Read implements io.Reader.
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if len(p) == 0 {
		// If the caller wanted a zero byte read, return immediately
		// without trying (but after acquiring the readLock).
		// Otherwise syscall.Read returns 0, nil which looks like
		// io.EOF.
		// TODO(bradfitz): make it wait for readability? (Issue 15735)
		return 0, nil // 如果请求零字节读取，立即返回
	}
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err // 准备读取失败
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW] // 如果是流式套接字，限制读取大小
	}
	for {
		n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p) // 执行读取操作
		if err != nil {
			n = 0
			//如果系统调用中断，则把fd重新加入到epoll实例中，然后堵塞自身协程
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err) // 检查 EOF 错误
		return n, err             // 返回读取的字节数和错误
	}
}

// Pread wraps the pread system call.
func (fd *FD) Pread(p []byte, off int64) (int, error) {
	// Call incref, not readLock, because since pread specifies the
	// offset it is independent from other reads.
	// Similarly, using the poller doesn't make sense for pread.
	if err := fd.incref(); err != nil {
		return 0, err // 增加引用计数失败
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW] // 如果是流式套接字，限制读取大小
	}
	var (
		n   int
		err error
	)
	for {
		n, err = syscall.Pread(fd.Sysfd, p, off) // 执行 Pread 操作
		if err != syscall.EINTR {
			break // 如果没有被中断，退出循环
		}
	}
	if err != nil {
		n = 0 // 如果发生错误，设置读取字节数为 0
	}
	fd.decref()               // 减少引用计数
	err = fd.eofError(n, err) // 检查 EOF 错误
	return n, err             // 返回读取的字节数和错误
}

// ReadFrom wraps the recvfrom network call.
func (fd *FD) ReadFrom(p []byte) (int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, nil, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, nil, err // 准备读取失败
	}
	for {
		n, sa, err := syscall.Recvfrom(fd.Sysfd, p, 0) // 执行接收操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err) // 检查 EOF 错误
		return n, sa, err         // 返回读取的字节数、地址信息和错误
	}
}

// ReadFromInet4 wraps the recvfrom network call for IPv4.
func (fd *FD) ReadFromInet4(p []byte, from *syscall.SockaddrInet4) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err // 准备读取失败
	}
	for {
		n, err := unix.RecvfromInet4(fd.Sysfd, p, 0, from) // 执行接收操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err) // 检查 EOF 错误
		return n, err             // 返回读取的字节数和错误
	}
}

// ReadFromInet6 wraps the recvfrom network call for IPv6.
func (fd *FD) ReadFromInet6(p []byte, from *syscall.SockaddrInet6) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err // 准备读取失败
	}
	for {
		n, err := unix.RecvfromInet6(fd.Sysfd, p, 0, from) // 执行接收操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err) // 检查 EOF 错误
		return n, err             // 返回读取的字节数和错误
	}
}

// ReadMsg wraps the recvmsg network call.
func (fd *FD) ReadMsg(p []byte, oob []byte, flags int) (int, int, int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, nil, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, nil, err // 准备读取失败
	}
	for {
		n, oobn, sysflags, sa, err := syscall.Recvmsg(fd.Sysfd, p, oob, flags) // 执行接收消息操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err)         // 检查 EOF 错误
		return n, oobn, sysflags, sa, err // 返回读取的字节数、附加字节数、系统标志和地址信息
	}
}

// ReadMsgInet4 is ReadMsg, but specialized for syscall.SockaddrInet4.
func (fd *FD) ReadMsgInet4(p []byte, oob []byte, flags int, sa4 *syscall.SockaddrInet4) (int, int, int, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, err // 准备读取失败
	}
	for {
		n, oobn, sysflags, err := unix.RecvmsgInet4(fd.Sysfd, p, oob, flags, sa4) // 执行接收消息操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err)     // 检查 EOF 错误
		return n, oobn, sysflags, err // 返回读取的字节数、附加字节数、系统标志和错误
	}
}

// ReadMsgInet6 is ReadMsg, but specialized for syscall.SockaddrInet6.
func (fd *FD) ReadMsgInet6(p []byte, oob []byte, flags int, sa6 *syscall.SockaddrInet6) (int, int, int, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, err // 准备读取失败
	}
	for {
		n, oobn, sysflags, err := unix.RecvmsgInet6(fd.Sysfd, p, oob, flags, sa6) // 执行接收消息操作
		if err != nil {
			if err == syscall.EINTR {
				continue // 如果被中断，重试
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 如果没有数据可读，等待可读事件
				}
			}
		}
		err = fd.eofError(n, err)     // 检查 EOF 错误
		return n, oobn, sysflags, err // 返回读取的字节数、附加字节数、系统标志和错误
	}
}

// Write implements io.Writer.
func (fd *FD) Write(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err // 准备写入失败
	}
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW // 如果是流式套接字，限制写入大小
		}
		n, err := ignoringEINTRIO(syscall.Write, fd.Sysfd, p[nn:max]) // 执行写入操作
		if n > 0 {
			if n > max-nn {
				// This can reportedly happen when using
				// some VPN software. Issue #61060.
				// If we don't check this we will panic
				// with slice bounds out of range.
				// Use a more informative panic.
				panic("invalid return from write: got " + itoa.Itoa(n) + " from a write of " + itoa.Itoa(max-nn))
			}
			nn += n // 累加已写入的字节数
		}
		if nn == len(p) {
			return nn, err // 返回已写入的字节数和错误
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return nn, err // 返回已写入的字节数和错误
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF // 返回已写入的字节数和意外的 EOF 错误
		}
	}
}

// Pwrite wraps the pwrite system call.
func (fd *FD) Pwrite(p []byte, off int64) (int, error) {
	// Call incref, not writeLock, because since pwrite specifies the
	// offset it is independent from other writes.
	// Similarly, using the poller doesn't make sense for pwrite.
	if err := fd.incref(); err != nil {
		return 0, err // 增加引用计数失败
	}
	defer fd.decref() // 确保在函数结束时减少引用计数
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW // 如果是流式套接字，限制写入大小
		}
		n, err := syscall.Pwrite(fd.Sysfd, p[nn:max], off+int64(nn)) // 执行 Pwrite 操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if n > 0 {
			nn += n // 累加已写入的字节数
		}
		if nn == len(p) {
			return nn, err // 返回已写入的字节数和错误
		}
		if err != nil {
			return nn, err // 返回已写入的字节数和错误
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF // 返回已写入的字节数和意外的 EOF 错误
		}
	}
}

// WriteToInet4 wraps the sendto network call for IPv4 addresses.
func (fd *FD) WriteToInet4(p []byte, sa *syscall.SockaddrInet4) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err // 准备写入失败
	}
	for {
		err := unix.SendtoInet4(fd.Sysfd, p, 0, sa) // 执行发送操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return 0, err // 返回错误
		}
		return len(p), nil // 返回已写入的字节数
	}
}

// WriteToInet6 wraps the sendto network call for IPv6 addresses.
func (fd *FD) WriteToInet6(p []byte, sa *syscall.SockaddrInet6) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err // 准备写入失败
	}
	for {
		err := unix.SendtoInet6(fd.Sysfd, p, 0, sa) // 执行发送操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return 0, err // 返回错误
		}
		return len(p), nil // 返回已写入的字节数
	}
}

// WriteTo wraps the sendto network call.
func (fd *FD) WriteTo(p []byte, sa syscall.Sockaddr) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err // 准备写入失败
	}
	for {
		err := syscall.Sendto(fd.Sysfd, p, 0, sa) // 执行发送操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return 0, err // 返回错误
		}
		return len(p), nil // 返回已写入的字节数
	}
}

// WriteMsg wraps the sendmsg network call.
func (fd *FD) WriteMsg(p []byte, oob []byte, sa syscall.Sockaddr) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err // 准备写入失败
	}
	for {
		n, err := syscall.SendmsgN(fd.Sysfd, p, oob, sa, 0) // 执行发送消息操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return n, 0, err // 返回已写入的字节数和错误
		}
		return n, len(oob), err // 返回已写入的字节数和附加字节数
	}
}

// WriteMsgInet4 is WriteMsg specialized for syscall.SockaddrInet4.
func (fd *FD) WriteMsgInet4(p []byte, oob []byte, sa *syscall.SockaddrInet4) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err // 准备写入失败
	}
	for {
		n, err := unix.SendmsgNInet4(fd.Sysfd, p, oob, sa, 0) // 执行发送消息操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return n, 0, err // 返回已写入的字节数和错误
		}
		return n, len(oob), err // 返回已写入的字节数和附加字节数
	}
}

// WriteMsgInet6 is WriteMsg specialized for syscall.SockaddrInet6.
func (fd *FD) WriteMsgInet6(p []byte, oob []byte, sa *syscall.SockaddrInet6) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err // 准备写入失败
	}
	for {
		n, err := unix.SendmsgNInet6(fd.Sysfd, p, oob, sa, 0) // 执行发送消息操作
		if err == syscall.EINTR {
			continue // 如果被中断，重试
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue // 如果没有数据可写，等待可写事件
			}
		}
		if err != nil {
			return n, 0, err // 返回已写入的字节数和错误
		}
		return n, len(oob), err // 返回已写入的字节数和附加字节数
	}
}

// Accept wraps the accept network call.
//
// Accept 封装了网络 accept 系统调用，实现了非阻塞的连接接受
func (fd *FD) Accept() (int, syscall.Sockaddr, string, error) {
	// 获取读锁，防止并发读取
	// 在非阻塞模式下，这个锁保护了 accept 和 read 操作
	if err := fd.readLock(); err != nil {
		return -1, nil, "", err // 获取读锁失败
	}
	// 确保在函数返回时释放锁
	defer fd.readUnlock()

	// 准备读取操作
	// 设置必要的状态，确保文件描述符已准备好接受连接
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return -1, nil, "", err // 准备读取失败
	}

	// 循环尝试接受连接
	for {
		// 尝试接受新连接
		// - s: 新连接的文件描述符
		// - rsa: 远程地址信息
		// - errcall: 错误发生时的系统调用名称
		s, rsa, errcall, err := accept(fd.Sysfd) // 执行 accept 操作
		if err == nil {
			return s, rsa, "", err // 返回新连接的文件描述符和远程地址
		}

		// 处理特定的错误情况
		switch err {
		case syscall.EINTR:
			// 系统调用被信号中断，这是正常的，重试即可
			continue

		case syscall.EAGAIN:
			// 当前没有连接可接受
			if fd.pd.pollable() {
				// 如果支持轮询，等待可读事件
				// waitRead 会通过 epoll/kqueue 等机制等待连接到达
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue // 有新连接到达，重试 accept
				}
			}

		case syscall.ECONNABORTED:
			// This means that a socket on the listen
			// queue was closed before we Accept()ed it;
			// it's a silly error, so try again.
			//
			// 连接在我们接受之前就被对端关闭了
			// 这是常见的情况，直接重试即可
			continue
		}

		// 其他错误情况，返回错误
		return -1, nil, errcall, err // 返回错误信息
	}
}

// Fchmod wraps syscall.Fchmod.
func (fd *FD) Fchmod(mode uint32) error {
	if err := fd.incref(); err != nil {
		return err // 增加引用计数失败
	}
	defer fd.decref() // 确保在函数结束时减少引用计数
	return ignoringEINTR(func() error {
		return syscall.Fchmod(fd.Sysfd, mode) // 执行 Fchmod 操作
	})
}

// Fstat wraps syscall.Fstat
func (fd *FD) Fstat(s *syscall.Stat_t) error {
	if err := fd.incref(); err != nil {
		return err // 增加引用计数失败
	}
	defer fd.decref() // 确保在函数结束时减少引用计数
	return ignoringEINTR(func() error {
		return syscall.Fstat(fd.Sysfd, s) // 执行 Fstat 操作
	})
}

// dupCloexecUnsupported indicates whether F_DUPFD_CLOEXEC is supported by the kernel.
var dupCloexecUnsupported atomic.Bool

// DupCloseOnExec dups fd and marks it close-on-exec.
func DupCloseOnExec(fd int) (int, string, error) {
	if syscall.F_DUPFD_CLOEXEC != 0 && !dupCloexecUnsupported.Load() {
		r0, err := unix.Fcntl(fd, syscall.F_DUPFD_CLOEXEC, 0) // 执行 F_DUPFD_CLOEXEC 操作
		if err == nil {
			return r0, "", nil // 返回新的文件描述符
		}
		switch err {
		case syscall.EINVAL, syscall.ENOSYS:
			// Old kernel, or js/wasm (which returns
			// ENOSYS). Fall back to the portable way from
			// now on.
			dupCloexecUnsupported.Store(true) // 标记不支持
		default:
			return -1, "fcntl", err // 返回错误
		}
	}
	return dupCloseOnExecOld(fd) // 使用旧方法复制文件描述符
}

// Dup duplicates the file descriptor.
func (fd *FD) Dup() (int, string, error) {
	if err := fd.incref(); err != nil {
		return -1, "", err // 增加引用计数失败
	}
	defer fd.decref()               // 确保在函数结束时减少引用计数
	return DupCloseOnExec(fd.Sysfd) // 执行 DupCloseOnExec 操作
}

// On Unix variants only, expose the IO event for the net code.

// WaitWrite waits until data can be written to fd.
func (fd *FD) WaitWrite() error {
	return fd.pd.waitWrite(fd.isFile) // 等待直到可以写入
}

// WriteOnce is for testing only. It makes a single write call.
func (fd *FD) WriteOnce(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err // 获取写锁失败
	}
	defer fd.writeUnlock()                             // 确保在函数结束时释放写锁
	return ignoringEINTRIO(syscall.Write, fd.Sysfd, p) // 执行写入操作
}

// RawRead invokes the user-defined function f for a read operation.
func (fd *FD) RawRead(f func(uintptr) bool) error {
	if err := fd.readLock(); err != nil {
		return err // 获取读锁失败
	}
	defer fd.readUnlock() // 确保在函数结束时释放读锁
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return err // 准备读取失败
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil // 调用用户定义的读取函数成功
		}
		if err := fd.pd.waitRead(fd.isFile); err != nil {
			return err // 等待读取失败
		}
	}
}

// RawWrite invokes the user-defined function f for a write operation.
func (fd *FD) RawWrite(f func(uintptr) bool) error {
	if err := fd.writeLock(); err != nil {
		return err // 获取写锁失败
	}
	defer fd.writeUnlock() // 确保在函数结束时释放写锁
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return err // 准备写入失败
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil // 调用用户定义的写入函数成功
		}
		if err := fd.pd.waitWrite(fd.isFile); err != nil {
			return err // 等待写入失败
		}
	}
}

// ignoringEINTRIO is like ignoringEINTR, but just for IO calls.
func ignoringEINTRIO(fn func(fd int, p []byte) (int, error), fd int, p []byte) (int, error) {
	for {
		n, err := fn(fd, p) // 执行 I/O 操作
		if err != syscall.EINTR {
			return n, err // 返回结果
		}
	}
}
