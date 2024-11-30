// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements accept for platforms that do not provide a fast path for
// setting SetNonblock and CloseOnExec.

//go:build aix || darwin || (js && wasm) || wasip1

package poll

import (
	"syscall"
)

// Wrapper around the accept system call that marks the returned file
// descriptor as nonblocking and close-on-exec.
//
// accept 函数封装了 accept 系统调用，并为返回的文件描述符设置两个重要标志：
// 1. 非阻塞模式 (nonblocking)
// 2. 执行时关闭 (close-on-exec)
func accept(s int) (int, syscall.Sockaddr, string, error) {
	// See ../syscall/exec_unix.go for description of ForkLock.
	// It is probably okay to hold the lock across syscall.Accept
	// because we have put fd.sysfd into non-blocking mode.
	// However, a call to the File method will put it back into
	// blocking mode. We can't take that risk, so no use of ForkLock here.
	//
	// 这里不使用 ForkLock 的原因：
	// 1. 虽然 fd.sysfd 已经是非阻塞模式，持有锁可能是安全的
	// 2. 但 File 方法可能将其改回阻塞模式，这个风险不能承担

	// 接受新的连接
	// - ns: 新的 socket 文件描述符
	// - sa: 对端地址信息
	ns, sa, err := AcceptFunc(s)
	if err == nil {
		// 设置 close-on-exec 标志
		// 这确保在执行新程序时关闭此描述符
		syscall.CloseOnExec(ns)
	}
	if err != nil {
		return -1, nil, "accept", err
	}

	// 设置非阻塞模式
	// 这对于 Go 的网络 I/O 模型至关重要
	if err = syscall.SetNonblock(ns, true); err != nil {
		// 如果设置失败，清理资源并返回错误
		CloseFunc(ns)
		return -1, nil, "setnonblock", err
	}

	return ns, sa, "", nil
}
