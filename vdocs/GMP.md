# Go GMP 调度器模型

## 概述

GMP是Go语言运行时调度器的核心模型，由Goroutine（G）、Machine（M）、Processor（P）三个主要组件构成。这个模型实现了高效的用户级线程调度，支持数百万goroutine的并发执行，同时保持良好的CPU利用率和负载均衡。

## 核心概念

### G (Goroutine)
Go协程，是Go语言中的轻量级线程，代表一个执行任务的实体。

### M (Machine) 
操作系统线程的抽象，负责执行goroutine。一个M在某一时刻只能与一个P绑定。

### P (Processor)
处理器，代表执行Go代码所需的资源（如本地运行队列、内存分配器等）。P的数量决定了Go程序的并行度，通常等于CPU核心数。

## 数据结构详解

### 1. Goroutine (g) 结构

```go
// src/runtime/runtime2.go
type g struct {
    // goroutine栈信息
    stack       stack   // 栈的描述符
    stackguard0 uintptr // 栈保护，用于栈溢出检测
    stackguard1 uintptr // C栈的栈保护
    
    // 调度相关
    m              *m          // 当前正在执行该g的m
    sched          gobuf       // 调度时的寄存器状态
    syscallsp      uintptr     // 系统调用时的栈指针
    syscallpc      uintptr     // 系统调用时的程序计数器
    param          unsafe.Pointer // wakeup时传递的参数
    atomicstatus   uint32      // 原子状态
    goid           int64       // goroutine id
    
    // 抢占相关
    preempt       bool  // 抢占信号
    preemptStop   bool  // 抢占停止
    preemptShrink bool  // 栈收缩
    
    // 同步相关
    waiting       *sudog // 如果g在等待队列中，这个指向等待记录
    
    // 调试和跟踪
    gcscandone    bool   // GC扫描完成标记
    throwsplit    bool   // 不允许栈分裂
    lockedm       muintptr // g锁定到的m
}

// goroutine状态
const (
    _Gidle = iota       // 刚分配，未初始化
    _Grunnable         // 可运行，在运行队列中
    _Grunning          // 正在运行
    _Gsyscall          // 系统调用中
    _Gwaiting          // 等待中（如等待channel）
    _Gdead             // 已死亡
    _Gcopystack        // 栈正在复制
    _Gpreempted        // 被抢占
    _Gscan            // GC正在扫描栈
)
```

### 2. Machine (m) 结构

```go
type m struct {
    g0      *g          // 带有调度栈的goroutine
    curg    *g          // 当前正在执行的用户goroutine
    p       puintptr    // 关联的P（P在执行Go代码时）
    nextp   puintptr    // 暂存的P（当m正在park时）
    oldp    puintptr    // 执行系统调用前绑定的P
    
    // 系统调用相关
    mstartfn      func()    // m启动函数
    id            int64     // m的id
    mallocing     int32     // 正在分配内存的状态
    throwing      int32     // 正在抛异常
    preemptoff    string    // 禁用抢占的原因
    locks         int32     // 锁计数
    dying         int32     // m正在死亡
    helpgc        int32     // 帮助GC
    spinning      bool      // m正在寻找work
    blocked       bool      // m阻塞在note上
    newSigstack   bool      // C线程上的minit使用了信号栈
    printlock     int8      // 打印锁
    incgo         bool      // m正在执行cgo调用
    freeWait      uint32    // 如果 == UINT32_MAX，是 freeMWait 列表上的 m
    fastrand      uint64    // 随机数生成器状态
    needextram    bool      // 需要额外的M
    traceback     uint8     // 追踪状态
    ncgocall      uint64    // cgo调用数量
    
    // Per-M缓存
    mcache    *mcache      // 当前m的内存分配器缓存
    lockedg   guintptr     // 锁定的goroutine
    createstack [32]uintptr // 创建该m的stack trace
    lockedInt  uint32      // 内部锁定状态
    lockedExt  uint32      // 外部锁定状态
    
    // 系统监控
    mOS        // 系统相关字段
}
```

### 3. Processor (p) 结构

```go
type p struct {
    id          int32       // P的id
    status      uint32      // P的状态
    link        puintptr    // 下一个P（空闲链表中）
    m           muintptr    // 反向链接到关联的M
    mcache      *mcache     // 内存分配缓存
    pcache      pageCache   // 页缓存
    
    // 可运行的goroutine队列（本地队列）
    runqhead uint32         // 队列头
    runqtail uint32         // 队列尾
    runq     [256]guintptr  // 本地运行队列，大小固定为256
    runnext  guintptr       // 下一个要运行的g，优先级最高
    
    // 空闲的goroutine列表
    gFree struct {
        gList
        n int32  // 空闲g的数量
    }
    
    sudogcache []*sudog     // sudog缓存
    sudogbuf   [128]*sudog  // sudog缓冲区
    
    // GC相关
    gcAssistTime         int64    // GC辅助时间
    gcFractionalMarkTime int64    // GC分数标记时间
    gcBgMarkWorker       guintptr // 后台标记工作者
    gcMarkWorkerMode     gcMarkWorkerMode
    
    // 垃圾收集器状态
    gcMarkWorkerStartTime int64
    gcw                   gcWork // GC工作缓冲区
    
    // 抢占相关
    preempt bool           // 抢占标志
    
    // timers
    timers     []*timer    // 计时器堆
    numTimers  uint32      // 计时器数量
    deletedTimers uint32   // 已删除的计时器数量
    
    // 调试用
    selectDone uint32      // select语句完成标志
    
    // pallocCache是每个P的页分配器缓存
    pallocCache
    
    // 调度器相关
    schedtick   uint32     // 每次调度递增
    syscalltick uint32     // 每次系统调用递增
    sysmontick  sysmontick // sysmon监控计数
}

// P状态
const (
    _Pidle    = iota  // 空闲
    _Prunning         // 运行中
    _Psyscall         // 系统调用中
    _Pgcstop          // GC停止
    _Pdead            // 已死亡
)
```

## 调度原理

### 1. 调度器初始化

```go
// 调度器初始化
func schedinit() {
    // 初始化m0（主线程）
    _g_ := getg()
    _g_.m.g0 = _g_
    _g_.m.g0.m = _g_.m
    
    // 初始化调度器数据结构
    sched.maxmcount = 10000  // 最大M数量
    
    // 设置P的数量（通常等于CPU核心数）
    procs := ncpu
    if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
        procs = n
    }
    
    // 调整P的数量
    if procresize(procs) != nil {
        throw("unknown runnable goroutine during bootstrap")
    }
}

// 调整P的数量
func procresize(nprocs int32) *p {
    old := gomaxprocs
    if old < 0 || nprocs <= 0 {
        throw("procresize: invalid arg")
    }
    
    // 更新全局变量
    if atomic.Load(&sched.procresizetime) != 0 {
        atomic.Store(&sched.procresizetime, nanotime())
    }
    
    // 分配P数组
    if nprocs > int32(len(allp)) {
        // 如果需要更多P，分配新的切片
        lock(&allpLock)
        if nprocs <= int32(cap(allp)) {
            allp = allp[:nprocs]
        } else {
            nallp := make([]*p, nprocs)
            copy(nallp, allp[:cap(allp)])
            allp = nallp
        }
        unlock(&allpLock)
    }
    
    // 初始化新P
    for i := old; i < nprocs; i++ {
        pp := allp[i]
        if pp == nil {
            pp = new(p)
        }
        pp.init(i)
        atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
    }
    
    return allp[0]
}
```

### 2. 核心调度函数

```go
// 调度的核心函数：寻找可运行的goroutine
func findrunnable() (gp *g, inheritTime bool, tryWakeP bool) {
top:
    _p_ := _g_.m.p.ptr()
    
    // 检查本地队列
    if gp, inheritTime := runqget(_p_); gp != nil {
        return gp, inheritTime, false
    }
    
    // 检查全局队列
    if sched.runqsize != 0 {
        lock(&sched.lock)
        gp := globrunqget(_p_, 0)
        unlock(&sched.lock)
        if gp != nil {
            return gp, false, false
        }
    }
    
    // 检查网络轮询器
    if netpollinited() && netpollWaiters.Load() > 0 {
        if list, delta := netpoll(0); !list.empty() {
            gp := list.pop()
            injectglist(&list)
            netpollAdjustWaiters(delta)
            casgstatus(gp, _Gwaiting, _Grunnable)
            return gp, false, false
        }
    }
    
    // 工作窃取：从其他P的本地队列偷取
    for i := 0; i < 4; i++ {
        _p_ := _g_.m.p.ptr()
        for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
            if sched.gcwaiting != 0 {
                goto top
            }
            stealRunNextG := i > 2  // 前两轮不偷取runnext
            p2 := allp[enum.position()]
            if _p_ == p2 {
                continue
            }
            if gp := runqsteal(_p_, p2, stealRunNextG); gp != nil {
                return gp, false, false
            }
        }
    }
    
    // 没有找到工作，准备park
    return nil, false, false
}

// 从P的本地队列获取goroutine
func runqget(_p_ *p) (gp *g, inheritTime bool) {
    // 优先从runnext获取
    next := _p_.runnext
    if next != 0 && _p_.runnext.cas(next, 0) {
        return next.ptr(), true
    }
    
    // 从本地队列获取
    for {
        h := atomic.Load(&_p_.runqhead)
        t := _p_.runqtail
        if t == h {
            return nil, false
        }
        gp := _p_.runq[h%uint32(len(_p_.runq))].ptr()
        if atomic.Cas(&_p_.runqhead, h, h+1) {
            return gp, false
        }
    }
}

// 工作窃取算法
func runqsteal(_p_, p2 *p, stealRunNext bool) *g {
    t := _p_.runqtail
    n := t - _p_.runqhead
    n = n - n/2  // 只偷取一半
    if n == 0 {
        return nil
    }
    
    if n > uint32(len(_p_.runq))/2 {
        n = uint32(len(_p_.runq)) / 2
    }
    
    h := atomic.Load(&p2.runqhead)
    if t := p2.runqtail; t-h < n {
        n = t - h
    }
    
    if n == 0 {
        return nil
    }
    
    // 执行窃取
    batch := make([]*g, n)
    for i := uint32(0); i < n; i++ {
        gp := p2.runq[(h+i)%uint32(len(p2.runq))].ptr()
        batch[i] = gp
    }
    
    if !atomic.Cas(&p2.runqhead, h, h+n) {
        return nil
    }
    
    // 将偷来的goroutine加入本地队列
    for i := uint32(1); i < n; i++ {
        runqput(_p_, batch[i], false)
    }
    
    return batch[0]
}
```

### 3. Goroutine切换

```go
// goroutine调度切换
func schedule() {
    _g_ := getg()
    
    if _g_.m.locks != 0 {
        throw("schedule: holding locks")
    }
    
    if _g_.m.lockedg != 0 {
        stoplockedm()
        execute(_g_.m.lockedg.ptr(), false)
    }
    
top:
    pp := _g_.m.p.ptr()
    pp.preempt = false
    
    // 检查GC
    if sched.gcwaiting != 0 {
        gcstopm()
        goto top
    }
    
    // 安全点检查
    if pp.runSafePointFn != 0 {
        runSafePointFn()
    }
    
    // 寻找可运行的goroutine
    var gp *g
    var inheritTime bool
    
    if gp, inheritTime = findrunnable(); gp == nil {
        // 没有找到，进入休眠
        stopm()
        goto top
    }
    
    // 执行goroutine
    execute(gp, inheritTime)
}

// 执行goroutine
func execute(gp *g, inheritTime bool) {
    _g_ := getg()
    
    // 将gp赋值给m的curg
    _g_.m.curg = gp
    gp.m = _g_.m
    
    // 设置goroutine状态为运行中
    casgstatus(gp, _Grunnable, _Grunning)
    gp.waitsince = 0
    gp.preempt = false
    gp.stackguard0 = gp.stack.lo + _StackGuard
    
    if !inheritTime {
        _g_.m.p.ptr().schedtick++
    }
    
    // 执行goroutine
    gogo(&gp.sched)
}
```

## 抢占调度

### 1. 协作式抢占

```go
// 检查抢占
func preemptone(_p_ *p) bool {
    mp := _p_.m.ptr()
    if mp == nil || mp == getg().m {
        return false
    }
    
    gp := mp.curg
    if gp == nil || gp == mp.g0 {
        return false
    }
    
    // 设置抢占标志
    gp.preempt = true
    gp.stackguard0 = stackPreempt
    
    // 发送抢占信号
    preemptM(mp)
    
    return true
}

// 抢占检查点
func stackcheck() {
    gp := getg()
    if gp.stackguard0 == stackPreempt {
        if gp.preemptShrink {
            // 栈收缩
            shrinkstack(gp)
            gp.preemptShrink = false
        } else if gp.preemptStop {
            // 停止执行
            preemptPark(gp)
        } else {
            // 让出执行权
            gopreempt_m(gp)
        }
    }
}
```

### 2. 信号式抢占

```go
// 信号抢占处理
func doSigPreempt(gp *g, ctxt *sigctxt) {
    // 检查是否可以安全抢占
    if wantAsyncPreempt(gp) && isAsyncSafePoint(gp, ctxt.sigpc(), ctxt.sigsp(), ctxt.siglr()) {
        // 异步抢占
        asyncPreempt(ctxt)
    }
}

// 异步抢占
func asyncPreempt(ctxt *sigctxt) {
    gp := getg()
    
    // 保存当前上下文
    gp.asyncSafePoint = true
    save := gp.sched
    
    // 切换到g0栈
    mcall(asyncPreempt2)
    
    // 恢复上下文
    gp.asyncSafePoint = false
    gp.sched = save
}
```

## 系统调用处理

### 1. 系统调用进入

```go
// 系统调用进入
func entersyscall() {
    _g_ := getg()
    
    // 禁用抢占
    _g_.m.locks++
    
    // 保存用户g的状态
    save(getg().sched.pc, getg().sched.sp)
    _g_.syscallsp = _g_.sched.sp
    _g_.syscallpc = _g_.sched.pc
    
    // 设置状态为系统调用
    casgstatus(_g_, _Grunning, _Gsyscall)
    
    // 释放P
    if atomic.Load(&sched.sysmonwait) != 0 {
        systemstack(entersyscall_sysmon)
    } else {
        save(_g_.sched.pc, _g_.sched.sp)
    }
    
    _g_.m.syscalltick++
    _g_.m.locks--
}

// 系统调用退出
func exitsyscall() {
    _g_ := getg()
    
    _g_.m.locks++
    
    if exitsyscallfast(_g_) {
        // 快速路径：重新获取到P
        _g_.m.p.ptr().syscalltick++
        casgstatus(_g_, _Gsyscall, _Grunning)
    } else {
        // 慢速路径：需要重新调度
        mcall(exitsyscall0)
        // 在这里_g已经不再运行，需要重新调度
    }
    
    _g_.m.locks--
}

// 系统调用慢速路径
func exitsyscall0(gp *g) {
    casgstatus(gp, _Gsyscall, _Grunnable)
    
    // 尝试获取P
    _p_ := pidleget()
    if _p_ != nil {
        // 获取到P，可以继续执行
        acquirep(_p_)
        execute(gp, false)
    }
    
    // 没有获取到P，放入全局队列
    lock(&sched.lock)
    globrunqput(gp)
    unlock(&sched.lock)
    
    // park当前M
    stopm()
}
```

## 网络轮询器集成

### 1. 网络事件处理

```go
// 网络轮询
func netpoll(delay int64) (gList, int32) {
    if epfd == -1 {
        return gList{}, 0
    }
    
    var waitms int32
    if delay < 0 {
        waitms = -1
    } else if delay == 0 {
        waitms = 0
    } else {
        waitms = int32(delay / 1e6)
    }
    
    // epoll_wait
    var events [128]epollevent
retry:
    n := epollwait(epfd, events[:], int32(len(events)), waitms)
    if n < 0 {
        if n != -_EINTR {
            println("runtime: epollwait on fd", epfd, "failed with", -n)
            throw("runtime: netpoll failed")
        }
        if waitms > 0 {
            return gList{}, 0
        }
        goto retry
    }
    
    var toRun gList
    for i := int32(0); i < n; i++ {
        ev := &events[i]
        
        var mode int32
        if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
            mode += 'r'
        }
        if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
            mode += 'w'
        }
        
        if mode != 0 {
            pd := (*pollDesc)(unsafe.Pointer(ev.data))
            netpollready(&toRun, pd, mode)
        }
    }
    
    return toRun, int32(len(toRun))
}

// 网络事件就绪
func netpollready(toRun *gList, pd *pollDesc, mode int32) {
    var rg, wg *g
    
    if mode == 'r' || mode == 'r'+'w' {
        rg = netpollunblock(pd, 'r', true)
    }
    if mode == 'w' || mode == 'r'+'w' {
        wg = netpollunblock(pd, 'w', true)
    }
    
    if rg != nil {
        toRun.push(rg)
    }
    if wg != nil {
        toRun.push(wg)
    }
}
```

## 性能优化

### 1. 本地队列优化

```go
// 高效的本地队列操作
func runqput(_p_ *p, gp *g, next bool) {
    if randomizeScheduler && next && fastrand()%2 == 0 {
        next = false
    }
    
    if next {
    retryNext:
        oldnext := _p_.runnext
        if !_p_.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
            goto retryNext
        }
        if oldnext == 0 {
            return
        }
        gp = oldnext.ptr()
    }
    
retry:
    h := atomic.Load(&_p_.runqhead)
    t := _p_.runqtail
    if t-h < uint32(len(_p_.runq)) {
        _p_.runq[t%uint32(len(_p_.runq))].set(gp)
        atomic.Store(&_p_.runqtail, t+1)
        return
    }
    
    // 本地队列满，放入全局队列
    if runqputslow(_p_, gp, h, t) {
        return
    }
    goto retry
}

// 本地队列满时的慢速处理
func runqputslow(_p_ *p, gp *g, h, t uint32) bool {
    var batch [len(_p_.runq)/2 + 1]*g
    
    // 将本地队列一半的goroutine取出
    n := t - h
    n = n / 2
    if n != uint32(len(_p_.runq)/2) {
        throw("runqputslow: queue is not full")
    }
    
    for i := uint32(0); i < n; i++ {
        batch[i] = _p_.runq[(h+i)%uint32(len(_p_.runq))].ptr()
    }
    
    if !atomic.Cas(&_p_.runqhead, h, h+n) {
        return false
    }
    
    batch[n] = gp
    
    // 将这些goroutine放入全局队列
    lock(&sched.lock)
    globrunqputbatch(&batch[0], int32(n+1))
    unlock(&sched.lock)
    
    return true
}
```

### 2. 系统监控

```go
// 系统监控goroutine
func sysmon() {
    lock(&sched.lock)
    sched.nmsys++
    checkdead()
    unlock(&sched.lock)
    
    lasttrace := int64(0)
    idle := 0
    delay := uint32(0)
    
    for {
        if idle == 0 {
            delay = 20
        } else if idle > 50 {
            delay *= 2
        }
        if delay > 10*1000 {
            delay = 10 * 1000
        }
        
        usleep(delay)
        
        now := nanotime()
        
        // 抢占长时间运行的goroutine
        if retake(now) != 0 {
            idle = 0
        } else {
            idle++
        }
        
        // 强制GC
        if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && atomic.Load(&forcegc.idle) != 0 {
            lock(&forcegc.lock)
            forcegc.idle = 0
            var list gList
            list.push(forcegc.g)
            injectglist(&list)
            unlock(&forcegc.lock)
        }
    }
}

// 抢占检查
func retake(now int64) uint32 {
    n := 0
    lock(&allpLock)
    for i := 0; i < len(allp); i++ {
        _p_ := allp[i]
        if _p_ == nil {
            continue
        }
        
        pd := &_p_.sysmontick
        s := _p_.status
        sysretake := false
        
        if s == _Prunning || s == _Psyscall {
            // 检查运行时间
            t := int64(_p_.schedtick)
            if int64(pd.schedtick) != t {
                pd.schedtick = uint32(t)
                pd.schedwhen = now
            } else if pd.schedwhen+forcePreemptNS <= now {
                // goroutine运行时间过长，抢占
                preemptone(_p_)
                sysretake = true
            }
        }
        
        if s == _Psyscall {
            // 系统调用时间过长，释放P
            if runqempty(_p_) && atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) > 0 && pd.syscallwhen+10*1000*1000 > now {
                continue
            }
            
            unlock(&allpLock)
            if atomic.Cas(&_p_.status, s, _Pidle) {
                n++
                handoffp(_p_)
            }
            lock(&allpLock)
        }
    }
    unlock(&allpLock)
    
    return uint32(n)
}
```

## 调试和监控

### 1. 调度器统计

```go
// 调度器统计信息
func schedtrace(detailed bool) {
    now := nanotime()
    id1, id2, id3 := 0, 0, 0
    if detailed {
        id1 = getg().m.id
        id2 = _p_.id  
        id3 = _p_.m.ptr().id
    }
    
    lock(&sched.lock)
    print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs,
        " idleprocs=", sched.npidle, " threads=", mcount(),
        " spinningthreads=", sched.nmspinning, " idlethreads=", sched.nmidle,
        " runqueue=", sched.runqsize)
        
    if detailed {
        print(" gcwaiting=", sched.gcwaiting, " nmidlelocked=", sched.nmidlelocked,
            " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait)
    }
    unlock(&sched.lock)
    
    // 打印每个P的状态
    for i, _p_ := range allp {
        mp := _p_.m.ptr()
        h := atomic.Load(&_p_.runqhead)
        t := _p_.runqtail
        print(" P", i, ": status=", _p_.status, " schedtick=", _p_.schedtick,
            " syscalltick=", _p_.syscalltick, " m=")
        if mp != nil {
            print(mp.id)
        } else {
            print("nil")
        }
        print(" runqsize=", t-h, " gfreecnt=", _p_.gFree.n, "\n")
    }
}
```

### 2. Goroutine泄漏检测

```go
// 检测goroutine泄漏
func checkGoroutineLeak() {
    var buf [64 << 10]byte
    buf = buf[:runtime.Stack(buf[:], true)]
    
    // 分析stack trace
    lines := strings.Split(string(buf), "\n")
    goroutines := make(map[string]int)
    
    for i, line := range lines {
        if strings.HasPrefix(line, "goroutine ") {
            if i+1 < len(lines) {
                fn := lines[i+1]
                goroutines[fn]++
            }
        }
    }
    
    // 输出统计信息
    for fn, count := range goroutines {
        if count > 1000 {
            fmt.Printf("潜在泄漏：%s 有 %d 个goroutine\n", fn, count)
        }
    }
}
```

## 最佳实践

### 1. Goroutine池

```go
// 工作者池模式
type WorkerPool struct {
    tasks   chan Task
    workers int
    wg      sync.WaitGroup
}

func NewWorkerPool(workers int, bufferSize int) *WorkerPool {
    return &WorkerPool{
        tasks:   make(chan Task, bufferSize),
        workers: workers,
    }
}

func (p *WorkerPool) Start() {
    for i := 0; i < p.workers; i++ {
        p.wg.Add(1)
        go p.worker()
    }
}

func (p *WorkerPool) worker() {
    defer p.wg.Done()
    for task := range p.tasks {
        task.Process()
    }
}

func (p *WorkerPool) Submit(task Task) {
    p.tasks <- task
}

func (p *WorkerPool) Stop() {
    close(p.tasks)
    p.wg.Wait()
}
```

### 2. GOMAXPROCS调优

```go
import (
    "runtime"
    "go.uber.org/automaxprocs/maxprocs"
)

func init() {
    // 在容器环境中自动设置GOMAXPROCS
    maxprocs.Set(maxprocs.Logger(log.Printf))
    
    // 手动设置GOMAXPROCS
    if containerCPU := os.Getenv("CONTAINER_CPU"); containerCPU != "" {
        if cpu, err := strconv.Atoi(containerCPU); err == nil {
            runtime.GOMAXPROCS(cpu)
        }
    }
}
```

## 总结

GMP调度器是Go语言高并发能力的核心，通过以下特性实现了高效的goroutine调度：

1. **多级队列**: 本地队列 + 全局队列 + 网络轮询器
2. **工作窃取**: 负载均衡机制，提高CPU利用率  
3. **抢占调度**: 防止goroutine长时间占用CPU
4. **系统调用优化**: 异步处理，避免阻塞其他goroutine
5. **内存局部性**: P绑定本地资源，减少锁竞争

理解GMP模型有助于：
- 编写高效的并发程序
- 合理控制goroutine数量
- 优化系统调用使用
- 调试性能问题

掌握调度器原理是Go语言性能优化的基础。
