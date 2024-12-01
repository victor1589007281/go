// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || js || wasip1 || windows

package net

import (
	"context"
	"io"
	"os"
	"syscall"
)

func sockaddrToTCP(sa syscall.Sockaddr) Addr {
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		return &TCPAddr{IP: sa.Addr[0:], Port: sa.Port}
	case *syscall.SockaddrInet6:
		return &TCPAddr{IP: sa.Addr[0:], Port: sa.Port, Zone: zoneCache.name(int(sa.ZoneId))}
	}
	return nil
}

func (a *TCPAddr) family() int {
	if a == nil || len(a.IP) <= IPv4len {
		return syscall.AF_INET
	}
	if a.IP.To4() != nil {
		return syscall.AF_INET
	}
	return syscall.AF_INET6
}

func (a *TCPAddr) sockaddr(family int) (syscall.Sockaddr, error) {
	// 如果 TCP 地址为空，直接返回 nil
	if a == nil {
		return nil, nil
	}
	// 将 IP 地址、端口和区域信息转换为系统层面的 sockaddr 结构
	return ipToSockaddr(family, a.IP, a.Port, a.Zone)
}

func (a *TCPAddr) toLocal(net string) sockaddr {
	return &TCPAddr{loopbackIP(net), a.Port, a.Zone}
}

func (c *TCPConn) readFrom(r io.Reader) (int64, error) {
	if n, err, handled := spliceFrom(c.fd, r); handled {
		return n, err
	}
	if n, err, handled := sendFile(c.fd, r); handled {
		return n, err
	}
	return genericReadFrom(c, r)
}

func (c *TCPConn) writeTo(w io.Writer) (int64, error) {
	if n, err, handled := spliceTo(w, c.fd); handled {
		return n, err
	}
	return genericWriteTo(c, w)
}

func (sd *sysDialer) dialTCP(ctx context.Context, laddr, raddr *TCPAddr) (*TCPConn, error) {
	// 检查是否存在系统拨号器级别的测试钩子函数
	if h := sd.testHookDialTCP; h != nil {
		return h(ctx, sd.network, laddr, raddr)
	}
	// 检查是否存在全局级别的测试钩子函数
	if h := testHookDialTCP; h != nil {
		return h(ctx, sd.network, laddr, raddr)
	}
	// 如果没有测试钩子，则执行实际的 TCP 连接操作
	return sd.doDialTCP(ctx, laddr, raddr)
}

// doDialTCP 是一个简单的包装函数，调用 doDialTCPProto 并使用默认协议号(0)
func (sd *sysDialer) doDialTCP(ctx context.Context, laddr, raddr *TCPAddr) (*TCPConn, error) {
	return sd.doDialTCPProto(ctx, laddr, raddr, 0)
}

// doDialTCPProto 执行实际的 TCP 连接操作
//
// 参数说明：
// - ctx: 上下文，用于控制连接超时等
// - laddr: 本地地址，可以为 nil
// - raddr: 远程地址，建立连接的目标地址
// - proto: 协议号，0 表示使用默认 TCP 协议
func (sd *sysDialer) doDialTCPProto(ctx context.Context, laddr, raddr *TCPAddr, proto int) (*TCPConn, error) {
	// 获取用户配置的连接控制函数
	ctrlCtxFn := sd.Dialer.ControlContext
	// 如果没有设置 ControlContext 但设置了 Control，则创建一个包装函数
	if ctrlCtxFn == nil && sd.Dialer.Control != nil {
		ctrlCtxFn = func(ctx context.Context, network, address string, c syscall.RawConn) error {
			return sd.Dialer.Control(network, address, c)
		}
	}

	// 创建 TCP socket 并进行连接
	fd, err := internetSocket(ctx, sd.network, laddr, raddr, syscall.SOCK_STREAM, proto, "dial", ctrlCtxFn)

	// TCP has a rarely used mechanism called a 'simultaneous connection' in
	// which Dial("tcp", addr1, addr2) run on the machine at addr1 can
	// connect to a simultaneous Dial("tcp", addr2, addr1) run on the machine
	// at addr2, without either machine executing Listen. If laddr == nil,
	// it means we want the kernel to pick an appropriate originating local
	// address. Some Linux kernels cycle blindly through a fixed range of
	// local ports, regardless of destination port. If a kernel happens to
	// pick local port 50001 as the source for a Dial("tcp", "", "localhost:50001"),
	// then the Dial will succeed, having simultaneously connected to itself.
	// This can only happen when we are letting the kernel pick a port (laddr == nil)
	// and when there is no listener for the destination address.
	// It's hard to argue this is anything other than a kernel bug. If we
	// see this happen, rather than expose the buggy effect to users, we
	// close the fd and try again. If it happens twice more, we relent and
	// use the result. See also:
	//	https://golang.org/issue/2690
	//	https://stackoverflow.com/questions/4949858/
	//
	// The opposite can also happen: if we ask the kernel to pick an appropriate
	// originating local address, sometimes it picks one that is already in use.
	// So if the error is EADDRNOTAVAIL, we have to try again too, just for
	// a different reason.
	//
	// The kernel socket code is no doubt enjoying watching us squirm.
	// 处理特殊情况：自连接(self-connect)和地址不可用
	// 当本地地址为空或端口为0时，内核会自动选择地址和端口
	// 在某些 Linux 内核中可能会出现以下问题：
	// 1. 自连接：内核选择的源端口恰好是目标端口，导致连接到自己
	// 2. 地址不可用：内核选择的地址已被占用
	//
	// 如果发生这些情况，最多重试两次
	for i := 0; i < 2 && (laddr == nil || laddr.Port == 0) && (selfConnect(fd, err) || spuriousENOTAVAIL(err)); i++ {
		if err == nil {
			fd.Close()
		}
		fd, err = internetSocket(ctx, sd.network, laddr, raddr, syscall.SOCK_STREAM, proto, "dial", ctrlCtxFn)
	}

	// 如果连接出错，返回错误
	if err != nil {
		return nil, err
	}

	// 创建并返回新的 TCP 连接对象
	// 设置 keepalive 相关配置和测试钩子
	return newTCPConn(fd, sd.Dialer.KeepAlive, sd.Dialer.KeepAliveConfig, testPreHookSetKeepAlive, testHookSetKeepAlive), nil
}

func selfConnect(fd *netFD, err error) bool {
	// If the connect failed, we clearly didn't connect to ourselves.
	if err != nil {
		return false
	}

	// The socket constructor can return an fd with raddr nil under certain
	// unknown conditions. The errors in the calls there to Getpeername
	// are discarded, but we can't catch the problem there because those
	// calls are sometimes legally erroneous with a "socket not connected".
	// Since this code (selfConnect) is already trying to work around
	// a problem, we make sure if this happens we recognize trouble and
	// ask the DialTCP routine to try again.
	// TODO: try to understand what's really going on.
	if fd.laddr == nil || fd.raddr == nil {
		return true
	}
	l := fd.laddr.(*TCPAddr)
	r := fd.raddr.(*TCPAddr)
	return l.Port == r.Port && l.IP.Equal(r.IP)
}

func spuriousENOTAVAIL(err error) bool {
	if op, ok := err.(*OpError); ok {
		err = op.Err
	}
	if sys, ok := err.(*os.SyscallError); ok {
		err = sys.Err
	}
	return err == syscall.EADDRNOTAVAIL
}

func (ln *TCPListener) ok() bool { return ln != nil && ln.fd != nil }

// accept 接受一个新的 TCP 连接
//
// 该方法是 TCPListener.Accept 的内部实现，它：
// 1. 接受新的连接并获取对应的文件描述符
// 2. 使用该文件描述符创建的 TCP 连接对象
func (ln *TCPListener) accept() (*TCPConn, error) {
	// 调用底层文件描述符的 accept 方法接受新连接
	// 返回新连接的文件描述符
	fd, err := ln.fd.accept()
	if err != nil {
		return nil, err
	}

	// 创建并返回新的 TCP 连接对象，设置：
	// - fd: 新连接的文件描述符
	// - KeepAlive: TCP keepalive 设置
	// - KeepAliveConfig: keepalive 的详细配置
	// - testPreHookSetKeepAlive, testHookSetKeepAlive: 用于测试的钩子函数
	return newTCPConn(fd, ln.lc.KeepAlive, ln.lc.KeepAliveConfig, testPreHookSetKeepAlive, testHookSetKeepAlive), nil
}

func (ln *TCPListener) close() error {
	return ln.fd.Close()
}

func (ln *TCPListener) file() (*os.File, error) {
	f, err := ln.fd.dup()
	if err != nil {
		return nil, err
	}
	return f, nil
}

// listenTCP 创建一个 TCP 网络监听器
//
// 参数说明：
// - ctx: 上下文，用于控制操作的生命周期
// - laddr: 本地 TCP 地址，指定监听的地址和端口
//
// 内部调用 listenTCPProto 并使用默认协议号(0)创建监听器
func (sl *sysListener) listenTCP(ctx context.Context, laddr *TCPAddr) (*TCPListener, error) {
	// 调用 listenTCPProto，最后一个参数 0 表示使用默认的 TCP 协议
	return sl.listenTCPProto(ctx, laddr, 0)
}

// listenTCPProto 使用指定的协议创建 TCP 网络监听器
//
// 参数说明：
// - ctx: 上下文，用于控制操作的生命周期
// - laddr: 本地 TCP 地址，指定监听的地址和端口
// - proto: 协议号，0 表示使用默认 TCP 协议
func (sl *sysListener) listenTCPProto(ctx context.Context, laddr *TCPAddr, proto int) (*TCPListener, error) {
	// 定义用于控制底层连接的函数变量
	var ctrlCtxFn func(ctx context.Context, network, address string, c syscall.RawConn) error

	// 如果配置中指定了自定义的控制函数，则创建包装函数
	if sl.ListenConfig.Control != nil {
		ctrlCtxFn = func(ctx context.Context, network, address string, c syscall.RawConn) error {
			return sl.ListenConfig.Control(network, address, c)
		}
	}

	// 创建底层网络套接字
	// - sl.network: 网络类型（如 "tcp"、"tcp4"、"tcp6"）
	// - laddr: 本地地址
	// - nil: 远程地址（监听时为空）
	// - syscall.SOCK_STREAM: 表示使用面向连接的 TCP 协议
	// - proto: 指定协议号
	// - "listen": 操作类型
	fd, err := internetSocket(ctx, sl.network, laddr, nil, syscall.SOCK_STREAM, proto, "listen", ctrlCtxFn)
	if err != nil {
		return nil, err
	}

	// 创建并返回 TCP 监听器对象，包含：
	// - fd: 底层文件描述符
	// - lc: 监听器配置
	return &TCPListener{fd: fd, lc: sl.ListenConfig}, nil
}
