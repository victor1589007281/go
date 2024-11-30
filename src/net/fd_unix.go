// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package net

import (
	"context"
	"internal/poll"
	"os"
	"runtime"
	"syscall"
)

const (
	readSyscallName     = "read"
	readFromSyscallName = "recvfrom"
	readMsgSyscallName  = "recvmsg"
	writeSyscallName    = "write"
	writeToSyscallName  = "sendto"
	writeMsgSyscallName = "sendmsg"
)

func newFD(sysfd, family, sotype int, net string) (*netFD, error) {
	ret := &netFD{
		pfd: poll.FD{
			Sysfd:         sysfd,
			IsStream:      sotype == syscall.SOCK_STREAM,
			ZeroReadIsEOF: sotype != syscall.SOCK_DGRAM && sotype != syscall.SOCK_RAW,
		},
		family: family,
		sotype: sotype,
		net:    net,
	}
	return ret, nil
}

// init 初始化网络文件描述符
//
// 该方法通过调用 poll.FD 的 Init 方法来初始化底层文件描述符
// - fd.net: 网络类型（如 "tcp"、"udp" 等）
// - true: 表示这是一个网络连接，需要设置为非阻塞模式
//
// 初始化过程包括：
// 1. 设置非阻塞模式
// 2. 设置关闭时执行的清理操作
// 3. 将文件描述符添加到运行时网络轮询器中
func (fd *netFD) init() error {
	// 调用 poll.FD 的 Init 方法完成实际的初始化
	return fd.pfd.Init(fd.net, true)
}

func (fd *netFD) name() string {
	var ls, rs string
	if fd.laddr != nil {
		ls = fd.laddr.String()
	}
	if fd.raddr != nil {
		rs = fd.raddr.String()
	}
	return fd.net + ":" + ls + "->" + rs
}

func (fd *netFD) connect(ctx context.Context, la, ra syscall.Sockaddr) (rsa syscall.Sockaddr, ret error) {
	// Do not need to call fd.writeLock here,
	// because fd is not yet accessible to user,
	// so no concurrent operations are possible.
	switch err := connectFunc(fd.pfd.Sysfd, ra); err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
	case nil, syscall.EISCONN:
		select {
		case <-ctx.Done():
			return nil, mapErr(ctx.Err())
		default:
		}
		if err := fd.pfd.Init(fd.net, true); err != nil {
			return nil, err
		}
		runtime.KeepAlive(fd)
		return nil, nil
	case syscall.EINVAL:
		// On Solaris and illumos we can see EINVAL if the socket has
		// already been accepted and closed by the server.  Treat this
		// as a successful connection--writes to the socket will see
		// EOF.  For details and a test case in C see
		// https://golang.org/issue/6828.
		if runtime.GOOS == "solaris" || runtime.GOOS == "illumos" {
			return nil, nil
		}
		fallthrough
	default:
		return nil, os.NewSyscallError("connect", err)
	}
	if err := fd.pfd.Init(fd.net, true); err != nil {
		return nil, err
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		fd.pfd.SetWriteDeadline(deadline)
		defer fd.pfd.SetWriteDeadline(noDeadline)
	}

	// Start the "interrupter" goroutine, if this context might be canceled.
	//
	// The interrupter goroutine waits for the context to be done and
	// interrupts the dial (by altering the fd's write deadline, which
	// wakes up waitWrite).
	ctxDone := ctx.Done()
	if ctxDone != nil {
		// Wait for the interrupter goroutine to exit before returning
		// from connect.
		done := make(chan struct{})
		interruptRes := make(chan error)
		defer func() {
			close(done)
			if ctxErr := <-interruptRes; ctxErr != nil && ret == nil {
				// The interrupter goroutine called SetWriteDeadline,
				// but the connect code below had returned from
				// waitWrite already and did a successful connect (ret
				// == nil). Because we've now poisoned the connection
				// by making it unwritable, don't return a successful
				// dial. This was issue 16523.
				ret = mapErr(ctxErr)
				fd.Close() // prevent a leak
			}
		}()
		go func() {
			select {
			case <-ctxDone:
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite
				// below.
				fd.pfd.SetWriteDeadline(aLongTimeAgo)
				testHookCanceledDial()
				interruptRes <- ctx.Err()
			case <-done:
				interruptRes <- nil
			}
		}()
	}

	for {
		// Performing multiple connect system calls on a
		// non-blocking socket under Unix variants does not
		// necessarily result in earlier errors being
		// returned. Instead, once runtime-integrated network
		// poller tells us that the socket is ready, get the
		// SO_ERROR socket option to see if the connection
		// succeeded or failed. See issue 7474 for further
		// details.
		if err := fd.pfd.WaitWrite(); err != nil {
			select {
			case <-ctxDone:
				return nil, mapErr(ctx.Err())
			default:
			}
			return nil, err
		}
		nerr, err := getsockoptIntFunc(fd.pfd.Sysfd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return nil, os.NewSyscallError("getsockopt", err)
		}
		switch err := syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		case syscall.EISCONN:
			return nil, nil
		case syscall.Errno(0):
			// The runtime poller can wake us up spuriously;
			// see issues 14548 and 19289. Check that we are
			// really connected; if not, wait again.
			if rsa, err := syscall.Getpeername(fd.pfd.Sysfd); err == nil {
				return rsa, nil
			}
		default:
			return nil, os.NewSyscallError("connect", err)
		}
		runtime.KeepAlive(fd)
	}
}

// accept 接受一个新的连接并返回对应的网络文件描述符
//
// 该方法完成从底层接受连接到创建新的网络文件描述符的完整过程
func (fd *netFD) accept() (netfd *netFD, err error) {
	// 调用底层的 Accept 方法接受新连接
	// - d: 新连接的文件描述符
	// - rsa: 远程地址
	// - errcall: 如果出错，指示是哪个系统调用失败
	// 接收一个连接，如果连接没有到达则堵塞当前协程
	d, rsa, errcall, err := fd.pfd.Accept()
	if err != nil {
		if errcall != "" {
			// 如果有系统调用名，包装成系统调用错误
			err = wrapSyscallError(errcall, err)
		}
		return nil, err
	}

	// 使用新的文件描述符创建 netFD 对象
	// 继承原监听器的地址族、套接字类型和网络类型
	// 把新到的连接也添加到epoll中进行管理
	if netfd, err = newFD(d, fd.family, fd.sotype, fd.net); err != nil {
		// 如果创建失败，关闭文件描述符
		poll.CloseFunc(d)
		return nil, err
	}

	// 初始化新的文件描述符（设置非阻塞模式等）
	if err = netfd.init(); err != nil {
		// 如果初始化失败，关闭整个网络文件描述符
		netfd.Close()
		return nil, err
	}

	// 获取本地地址并设置连接的本地和远程地址
	lsa, _ := syscall.Getsockname(netfd.pfd.Sysfd)
	netfd.setAddr(netfd.addrFunc()(lsa), netfd.addrFunc()(rsa))

	return netfd, nil
}

// Defined in os package.
func newUnixFile(fd int, name string) *os.File

func (fd *netFD) dup() (f *os.File, err error) {
	ns, call, err := fd.pfd.Dup()
	if err != nil {
		if call != "" {
			err = os.NewSyscallError(call, err)
		}
		return nil, err
	}

	return newUnixFile(ns, fd.name()), nil
}
