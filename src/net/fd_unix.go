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

// connect 建立网络连接
//
// 参数:
// - ctx: 上下文，用于控制连接超时和取消
// - la: 本地地址 (Local Address)
// - ra: 远程地址 (Remote Address)
//
// 返回:
// - rsa: 远程套接字地址
// - ret: 错误信息
func (fd *netFD) connect(ctx context.Context, la, ra syscall.Sockaddr) (rsa syscall.Sockaddr, ret error) {
	// Do not need to call fd.writeLock here,
	// because fd is not yet accessible to user,
	// so no concurrent operations are possible.
	// 不需要获取写锁，因为此时文件描述符还未暴露给用户，不存在并发操作
	//调用系统调用connect
	switch err := connectFunc(fd.pfd.Sysfd, ra); err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		// 连接正在进行中，继续后续处理
	case nil, syscall.EISCONN:
		// 连接成功或已经连接
		select {
		case <-ctx.Done():
			return nil, mapErr(ctx.Err())
		default:
		}
		//把文件描述符设置为非阻塞模式，同时加入到epoll实例中
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
		// Solaris 和 illumos 系统特殊处理：
		// 如果服务器已经接受并关闭了socket，会返回EINVAL
		// 这种情况下将其视为连接成功，后续写入会收到EOF
		if runtime.GOOS == "solaris" || runtime.GOOS == "illumos" {
			return nil, nil
		}
		fallthrough
	default:
		return nil, os.NewSyscallError("connect", err)
	}
	//把文件描述符设置为非阻塞模式，同时加入到epoll实例中
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
	// 如果上下文可能被取消，启动中断器goroutine
	ctxDone := ctx.Done()
	if ctxDone != nil {
		// Wait for the interrupter goroutine to exit before returning
		// from connect.
		// 创建通道用于等待中断器goroutine退出
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
				// 如果中断器调用了SetWriteDeadline，但连接已成功建立
				// 由于连接已被设置为不可写，不应返回成功
				// 这是为了修复issue 16523
				ret = mapErr(ctxErr)
				fd.Close() // 防止泄漏
			}
		}()

		// 启动中断器goroutine
		go func() {
			select {
			case <-ctxDone:
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite
				// below.
				// 强制运行时轮询器立即放弃等待可写性
				fd.pfd.SetWriteDeadline(aLongTimeAgo)
				testHookCanceledDial()
				interruptRes <- ctx.Err()
			case <-done:
				interruptRes <- nil
			}
		}()
	}

	// 循环尝试建立连接
	for {
		// Performing multiple connect system calls on a
		// non-blocking socket under Unix variants does not
		// necessarily result in earlier errors being
		// returned. Instead, once runtime-integrated network
		// poller tells us that the socket is ready, get the
		// SO_ERROR socket option to see if the connection
		// succeeded or failed. See issue 7474 for further
		// details.
		// Unix系统下对非阻塞socket多次调用connect
		// 不一定能立即返回早期的错误
		// 因此，等待运行时网络轮询器通知socket就绪后
		// 通过SO_ERROR选项检查连接是否成功
		// 详见issue 7474
		if err := fd.pfd.WaitWrite(); err != nil {
			select {
			case <-ctxDone:
				return nil, mapErr(ctx.Err())
			default:
			}
			return nil, err
		}

		// 获取socket错误状态
		nerr, err := getsockoptIntFunc(fd.pfd.Sysfd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return nil, os.NewSyscallError("getsockopt", err)
		}

		switch err := syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
			// 连接仍在进行中，继续等待
		case syscall.EISCONN:
			// 已连接成功
			return nil, nil
		case syscall.Errno(0):
			// The runtime poller can wake us up spuriously;
			// see issues 14548 and 19289. Check that we are
			// really connected; if not, wait again.
			// 运行时轮询器可能会虚假唤醒（见issues 14548和19289）
			// 通过Getpeername确认是否真正连接；若未连接，继续等待
			if rsa, err := syscall.Getpeername(fd.pfd.Sysfd); err == nil {
				return rsa, nil
			}
		default:
			return nil, os.NewSyscallError("connect", err)
		}
		runtime.KeepAlive(fd) // 防止fd被过早回收
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
