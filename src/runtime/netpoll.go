// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || wasip1 || windows

package runtime

import (
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
// func netpollinit()
//     Initialize the poller. Only called once.
//
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd. The pd argument is to pass
//     back to netpollready when fd is ready. Return an errno value.
//
// func netpollclose(fd uintptr) int32
//     Disable notifications for fd. Return an errno value.
//
// func netpoll(delta int64) (gList, int32)
//     Poll the network. If delta < 0, block indefinitely. If delta == 0,
//     poll without blocking. If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready,
//     and a delta to add to netpollWaiters when all goroutines are ready.
//     This will never return an empty list with a non-zero delta.
//
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
//
// func netpollIsPollDescriptor(fd uintptr) bool
//     Reports whether fd is a file descriptor used by the poller.

// Error codes returned by runtime_pollReset and runtime_pollWait.
// These must match the values in internal/poll/fd_poll_runtime.go.
const (
	pollNoError        = 0 // no error
	pollErrClosing     = 1 // descriptor is closed
	pollErrTimeout     = 2 // I/O timeout
	pollErrNotPollable = 3 // general error polling descriptor
)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
//
//	pdReady - io readiness notification is pending;
//	          a goroutine consumes the notification by changing the state to pdNil.
//	pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//	         the goroutine commits to park by changing the state to G pointer,
//	         or, alternatively, concurrent io notification changes the state to pdReady,
//	         or, alternatively, concurrent timeout/close changes the state to pdNil.
//	G pointer - the goroutine is blocked on the semaphore;
//	            io notification or timeout/close changes the state to pdReady or pdNil respectively
//	            and unparks the goroutine.
//	pdNil - none of the above.
const (
	pdNil   uintptr = 0
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// Network poller descriptor.
//
// No heap pointers.
type pollDesc struct {
	_     sys.NotInHeap
	link  *pollDesc      // in pollcache, protected by pollcache.lock
	fd    uintptr        // constant for pollDesc usage lifetime
	fdseq atomic.Uintptr // protects against stale pollDesc

	// atomicInfo holds bits from closing, rd, and wd,
	// which are only ever written while holding the lock,
	// summarized for use by netpollcheckerr,
	// which cannot acquire the lock.
	// After writing these fields under lock in a way that
	// might change the summary, code must call publishInfo
	// before releasing the lock.
	// Code that changes fields and then calls netpollunblock
	// (while still holding the lock) must call publishInfo
	// before calling netpollunblock, because publishInfo is what
	// stops netpollblock from blocking anew
	// (by changing the result of netpollcheckerr).
	// atomicInfo also holds the eventErr bit,
	// recording whether a poll event on the fd got an error;
	// atomicInfo is the only source of truth for that bit.
	atomicInfo atomic.Uint32 // atomic pollInfo

	// rg, wg are accessed atomically and hold g pointers.
	// (Using atomic.Uintptr here is similar to using guintptr elsewhere.)
	rg atomic.Uintptr // pdReady, pdWait, G waiting for read or pdNil
	wg atomic.Uintptr // pdReady, pdWait, G waiting for write or pdNil

	lock    mutex // protects the following fields
	closing bool
	rrun    bool      // whether rt is running
	wrun    bool      // whether wt is running
	user    uint32    // user settable cookie
	rseq    uintptr   // protects from stale read timers
	rt      timer     // read deadline timer
	rd      int64     // read deadline (a nanotime in the future, -1 when expired)
	wseq    uintptr   // protects from stale write timers
	wt      timer     // write deadline timer
	wd      int64     // write deadline (a nanotime in the future, -1 when expired)
	self    *pollDesc // storage for indirect interface. See (*pollDesc).makeArg.
}

// pollInfo is the bits needed by netpollcheckerr, stored atomically,
// mostly duplicating state that is manipulated under lock in pollDesc.
// The one exception is the pollEventErr bit, which is maintained only
// in the pollInfo.
type pollInfo uint32

const (
	pollClosing = 1 << iota
	pollEventErr
	pollExpiredReadDeadline
	pollExpiredWriteDeadline
	pollFDSeq // 20 bit field, low 20 bits of fdseq field
)

const (
	pollFDSeqBits = 20                   // number of bits in pollFDSeq
	pollFDSeqMask = 1<<pollFDSeqBits - 1 // mask for pollFDSeq
)

func (i pollInfo) closing() bool              { return i&pollClosing != 0 }
func (i pollInfo) eventErr() bool             { return i&pollEventErr != 0 }
func (i pollInfo) expiredReadDeadline() bool  { return i&pollExpiredReadDeadline != 0 }
func (i pollInfo) expiredWriteDeadline() bool { return i&pollExpiredWriteDeadline != 0 }

// info returns the pollInfo corresponding to pd.
func (pd *pollDesc) info() pollInfo {
	return pollInfo(pd.atomicInfo.Load())
}

// publishInfo updates pd.atomicInfo (returned by pd.info)
// using the other values in pd.
// It must be called while holding pd.lock,
// and it must be called after changing anything
// that might affect the info bits.
// In practice this means after changing closing
// or changing rd or wd from < 0 to >= 0.
func (pd *pollDesc) publishInfo() {
	var info uint32
	if pd.closing {
		info |= pollClosing
	}
	if pd.rd < 0 {
		info |= pollExpiredReadDeadline
	}
	if pd.wd < 0 {
		info |= pollExpiredWriteDeadline
	}
	info |= uint32(pd.fdseq.Load()&pollFDSeqMask) << pollFDSeq

	// Set all of x except the pollEventErr bit.
	x := pd.atomicInfo.Load()
	for !pd.atomicInfo.CompareAndSwap(x, (x&pollEventErr)|info) {
		x = pd.atomicInfo.Load()
	}
}

// setEventErr sets the result of pd.info().eventErr() to b.
// We only change the error bit if seq == 0 or if seq matches pollFDSeq
// (issue #59545).
func (pd *pollDesc) setEventErr(b bool, seq uintptr) {
	mSeq := uint32(seq & pollFDSeqMask)
	x := pd.atomicInfo.Load()
	xSeq := (x >> pollFDSeq) & pollFDSeqMask
	if seq != 0 && xSeq != mSeq {
		return
	}
	for (x&pollEventErr != 0) != b && !pd.atomicInfo.CompareAndSwap(x, x^pollEventErr) {
		x = pd.atomicInfo.Load()
		xSeq := (x >> pollFDSeq) & pollFDSeqMask
		if seq != 0 && xSeq != mSeq {
			return
		}
	}
}

type pollCache struct {
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInitLock mutex
	netpollInited   atomic.Uint32

	pollcache      pollCache
	netpollWaiters atomic.Uint32
)

// netpollWaiters is accessed in tests
//go:linkname netpollWaiters

//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollGenericInit()
}

// netpollGenericInit 初始化网络轮询器的通用部分
//
// 该函数确保网络轮询器只被初始化一次，使用了双重检查锁定模式（Double-Checked Locking Pattern）
// 来保证线程安全和初始化的唯一性
func netpollGenericInit() {
	// 首次检查：检查是否已经初始化
	// 使用原子操作确保线程安全
	if netpollInited.Load() == 0 {
		// 初始化两个互斥锁
		// - netpollInitLock: 用于保护初始化过程
		// - pollcache.lock: 用于保护轮询缓存
		lockInit(&netpollInitLock, lockRankNetpollInit)
		lockInit(&pollcache.lock, lockRankPollCache)

		// 获取初始化锁
		lock(&netpollInitLock)

		// 二次检查：确保在获取锁的过程中没有其他goroutine完成初始化
		if netpollInited.Load() == 0 {
			// 调用平台特定的初始化函数（如 epoll_create、kqueue 等）
			netpollinit()
			// 标记初始化完成
			netpollInited.Store(1)
		}

		// 释放初始化锁
		unlock(&netpollInitLock)
	}
}

func netpollinited() bool {
	return netpollInited.Load() != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	return netpollIsPollDescriptor(fd)
}

// poll_runtime_pollOpen 为给定的文件描述符创建并初始化一个轮询描述符
//
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	// 从轮询缓存中分配一个轮询描述符
	pd := pollcache.alloc()

	// 加锁保护轮询描述符的初始化
	lock(&pd.lock)

	// 检查写等待组的状态，确保它是空闲的或就绪的
	wg := pd.wg.Load()
	if wg != pdNil && wg != pdReady {
		throw("runtime: blocked write on free polldesc")
	}

	// 检查读等待组的状态，确保它是空闲的或就绪的
	rg := pd.rg.Load()
	if rg != pdNil && rg != pdReady {
		throw("runtime: blocked read on free polldesc")
	}

	// 初始化轮询描述符的各个字段
	pd.fd = fd // 设置文件描述符
	if pd.fdseq.Load() == 0 {
		// 0 值在 setEventErr 中有特殊含义，避免使用它
		pd.fdseq.Store(1)
	}
	pd.closing = false       // 设置为非关闭状态
	pd.setEventErr(false, 0) // 清除事件错误标志
	pd.rseq++                // 增加读序列号
	pd.rg.Store(pdNil)       // 重置读等待组
	pd.rd = 0                // 清除读截止时间
	pd.wseq++                // 增加写序列号
	pd.wg.Store(pdNil)       // 重置写等待组
	pd.wd = 0                // 清除写截止时间
	pd.self = pd             // 设置自引用
	pd.publishInfo()         // 发布描述符信息
	unlock(&pd.lock)         // 释放锁

	// 将文件描述符注册到操作系统的轮询系统（如 epoll）
	errno := netpollopen(fd, pd)
	if errno != 0 {
		// 如果注册失败，释放轮询描述符并返回错误
		pollcache.free(pd)
		return nil, int(errno)
	}

	return pd, 0
}

//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}
	wg := pd.wg.Load()
	if wg != pdNil && wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	rg := pd.rg.Load()
	if rg != pdNil && rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}
	netpollclose(pd.fd)
	pollcache.free(pd)
}

func (c *pollCache) free(pd *pollDesc) {
	// pd can't be shared here, but lock anyhow because
	// that's what publishInfo documents.
	lock(&pd.lock)

	// Increment the fdseq field, so that any currently
	// running netpoll calls will not mark pd as ready.
	fdseq := pd.fdseq.Load()
	fdseq = (fdseq + 1) & (1<<taggedPointerBits - 1)
	pd.fdseq.Store(fdseq)

	pd.publishInfo()

	unlock(&pd.lock)

	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

// poll_runtime_pollReset, which is internal/poll.runtime_pollReset,
// prepares a descriptor for polling in mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	if mode == 'r' {
		pd.rg.Store(pdNil)
	} else if mode == 'w' {
		pd.wg.Store(pdNil)
	}
	return pollNoError
}

// poll_runtime_pollWait, which is internal/poll.runtime_pollWait,
// waits for a descriptor to be ready for reading or writing,
// according to mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
// poll_runtime_pollWait 等待文件描述符准备好进行读取或写入
// 参数：
// - pd: 轮询描述符
// - mode: 模式，'r'表示读，'w'表示写
// 返回值：错误码，定义见上方
//
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	// 首先检查是否有错误发生
	// 例如：文件描述符已关闭、超时等
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}

	// 某些操作系统使用水平触发的 I/O
	// 需要重新注册（arm）事件
	if GOOS == "solaris" || GOOS == "illumos" || GOOS == "aix" || GOOS == "wasip1" {
		netpollarm(pd, mode)
	}

	// 循环尝试阻塞等待
	// netpollblock 返回 true 表示事件已就绪
	// 返回 false 表示需要重试（可能是虚假唤醒）
	for !netpollblock(pd, int32(mode), false) {
		// 每次重试前都要检查错误
		errcode = netpollcheckerr(pd, int32(mode))
		if errcode != pollNoError {
			return errcode
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
		//
		// 可能发生的情况：
		// 1. 超时触发并唤醒了我们
		// 2. 但在我们运行之前，超时被重置了
		// 这种情况下我们假装什么都没发生，继续重试
	}

	return pollNoError
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	for !netpollblock(pd, int32(mode), true) {
	}
}

//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	if d > 0 {
		d += nanotime()
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			d = 1<<63 - 1
		}
	}
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}
	pd.publishInfo()
	combo := pd.rd > 0 && pd.rd == pd.wd
	rtf := netpollReadDeadline
	if combo {
		rtf = netpollDeadline
	}
	if !pd.rrun {
		if pd.rd > 0 {
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.modify(pd.rd, 0, rtf, pd.makeArg(), pd.rseq)
			pd.rrun = true
		}
	} else if pd.rd != rd0 || combo != combo0 {
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			pd.rt.modify(pd.rd, 0, rtf, pd.makeArg(), pd.rseq)
		} else {
			pd.rt.stop()
			pd.rrun = false
		}
	}
	if !pd.wrun {
		if pd.wd > 0 && !combo {
			pd.wt.modify(pd.wd, 0, netpollWriteDeadline, pd.makeArg(), pd.wseq)
			pd.wrun = true
		}
	} else if pd.wd != wd0 || combo != combo0 {
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			pd.wt.modify(pd.wd, 0, netpollWriteDeadline, pd.makeArg(), pd.wseq)
		} else {
			pd.wt.stop()
			pd.wrun = false
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	// Note that pd.publishInfo has already been called, above, immediately after modifying rd and wd.
	delta := int32(0)
	var rg, wg *g
	if pd.rd < 0 {
		rg = netpollunblock(pd, 'r', false, &delta)
	}
	if pd.wd < 0 {
		wg = netpollunblock(pd, 'w', false, &delta)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
	netpollAdjustWaiters(delta)
}

//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	pd.publishInfo()
	delta := int32(0)
	rg = netpollunblock(pd, 'r', false, &delta)
	wg = netpollunblock(pd, 'w', false, &delta)
	if pd.rrun {
		pd.rt.stop()
		pd.rrun = false
	}
	if pd.wrun {
		pd.wt.stop()
		pd.wrun = false
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
	netpollAdjustWaiters(delta)
}

// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
//
// This returns a delta to apply to netpollWaiters.
//
// This may run while the world is stopped, so write barriers are not allowed.
//
// netpollready 由平台特定的 netpoll 函数调用
// 用于通知与 pd 关联的文件描述符已经准备好进行 I/O 操作
//
//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) int32 {
	// delta 用于跟踪等待者数量的变化
	delta := int32(0)

	// rg: 等待读取的 goroutine
	// wg: 等待写入的 goroutine
	var rg, wg *g

	// 处理读就绪事件
	// mode 可能是 'r' 或 'r'+'w'
	if mode == 'r' || mode == 'r'+'w' {
		// 解除读等待的 goroutine 阻塞
		rg = netpollunblock(pd, 'r', true, &delta)
	}

	// 处理写就绪事件
	// mode 可能是 'w' 或 'r'+'w'
	if mode == 'w' || mode == 'r'+'w' {
		// 解除写等待的 goroutine 阻塞
		wg = netpollunblock(pd, 'w', true, &delta)
	}

	// 将解除阻塞的 goroutine 添加到待运行列表
	if rg != nil {
		toRun.push(rg) // 添加读 goroutine
	}
	if wg != nil {
		toRun.push(wg) // 添加写 goroutine
	}

	// 返回等待者数量的变化
	return delta
}

func netpollcheckerr(pd *pollDesc, mode int32) int {
	info := pd.info()
	if info.closing() {
		return pollErrClosing
	}
	if (mode == 'r' && info.expiredReadDeadline()) || (mode == 'w' && info.expiredWriteDeadline()) {
		return pollErrTimeout
	}
	// Report an event scanning error only on a read event.
	// An error on a write event will be captured in a subsequent
	// write call that is able to report a more specific error.
	if mode == 'r' && info.eventErr() {
		return pollErrNotPollable
	}
	return pollNoError
}

func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		netpollAdjustWaiters(1)
	}
	return r
}

func netpollgoready(gp *g, traceskip int) {
	goready(gp, traceskip+1)
}

// returns true if IO is ready, or false if timed out or closed
// waitio - wait only for completed IO, ignore errors
// Concurrent calls to netpollblock in the same mode are forbidden, as pollDesc
// can hold only a single waiting goroutine for each mode.
//
// netpollblock 阻塞当前 goroutine 直到 I/O 就绪或超时/关闭
// - 如果 I/O 就绪返回 true
// - 如果超时或关闭返回 false
// - waitio 参数表示是否只等待 I/O 完成而忽略错误
// - 同一模式下禁止并发调用，因为每个模式只能有一个等待的 goroutine
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	// 选择读或写等待组
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// 设置等待组状态为 pdWait
	for {
		// Consume notification if already ready.
		if gpp.CompareAndSwap(pdReady, pdNil) {
			return true
		}
		if gpp.CompareAndSwap(pdNil, pdWait) {
			break
		}

		// Double check that this isn't corrupt; otherwise we'd loop
		// forever.
		if v := gpp.Load(); v != pdReady && v != pdNil {
			throw("runtime: double wait")
		}
	}

	// need to recheck error states after setting gpp to pdWait
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, publishInfo, load of rg/wg
	if waitio || netpollcheckerr(pd, mode) == pollNoError {
		// 将当前 goroutine 置于等待状态
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceBlockNet, 5)
	}

	// 恢复为空闲状态，并检查是否有就绪通知
	old := gpp.Swap(pdNil)
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	return old == pdReady
}

// netpollunblock moves either pd.rg (if mode == 'r') or
// pd.wg (if mode == 'w') into the pdReady state.
// This returns any goroutine blocked on pd.{rg,wg}.
// It adds any adjustment to netpollWaiters to *delta;
// this adjustment should be applied after the goroutine has
// been marked ready.
//
// netpollunblock 将 pd.rg（读）或 pd.wg（写）转换为就绪状态
// 返回在 pd.rg 或 pd.wg 上阻塞的 goroutine
func netpollunblock(pd *pollDesc, mode int32, ioready bool, delta *int32) *g {
	// 根据模式选择读或写等待组指针
	gpp := &pd.rg // 默认读等待组
	if mode == 'w' {
		gpp = &pd.wg // 写等待组
	}

	// 循环尝试更新等待组状态
	for {
		// 获取当前状态
		old := gpp.Load()

		// 如果已经是就绪状态，无需处理
		if old == pdReady {
			return nil
		}

		// 如果没有等待的 goroutine 且不是 I/O 就绪
		// 则不设置就绪状态
		if old == pdNil && !ioready {
			// Only set pdReady for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}

		// 确定新状态
		new := pdNil // 默认设为空闲
		if ioready {
			new = pdReady // I/O 就绪时设为就绪
		}

		// 原子地更新状态
		if gpp.CompareAndSwap(old, new) {
			if old == pdWait {
				// 如果之前是等待状态，转换为空闲状态
				old = pdNil
			} else if old != pdNil {
				// 如果之前不是空闲状态，减少等待计数
				*delta -= 1
			}
			// 返回被解除阻塞的 goroutine
			return (*g)(unsafe.Pointer(old))
		}
		// CAS 失败则重试
	}
}

func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	currentSeq := pd.rseq
	if !read {
		currentSeq = pd.wseq
	}
	if seq != currentSeq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	delta := int32(0)
	var rg *g
	if read {
		if pd.rd <= 0 || !pd.rrun {
			throw("runtime: inconsistent read deadline")
		}
		pd.rd = -1
		pd.publishInfo()
		rg = netpollunblock(pd, 'r', false, &delta)
	}
	var wg *g
	if write {
		if pd.wd <= 0 || !pd.wrun && !read {
			throw("runtime: inconsistent write deadline")
		}
		pd.wd = -1
		pd.publishInfo()
		wg = netpollunblock(pd, 'w', false, &delta)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 0)
	}
	if wg != nil {
		netpollgoready(wg, 0)
	}
	netpollAdjustWaiters(delta)
}

func netpollDeadline(arg any, seq uintptr, delta int64) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

func netpollReadDeadline(arg any, seq uintptr, delta int64) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

func netpollWriteDeadline(arg any, seq uintptr, delta int64) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

// netpollAnyWaiters reports whether any goroutines are waiting for I/O.
func netpollAnyWaiters() bool {
	return netpollWaiters.Load() > 0
}

// netpollAdjustWaiters adds delta to netpollWaiters.
func netpollAdjustWaiters(delta int32) {
	if delta != 0 {
		netpollWaiters.Add(delta)
	}
}

func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	if c.first == nil {
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys)
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			lockInit(&pd.lock, lockRankPollDesc)
			pd.rt.init(nil, nil)
			pd.wt.init(nil, nil)
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link
	unlock(&c.lock)
	return pd
}

// makeArg converts pd to an interface{}.
// makeArg does not do any allocation. Normally, such
// a conversion requires an allocation because pointers to
// types which embed runtime/internal/sys.NotInHeap (which pollDesc is)
// must be stored in interfaces indirectly. See issue 42076.
func (pd *pollDesc) makeArg() (i any) {
	x := (*eface)(unsafe.Pointer(&i))
	x._type = pdType
	x.data = unsafe.Pointer(&pd.self)
	return
}

var (
	pdEface any    = (*pollDesc)(nil)
	pdType  *_type = efaceOf(&pdEface)._type
)
