// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || windows

package net

import (
	"context"
	"internal/poll"
	"os"
	"syscall"
)

// socket returns a network file descriptor that is ready for
// asynchronous I/O using the network poller.
//
// socket 返回一个网络文件描述符，该描述符已准备好使用网络轮询器进行异步 I/O
func socket(ctx context.Context, net string, family, sotype, proto int, ipv6only bool, laddr, raddr sockaddr, ctrlCtxFn func(context.Context, string, string, syscall.RawConn) error) (fd *netFD, err error) {
	// 创建系统套接字
	s, err := sysSocket(family, sotype, proto)
	if err != nil {
		return nil, err
	}

	// 设置默认的套接字选项（如 IPv6 only、地址重用等）
	if err = setDefaultSockopts(s, family, sotype, ipv6only); err != nil {
		poll.CloseFunc(s)
		return nil, err
	}

	// 创建网络文件描述符
	if fd, err = newFD(s, family, sotype, net); err != nil {
		poll.CloseFunc(s)
		return nil, err
	}

	// This function makes a network file descriptor for the
	// following applications:
	//
	// 此函数为以下应用场景创建网络文件描述符：
	//
	// - An endpoint holder that opens a passive stream
	//   connection, known as a stream listener
	// - 流监听器：用于打开被动流连接的端点持有者
	//
	// - An endpoint holder that opens a destination-unspecific
	//   datagram connection, known as a datagram listener
	// - 数据报监听器：用于打开未指定目标的数据报连接的端点持有者
	//
	// - An endpoint holder that opens an active stream or a
	//   destination-specific datagram connection, known as a dialer
	// - 拨号器：用于打开主动流连接或指定目标的数据报连接的端点持有者
	//
	// - An endpoint holder that opens the other connection, such
	//   as talking to the protocol stack inside the kernel
	// - 其他连接：如与内核协议栈通信的端点持有者

	// 当有本地地址但没有远程地址时，表示这是一个监听器
	if laddr != nil && raddr == nil {
		switch sotype {
		case syscall.SOCK_STREAM, syscall.SOCK_SEQPACKET:
			// 对于流式套接字，设置为流监听模式
			if err := fd.listenStream(ctx, laddr, listenerBacklog(), ctrlCtxFn); err != nil {
				fd.Close()
				return nil, err
			}
			return fd, nil
		case syscall.SOCK_DGRAM:
			// 对于数据报套接字，设置为数据报监听模式
			if err := fd.listenDatagram(ctx, laddr, ctrlCtxFn); err != nil {
				fd.Close()
				return nil, err
			}
			return fd, nil
		}
	}

	// 如果不是监听模式，则进行连接操作
	if err := fd.dial(ctx, laddr, raddr, ctrlCtxFn); err != nil {
		fd.Close()
		return nil, err
	}
	return fd, nil
}

func (fd *netFD) ctrlNetwork() string {
	switch fd.net {
	case "unix", "unixgram", "unixpacket":
		return fd.net
	}
	switch fd.net[len(fd.net)-1] {
	case '4', '6':
		return fd.net
	}
	if fd.family == syscall.AF_INET {
		return fd.net + "4"
	}
	return fd.net + "6"
}

func (fd *netFD) dial(ctx context.Context, laddr, raddr sockaddr, ctrlCtxFn func(context.Context, string, string, syscall.RawConn) error) error {
	var c *rawConn
	if ctrlCtxFn != nil {
		c = newRawConn(fd)
		var ctrlAddr string
		if raddr != nil {
			ctrlAddr = raddr.String()
		} else if laddr != nil {
			ctrlAddr = laddr.String()
		}
		if err := ctrlCtxFn(ctx, fd.ctrlNetwork(), ctrlAddr, c); err != nil {
			return err
		}
	}

	var lsa syscall.Sockaddr
	var err error
	if laddr != nil {
		if lsa, err = laddr.sockaddr(fd.family); err != nil {
			return err
		} else if lsa != nil {
			if err = syscall.Bind(fd.pfd.Sysfd, lsa); err != nil {
				return os.NewSyscallError("bind", err)
			}
		}
	}
	var rsa syscall.Sockaddr  // remote address from the user
	var crsa syscall.Sockaddr // remote address we actually connected to
	if raddr != nil {
		if rsa, err = raddr.sockaddr(fd.family); err != nil {
			return err
		}
		if crsa, err = fd.connect(ctx, lsa, rsa); err != nil {
			return err
		}
		fd.isConnected = true
	} else {
		if err := fd.init(); err != nil {
			return err
		}
	}
	// Record the local and remote addresses from the actual socket.
	// Get the local address by calling Getsockname.
	// For the remote address, use
	// 1) the one returned by the connect method, if any; or
	// 2) the one from Getpeername, if it succeeds; or
	// 3) the one passed to us as the raddr parameter.
	lsa, _ = syscall.Getsockname(fd.pfd.Sysfd)
	if crsa != nil {
		fd.setAddr(fd.addrFunc()(lsa), fd.addrFunc()(crsa))
	} else if rsa, _ = syscall.Getpeername(fd.pfd.Sysfd); rsa != nil {
		fd.setAddr(fd.addrFunc()(lsa), fd.addrFunc()(rsa))
	} else {
		fd.setAddr(fd.addrFunc()(lsa), raddr)
	}
	return nil
}

// listenStream 设置流式套接字（如 TCP）的监听
//
// 参数说明：
// - ctx: 上下文，用于控制操作的生命周期
// - laddr: 要监听的本地地址
// - backlog: 监听队列的最大长度
// - ctrlCtxFn: 用于自定义控制套接字的回调函数
func (fd *netFD) listenStream(ctx context.Context, laddr sockaddr, backlog int, ctrlCtxFn func(context.Context, string, string, syscall.RawConn) error) error {
	var err error
	// 设置监听套接字的默认选项（如地址重用等）
	if err = setDefaultListenerSockopts(fd.pfd.Sysfd); err != nil {
		return err
	}

	// 将高层地址转换为系统层套接字地址
	var lsa syscall.Sockaddr
	if lsa, err = laddr.sockaddr(fd.family); err != nil {
		return err
	}

	// 如果提供了控制函数，执行自定义的套接字配置
	if ctrlCtxFn != nil {
		c := newRawConn(fd)
		if err := ctrlCtxFn(ctx, fd.ctrlNetwork(), laddr.String(), c); err != nil {
			return err
		}
	}

	// 将套接字绑定到指定的本地地址
	if err = syscall.Bind(fd.pfd.Sysfd, lsa); err != nil {
		return os.NewSyscallError("bind", err)
	}

	// 开始监听连接请求
	if err = listenFunc(fd.pfd.Sysfd, backlog); err != nil {
		return os.NewSyscallError("listen", err)
	}

	// 初始化网络文件描述符（设置非阻塞模式等）
	if err = fd.init(); err != nil {
		return err
	}

	// 获取实际绑定的地址（可能与请求的地址不同，比如使用了随机端口）
	lsa, _ = syscall.Getsockname(fd.pfd.Sysfd)
	// 设置本地地址，远程地址为 nil（因为是监听套接字）
	fd.setAddr(fd.addrFunc()(lsa), nil)
	return nil
}

func (fd *netFD) listenDatagram(ctx context.Context, laddr sockaddr, ctrlCtxFn func(context.Context, string, string, syscall.RawConn) error) error {
	switch addr := laddr.(type) {
	case *UDPAddr:
		// We provide a socket that listens to a wildcard
		// address with reusable UDP port when the given laddr
		// is an appropriate UDP multicast address prefix.
		// This makes it possible for a single UDP listener to
		// join multiple different group addresses, for
		// multiple UDP listeners that listen on the same UDP
		// port to join the same group address.
		if addr.IP != nil && addr.IP.IsMulticast() {
			if err := setDefaultMulticastSockopts(fd.pfd.Sysfd); err != nil {
				return err
			}
			addr := *addr
			switch fd.family {
			case syscall.AF_INET:
				addr.IP = IPv4zero
			case syscall.AF_INET6:
				addr.IP = IPv6unspecified
			}
			laddr = &addr
		}
	}
	var err error
	var lsa syscall.Sockaddr
	if lsa, err = laddr.sockaddr(fd.family); err != nil {
		return err
	}

	if ctrlCtxFn != nil {
		c := newRawConn(fd)
		if err := ctrlCtxFn(ctx, fd.ctrlNetwork(), laddr.String(), c); err != nil {
			return err
		}
	}
	if err = syscall.Bind(fd.pfd.Sysfd, lsa); err != nil {
		return os.NewSyscallError("bind", err)
	}
	if err = fd.init(); err != nil {
		return err
	}
	lsa, _ = syscall.Getsockname(fd.pfd.Sysfd)
	fd.setAddr(fd.addrFunc()(lsa), nil)
	return nil
}
