// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || windows || wasip1

package poll

import (
	"errors"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

// runtimeNano returns the current value of the runtime clock in nanoseconds.
//
//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

func runtime_pollServerInit()                                // 初始化轮询服务器
func runtime_pollOpen(fd uintptr) (uintptr, int)             // 打开文件描述符进行轮询
func runtime_pollClose(ctx uintptr)                          // 关闭轮询上下文
func runtime_pollWait(ctx uintptr, mode int) int             // 等待文件描述符就绪
func runtime_pollWaitCanceled(ctx uintptr, mode int)         // 等待被取消的轮询
func runtime_pollReset(ctx uintptr, mode int) int            // 重置轮询上下文
func runtime_pollSetDeadline(ctx uintptr, d int64, mode int) // 设置轮询截止时间
func runtime_pollUnblock(ctx uintptr)                        // 解除轮询阻塞
func runtime_isPollServerDescriptor(fd uintptr) bool         // 检查是否为轮询服务器描述符

type pollDesc struct {
	runtimeCtx uintptr // 轮询上下文
}

var serverInit sync.Once // 确保轮询服务器初始化只执行一次

// init initializes the pollDesc for the given file descriptor.
// 初始化 pollDesc 以用于给定的文件描述符。
func (pd *pollDesc) init(fd *FD) error {
	serverInit.Do(runtime_pollServerInit)             // 确保轮询服务器初始化
	ctx, errno := runtime_pollOpen(uintptr(fd.Sysfd)) // 打开文件描述符进行轮询
	if errno != 0 {
		return errnoErr(syscall.Errno(errno)) // 返回错误
	}
	pd.runtimeCtx = ctx // 设置轮询上下文
	return nil
}

// close closes the pollDesc, releasing any resources.
// 关闭 pollDesc，释放任何资源。
func (pd *pollDesc) close() {
	if pd.runtimeCtx == 0 {
		return // 如果没有轮询上下文，直接返回
	}
	runtime_pollClose(pd.runtimeCtx) // 关闭轮询上下文
	pd.runtimeCtx = 0                // 重置轮询上下文
}

// Evict evicts fd from the pending list, unblocking any I/O running on fd.
// 从待处理列表中驱逐文件描述符，解除对其的任何 I/O 阻塞。
func (pd *pollDesc) evict() {
	if pd.runtimeCtx == 0 {
		return // 如果没有轮询上下文，直接返回
	}
	runtime_pollUnblock(pd.runtimeCtx) // 解除轮询阻塞
}

// prepare prepares the pollDesc for the specified mode.
// 准备 pollDesc 以用于指定的模式。
func (pd *pollDesc) prepare(mode int, isFile bool) error {
	if pd.runtimeCtx == 0 {
		return nil // 如果没有轮询上下文，直接返回
	}
	res := runtime_pollReset(pd.runtimeCtx, mode) // 重置轮询上下文
	return convertErr(res, isFile)                // 转换错误
}

// prepareRead prepares the pollDesc for read operations.
// 准备 pollDesc 以用于读取操作。
func (pd *pollDesc) prepareRead(isFile bool) error {
	return pd.prepare('r', isFile) // 调用 prepare 准备读取
}

// prepareWrite prepares the pollDesc for write operations.
// 准备 pollDesc 以用于写入操作。
func (pd *pollDesc) prepareWrite(isFile bool) error {
	return pd.prepare('w', isFile) // 调用 prepare 准备写入
}

// wait 等待文件描述符就绪
//
// 参数：
//   - mode: 等待模式
//     'r' (读取)
//     'w' (写入)
//   - isFile: 是否为普通文件
//
// 该方法是网络轮询器的核心等待函数
func (pd *pollDesc) wait(mode int, isFile bool) error {
	// 检查轮询上下文是否已初始化
	// runtimeCtx 是在 pd.init() 中通过 runtime_pollOpen 设置的
	if pd.runtimeCtx == 0 {
		return errors.New("waiting for unsupported file type") // 返回不支持的文件类型错误
	}

	// 调用运行时的等待函数
	// - pd.runtimeCtx: 轮询上下文，包含了文件描述符的信息
	// - mode: 等待模式（读/写）
	res := runtime_pollWait(pd.runtimeCtx, mode) // 等待文件描述符就绪

	// 转换错误码为合适的错误类型
	// 根据是否是文件可能有不同的错误处理策略
	return convertErr(res, isFile) // 转换错误
}

// waitRead 等待文件描述符变为可读状态
//
// 参数：
// - isFile: 是否是普通文件（而不是网络连接）
//
// 返回值：
// - error: 等待过程中的错误，nil 表示成功
//
// 该方法通过调用 wait('r', isFile) 来等待读事件：
// - 'r' 表示等待读事件（相对于 'w' 写事件）
// - 如果是网络连接，会使用网络轮询器（如 epoll）
// - 如果是普通文件，可能会使用不同的等待策略
func (pd *pollDesc) waitRead(isFile bool) error {
	return pd.wait('r', isFile) // 等待可读状态
}

// waitWrite 等待文件描述符变为可写状态
//
// 参数：
// - isFile: 是否是普通文件（而不是网络连接）
//
// 返回值：
// - error: 等待过程中的错误，nil 表示成功
func (pd *pollDesc) waitWrite(isFile bool) error {
	return pd.wait('w', isFile) // 等待可写状态
}

// waitCanceled waits for the poll to be canceled.
// 等待轮询被取消。
func (pd *pollDesc) waitCanceled(mode int) {
	if pd.runtimeCtx == 0 {
		return // 如果没有轮询上下文，直接返回
	}
	runtime_pollWaitCanceled(pd.runtimeCtx, mode) // 等待被取消的轮询
}

// pollable reports whether the pollDesc is valid for polling.
// pollable 报告 pollDesc 是否有效以进行轮询。
func (pd *pollDesc) pollable() bool {
	return pd.runtimeCtx != 0 // 如果有轮询上下文，则有效
}

// Error values returned by runtime_pollReset and runtime_pollWait.
// These must match the values in runtime/netpoll.go.
const (
	pollNoError        = 0 // 无错误
	pollErrClosing     = 1 // 关闭错误
	pollErrTimeout     = 2 // 超时错误
	pollErrNotPollable = 3 // 不可轮询错误
)

// convertErr converts the result of a poll operation to an error.
// convertErr 将轮询操作的结果转换为错误。
func convertErr(res int, isFile bool) error {
	switch res {
	case pollNoError:
		return nil // 无错误
	case pollErrClosing:
		return errClosing(isFile) // 关闭错误
	case pollErrTimeout:
		return ErrDeadlineExceeded // 超时错误
	case pollErrNotPollable:
		return ErrNotPollable // 不可轮询错误
	}
	println("unreachable: ", res) // 不可达的错误
	panic("unreachable")          // 触发恐慌
}

// SetDeadline sets the read and write deadlines associated with fd.
// 设置与文件描述符相关的读写截止时间。
func (fd *FD) SetDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r'+'w') // 设置读写截止时间
}

// SetReadDeadline sets the read deadline associated with fd.
// 设置与文件描述符相关的读截止时间。
func (fd *FD) SetReadDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r') // 设置读截止时间
}

// SetWriteDeadline sets the write deadline associated with fd.
// 设置与文件描述符相关的写截止时间。
func (fd *FD) SetWriteDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'w') // 设置写截止时间
}

// setDeadlineImpl sets the deadline for the given mode.
// setDeadlineImpl 为给定模式设置截止时间。
func setDeadlineImpl(fd *FD, t time.Time, mode int) error {
	var d int64
	if !t.IsZero() {
		d = int64(time.Until(t)) // 计算截止时间
		if d == 0 {
			d = -1 // 不要将当前截止时间与无截止时间混淆
		}
	}
	if err := fd.incref(); err != nil {
		return err // 增加引用计数失败
	}
	defer fd.decref() // 确保在函数结束时减少引用计数
	if fd.pd.runtimeCtx == 0 {
		return ErrNoDeadline // 如果没有轮询上下文，返回无截止时间错误
	}
	runtime_pollSetDeadline(fd.pd.runtimeCtx, d, mode) // 设置轮询截止时间
	return nil
}

// IsPollDescriptor reports whether fd is the descriptor being used by the poller.
// IsPollDescriptor 报告文件描述符是否为轮询器使用的描述符。
// This is only used for testing.
// IsPollDescriptor should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/opencontainers/runc
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname IsPollDescriptor
func IsPollDescriptor(fd uintptr) bool {
	return runtime_isPollServerDescriptor(fd) // 检查是否为轮询服务器描述符
}
