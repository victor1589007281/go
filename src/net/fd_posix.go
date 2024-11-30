// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || windows

package net

import (
	"internal/poll"
	"runtime"
	"syscall"
	"time"
)

// Network file descriptor.
// 网络文件描述符。
type netFD struct {
	pfd poll.FD // poll.FD 结构体，用于处理文件描述符的轮询

	// immutable until Close
	family      int    // 地址族（如 AF_INET, AF_INET6）
	sotype      int    // 套接字类型（如 SOCK_STREAM, SOCK_DGRAM）
	isConnected bool   // 是否已连接（握手完成或与对等方的关联使用）
	net         string // 网络类型（如 "tcp", "udp"）
	laddr       Addr   // 本地地址
	raddr       Addr   // 远程地址
}

// setAddr sets the local and remote addresses for the netFD.
// 设置 netFD 的本地和远程地址。
func (fd *netFD) setAddr(laddr, raddr Addr) {
	fd.laddr = laddr
	fd.raddr = raddr
	runtime.SetFinalizer(fd, (*netFD).Close) // 设置最终化器，关闭时调用 Close 方法
}

// Close closes the network file descriptor.
// 关闭网络文件描述符。
func (fd *netFD) Close() error {
	runtime.SetFinalizer(fd, nil) // 移除最终化器
	return fd.pfd.Close()         // 关闭文件描述符
}

// shutdown performs a shutdown on the network file descriptor.
// 对网络文件描述符执行关闭操作。
func (fd *netFD) shutdown(how int) error {
	err := fd.pfd.Shutdown(how)              // 调用 poll.FD 的 Shutdown 方法
	runtime.KeepAlive(fd)                    // 确保 fd 在调用期间不被垃圾回收
	return wrapSyscallError("shutdown", err) // 包装系统调用错误
}

// closeRead closes the read side of the network file descriptor.
// 关闭网络文件描述符的读取端。
func (fd *netFD) closeRead() error {
	return fd.shutdown(syscall.SHUT_RD) // 关闭读取
}

// closeWrite closes the write side of the network file descriptor.
// 关闭网络文件描述符的写入端。
func (fd *netFD) closeWrite() error {
	return fd.shutdown(syscall.SHUT_WR) // 关闭写入
}

// Read reads data from the network file descriptor.
// 从网络文件描述符读取数据。
func (fd *netFD) Read(p []byte) (n int, err error) {
	n, err = fd.pfd.Read(p)                          // 调用 poll.FD 的 Read 方法
	runtime.KeepAlive(fd)                            // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(readSyscallName, err) // 包装系统调用错误
}

// readFrom reads data from the network file descriptor and returns the source address.
// 从网络文件描述符读取数据并返回源地址。
func (fd *netFD) readFrom(p []byte) (n int, sa syscall.Sockaddr, err error) {
	n, sa, err = fd.pfd.ReadFrom(p)                          // 调用 poll.FD 的 ReadFrom 方法
	runtime.KeepAlive(fd)                                    // 确保 fd 在调用期间不被垃圾回收
	return n, sa, wrapSyscallError(readFromSyscallName, err) // 包装系统调用错误
}

// readFromInet4 reads data from the network file descriptor for IPv4 addresses.
// 从网络文件描述符读取 IPv4 地址的数据。
func (fd *netFD) readFromInet4(p []byte, from *syscall.SockaddrInet4) (n int, err error) {
	n, err = fd.pfd.ReadFromInet4(p, from)               // 调用 poll.FD 的 ReadFromInet4 方法
	runtime.KeepAlive(fd)                                // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(readFromSyscallName, err) // 包装系统调用错误
}

// readFromInet6 reads data from the network file descriptor for IPv6 addresses.
// 从网络文件描述符读取 IPv6 地址的数据。
func (fd *netFD) readFromInet6(p []byte, from *syscall.SockaddrInet6) (n int, err error) {
	n, err = fd.pfd.ReadFromInet6(p, from)               // 调用 poll.FD 的 ReadFromInet6 方法
	runtime.KeepAlive(fd)                                // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(readFromSyscallName, err) // 包装系统调用错误
}

// readMsg reads a message from the network file descriptor.
// 从网络文件描述符读取消息。
func (fd *netFD) readMsg(p []byte, oob []byte, flags int) (n, oobn, retflags int, sa syscall.Sockaddr, err error) {
	n, oobn, retflags, sa, err = fd.pfd.ReadMsg(p, oob, flags)              // 调用 poll.FD 的 ReadMsg 方法
	runtime.KeepAlive(fd)                                                   // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, retflags, sa, wrapSyscallError(readMsgSyscallName, err) // 包装系统调用错误
}

// readMsgInet4 reads a message from the network file descriptor for IPv4 addresses.
// 从网络文件描述符读取 IPv4 地址的消息。
func (fd *netFD) readMsgInet4(p []byte, oob []byte, flags int, sa *syscall.SockaddrInet4) (n, oobn, retflags int, err error) {
	n, oobn, retflags, err = fd.pfd.ReadMsgInet4(p, oob, flags, sa)     // 调用 poll.FD 的 ReadMsgInet4 方法
	runtime.KeepAlive(fd)                                               // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err) // 包装系统调用错误
}

// readMsgInet6 reads a message from the network file descriptor for IPv6 addresses.
// 从网络文件描述符读取 IPv6 地址的消息。
func (fd *netFD) readMsgInet6(p []byte, oob []byte, flags int, sa *syscall.SockaddrInet6) (n, oobn, retflags int, err error) {
	n, oobn, retflags, err = fd.pfd.ReadMsgInet6(p, oob, flags, sa)     // 调用 poll.FD 的 ReadMsgInet6 方法
	runtime.KeepAlive(fd)                                               // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err) // 包装系统调用错误
}

// Write writes data to the network file descriptor.
// 向网络文件描述符写入数据。
func (fd *netFD) Write(p []byte) (nn int, err error) {
	nn, err = fd.pfd.Write(p)                          // 调用 poll.FD 的 Write 方法
	runtime.KeepAlive(fd)                              // 确保 fd 在调用期间不被垃圾回收
	return nn, wrapSyscallError(writeSyscallName, err) // 包装系统调用错误
}

// writeTo writes data to the specified address.
// 向指定地址写入数据。
func (fd *netFD) writeTo(p []byte, sa syscall.Sockaddr) (n int, err error) {
	n, err = fd.pfd.WriteTo(p, sa)                      // 调用 poll.FD ��� WriteTo 方法
	runtime.KeepAlive(fd)                               // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(writeToSyscallName, err) // 包装系统调用错误
}

// writeToInet4 writes data to the specified IPv4 address.
// 向指定的 IPv4 地址写入数据。
func (fd *netFD) writeToInet4(p []byte, sa *syscall.SockaddrInet4) (n int, err error) {
	n, err = fd.pfd.WriteToInet4(p, sa)                 // 调用 poll.FD 的 WriteToInet4 方法
	runtime.KeepAlive(fd)                               // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(writeToSyscallName, err) // 包装系统调用错误
}

// writeToInet6 writes data to the specified IPv6 address.
// 向指定的 IPv6 地址写入数据。
func (fd *netFD) writeToInet6(p []byte, sa *syscall.SockaddrInet6) (n int, err error) {
	n, err = fd.pfd.WriteToInet6(p, sa)                 // 调用 poll.FD 的 WriteToInet6 方法
	runtime.KeepAlive(fd)                               // 确保 fd 在调用期间不被垃圾回收
	return n, wrapSyscallError(writeToSyscallName, err) // 包装系统调用错误
}

// writeMsg writes a message to the network file descriptor.
// 向网络文件描述符写入消息。
func (fd *netFD) writeMsg(p []byte, oob []byte, sa syscall.Sockaddr) (n int, oobn int, err error) {
	n, oobn, err = fd.pfd.WriteMsg(p, oob, sa)                 // 调用 poll.FD 的 WriteMsg 方法
	runtime.KeepAlive(fd)                                      // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err) // 包装系统调用错误
}

// writeMsgInet4 writes a message to the specified IPv4 address.
// 向指定的 IPv4 地址写入消息。
func (fd *netFD) writeMsgInet4(p []byte, oob []byte, sa *syscall.SockaddrInet4) (n int, oobn int, err error) {
	n, oobn, err = fd.pfd.WriteMsgInet4(p, oob, sa)            // 调用 poll.FD 的 WriteMsgInet4 方法
	runtime.KeepAlive(fd)                                      // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err) // 包装系统调用错误
}

// writeMsgInet6 writes a message to the specified IPv6 address.
// 向指定的 IPv6 地址写入消息。
func (fd *netFD) writeMsgInet6(p []byte, oob []byte, sa *syscall.SockaddrInet6) (n int, oobn int, err error) {
	n, oobn, err = fd.pfd.WriteMsgInet6(p, oob, sa)            // 调用 poll.FD 的 WriteMsgInet6 方法
	runtime.KeepAlive(fd)                                      // 确保 fd 在调用期间不被垃圾回收
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err) // 包装系统调用错误
}

// SetDeadline sets the read and write deadlines associated with the network file descriptor.
// 设置与网络文件描述符相关的读写截止时间。
func (fd *netFD) SetDeadline(t time.Time) error {
	return fd.pfd.SetDeadline(t) // 调用 poll.FD 的 SetDeadline 方法
}

// SetReadDeadline sets the read deadline associated with the network file descriptor.
// 设置与网络文件描述符相关的读截止时间。
func (fd *netFD) SetReadDeadline(t time.Time) error {
	return fd.pfd.SetReadDeadline(t) // 调用 poll.FD 的 SetReadDeadline 方法
}

// SetWriteDeadline sets the write deadline associated with the network file descriptor.
// 设置与网络文件描述符相关的写截止时间。
func (fd *netFD) SetWriteDeadline(t time.Time) error {
	return fd.pfd.SetWriteDeadline(t) // 调用 poll.FD 的 SetWriteDeadline 方法
}
