# Go 网络架构与 Netpoll 实现原理

## 概述

Go语言的网络模型采用了高效的事件驱动架构，通过netpoll机制实现了高性能的网络I/O处理。它将阻塞的网络操作转换为非阻塞的事件驱动模式，结合goroutine调度器，实现了对用户透明的异步网络编程模型。

## 网络架构总览

### 1. 分层结构

```
应用层 (net包接口)
    ↓
网络层 (netFD抽象)
    ↓ 
轮询层 (netpoll事件驱动)
    ↓
系统层 (epoll/kqueue/iocp)
```

### 2. 核心组件

- **net包**: 对外提供网络编程接口
- **netFD**: 网络文件描述符抽象
- **netpoll**: 网络轮询器，事件驱动核心
- **pollDesc**: 轮询描述符，连接netpoll和调度器

## netFD 网络文件描述符

### 1. netFD结构

```go
// src/internal/poll/fd.go
type FD struct {
    // 系统文件描述符
    Sysfd int
    
    // 轮询描述符
    pd pollDesc
    
    // 读写锁
    rop operation // 读操作状态
    wop operation // 写操作状态
    
    // 状态标志
    isFile        bool
    isBlocking    bool
    isConnected   bool
    ZeroReadIsEOF bool
    
    // 原子状态
    fdmuR    fdMutex // 读锁
    fdmuW    fdMutex // 写锁
    closing  bool    // 关闭中
}

// 网络特定的文件描述符
type netFD struct {
    pfd poll.FD
    
    // 网络类型
    family int    // AF_INET, AF_INET6
    sotype int    // SOCK_STREAM, SOCK_DGRAM
    net    string // "tcp", "udp", etc
    
    // 地址信息
    laddr Addr // 本地地址
    raddr Addr // 远程地址
    
    // 状态
    isConnected bool
}
```

### 2. netFD创建

```go
// 创建网络FD
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

// 初始化FD用于网络操作
func (fd *netFD) init() error {
    // 设置非阻塞模式
    if err := fd.pfd.Init(fd.net, false); err != nil {
        return err
    }
    
    // 设置socket选项
    if fd.family == syscall.AF_INET6 {
        syscall.SetsockoptInt(fd.pfd.Sysfd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0)
    }
    
    return nil
}
```

### 3. 网络读写操作

```go
// 网络读取
func (fd *netFD) Read(p []byte) (n int, err error) {
    n, err = fd.pfd.Read(p)
    runtime.KeepAlive(fd)
    return n, wrapSyscallError(readSyscallName, err)
}

// 网络写入
func (fd *netFD) Write(p []byte) (nn int, err error) {
    nn, err = fd.pfd.Write(p)
    runtime.KeepAlive(fd)
    return nn, wrapSyscallError(writeSyscallName, err)
}

// 底层读取实现
func (fd *FD) Read(p []byte) (int, error) {
    if err := fd.readLock(); err != nil {
        return 0, err
    }
    defer fd.readUnlock()
    
    if len(p) == 0 {
        return 0, nil
    }
    
    // 准备读取
    if err := fd.pd.prepareRead(fd.isFile); err != nil {
        return 0, err
    }
    
    // 限制读取大小
    if fd.IsStream && len(p) > maxRW {
        p = p[:maxRW]
    }
    
    // 执行读取循环
    for {
        n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p)
        if err != nil {
            n = 0
            if err == syscall.EAGAIN && fd.pd.pollable() {
                // EAGAIN，等待可读事件
                if err = fd.pd.waitRead(fd.isFile); err == nil {
                    continue
                }
            }
        }
        err = fd.eofError(n, err)
        return n, err
    }
}
```

## pollDesc 轮询描述符

### 1. pollDesc结构

```go
// src/runtime/netpoll.go
type pollDesc struct {
    runtimeCtx uintptr // 运行时上下文，指向runtime中的pollDesc
}

// 运行时pollDesc
type pollDesc struct {
    link *pollDesc      // 链表指针
    
    // 锁保护以下字段
    lock mutex
    
    fd      uintptr     // 文件描述符
    closing bool        // 是否正在关闭
    
    // 读写等待者
    rg uintptr          // 等待读的goroutine
    wg uintptr          // 等待写的goroutine
    rt timer            // 读超时定时器
    wt timer            // 写超时定时器
    
    // 事件状态
    rd int64            // 读就绪时间
    wd int64            // 写就绪时间
}
```

### 2. 轮询操作

```go
// 准备读取
func (pd *pollDesc) prepareRead(isFile bool) error {
    return pd.prepare('r', isFile)
}

// 准备写入
func (pd *pollDesc) prepareWrite(isFile bool) error {
    return pd.prepare('w', isFile)
}

// 通用准备函数
func (pd *pollDesc) prepare(mode int, isFile bool) error {
    if pd.runtimeCtx == 0 {
        return errors.New("operation on I/O-unaware fd")
    }
    res := runtime_pollReset(pd.runtimeCtx, mode)
    return convertErr(res, isFile)
}

// 等待读事件
func (pd *pollDesc) waitRead(isFile bool) error {
    return pd.wait('r', isFile)
}

// 等待写事件
func (pd *pollDesc) waitWrite(isFile bool) error {
    return pd.wait('w', isFile)
}

// 通用等待函数
func (pd *pollDesc) wait(mode int, isFile bool) error {
    if pd.runtimeCtx == 0 {
        return errors.New("waiting for unsupported file type")
    }
    res := runtime_pollWait(pd.runtimeCtx, mode)
    return convertErr(res, isFile)
}
```

## Netpoll 事件驱动核心

### 1. 全局netpoll结构

```go
// src/runtime/netpoll_epoll.go (Linux)
var (
    epfd int32 = -1          // epoll文件描述符
    netpollBreakWr uintptr   // 用于唤醒netpoll的写端
    netpollBreakRd uintptr   // 用于唤醒netpoll的读端
)

// netpoll初始化
func netpollinit() {
    // 创建epoll实例
    epfd = epollcreate1(_EPOLL_CLOEXEC)
    if epfd < 0 {
        println("runtime: epollcreate failed with", -epfd)
        throw("runtime: netpollinit failed")
    }
    
    // 创建管道用于中断netpoll
    r, w, errno := nonblockingPipe()
    if errno != 0 {
        println("runtime: pipe failed with", -errno)
        throw("runtime: pipe failed")
    }
    
    // 将管道读端加入epoll
    ev := epollevent{
        events: _EPOLLIN,
        data:   uintptr(r),
    }
    errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)
    if errno != 0 {
        println("runtime: epollctl failed with", -errno)
        throw("runtime: epollctl failed")
    }
    
    netpollBreakRd = uintptr(r)
    netpollBreakWr = uintptr(w)
}
```

### 2. 描述符注册

```go
// 注册文件描述符到netpoll
func netpollopen(fd uintptr, pd *pollDesc) int32 {
    // 配置epoll事件
    var ev epollevent
    ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET // 边缘触发
    *(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
    
    // 添加到epoll
    return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

// 关闭netpoll监听
func netpollclose(fd uintptr) int32 {
    var ev epollevent
    return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

// 设置可读
func netpollarm(pd *pollDesc, mode int) {
    // 通过runtime函数更新状态
    runtime_pollArm(pd.runtimeCtx, mode)
}
```

### 3. 事件轮询

```go
// netpoll核心轮询函数
func netpoll(delay int64) gList {
    if epfd == -1 {
        return gList{}
    }
    
    var waitms int32
    if delay < 0 {
        waitms = -1
    } else if delay == 0 {
        waitms = 0
    } else if delay < 1e6 {
        waitms = 1
    } else if delay < 1e15 {
        waitms = int32(delay / 1e6)
    } else {
        waitms = 1e9
    }
    
    // epoll_wait等待事件
    var events [128]epollevent
retry:
    n := epollwait(epfd, &events[0], int32(len(events)), waitms)
    if n < 0 {
        if n != -_EINTR {
            println("runtime: epollwait on fd", epfd, "failed with", -n)
            throw("runtime: netpoll failed")
        }
        // 中断重试
        if waitms > 0 {
            return gList{}
        }
        goto retry
    }
    
    // 处理就绪事件
    var toRun gList
    for i := int32(0); i < n; i++ {
        ev := &events[i]
        
        if ev.data == netpollBreakRd {
            // 这是唤醒事件，读取数据清空管道
            if delay != 0 {
                var tmp [16]byte
                read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
                atomic.Store(&netpollWakeSig, 0)
            }
            continue
        }
        
        // 获取pollDesc
        pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
        
        var mode int32
        if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
            mode += 'r'
        }
        if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
            mode += 'w'
        }
        
        if mode != 0 {
            // 唤醒等待的goroutine
            netpollready(&toRun, pd, mode)
        }
    }
    
    return toRun
}

// 准备就绪的goroutine
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

## 网络连接实现

### 1. TCP连接

```go
// TCP连接结构
type TCPConn struct {
    conn
}

// 通用连接
type conn struct {
    fd *netFD
}

// 实现net.Conn接口
func (c *conn) Read(b []byte) (int, error) {
    if !c.ok() {
        return 0, syscall.EINVAL
    }
    n, err := c.fd.Read(b)
    if err != nil && err != io.EOF {
        err = &OpError{Op: "read", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
    }
    return n, err
}

func (c *conn) Write(b []byte) (int, error) {
    if !c.ok() {
        return 0, syscall.EINVAL
    }
    n, err := c.fd.Write(b)
    if err != nil {
        err = &OpError{Op: "write", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
    }
    return n, err
}
```

### 2. TCP监听器

```go
// TCP监听器
type TCPListener struct {
    fd *netFD
    lc ListenConfig
}

// 接受连接
func (l *TCPListener) Accept() (Conn, error) {
    if !l.ok() {
        return nil, syscall.EINVAL
    }
    c, err := l.accept()
    if err != nil {
        return nil, &OpError{Op: "accept", Net: l.fd.net, Source: nil, Addr: l.fd.laddr, Err: err}
    }
    return c, nil
}

// 底层accept实现
func (l *TCPListener) accept() (*TCPConn, error) {
    fd, err := l.fd.accept()
    if err != nil {
        return nil, err
    }
    tc := &TCPConn{conn{fd}}
    return tc, nil
}

// netFD的accept方法
func (fd *netFD) accept() (netfd *netFD, err error) {
    d, rsa, errcall, err := fd.pfd.Accept()
    if err != nil {
        if errcall != "" {
            err = wrapSyscallError(errcall, err)
        }
        return nil, err
    }
    
    if netfd, err = newFD(d, fd.family, fd.sotype, fd.net); err != nil {
        poll.CloseFunc(d)
        return nil, err
    }
    if err = netfd.init(); err != nil {
        netfd.Close()
        return nil, err
    }
    
    // 设置远程地址
    lsa, _ := syscall.Getsockname(netfd.pfd.Sysfd)
    netfd.setAddr(netfd.addrFunc()(lsa), netfd.addrFunc()(rsa))
    
    return netfd, nil
}
```

### 3. UDP连接

```go
// UDP连接
type UDPConn struct {
    conn
}

// UDP特有的方法：读取带地址信息
func (c *UDPConn) ReadFromUDP(b []byte) (int, *UDPAddr, error) {
    if !c.ok() {
        return 0, nil, syscall.EINVAL
    }
    n, addr, err := c.readFrom(b)
    if err != nil {
        err = &OpError{Op: "read", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
    }
    return n, addr, err
}

// UDP写入带地址信息
func (c *UDPConn) WriteToUDP(b []byte, addr *UDPAddr) (int, error) {
    if !c.ok() {
        return 0, syscall.EINVAL
    }
    n, err := c.writeTo(b, addr)
    if err != nil {
        err = &OpError{Op: "write", Net: c.fd.net, Source: c.fd.laddr, Addr: addr, Err: err}
    }
    return n, err
}
```

## 超时和截止时间

### 1. 截止时间设置

```go
// 设置读写截止时间
func (fd *netFD) SetDeadline(t time.Time) error {
    return setDeadlineImpl(fd, t, 'r'+'w')
}

func (fd *netFD) SetReadDeadline(t time.Time) error {
    return setDeadlineImpl(fd, t, 'r')
}

func (fd *netFD) SetWriteDeadline(t time.Time) error {
    return setDeadlineImpl(fd, t, 'w')
}

// 设置截止时间的通用实现
func setDeadlineImpl(fd *netFD, t time.Time, mode int) error {
    diff := t.Sub(time.Now())
    d := runtimeNano() + diff.Nanoseconds()
    
    if diff < 0 {
        d = aLongTimeAgo
    }
    if mode == 'r' || mode == 'r'+'w' {
        fd.pfd.pd.setReadDeadline(d)
    }
    if mode == 'w' || mode == 'r'+'w' {
        fd.pfd.pd.setWriteDeadline(d)
    }
    return nil
}
```

### 2. 超时处理

```go
// runtime中的超时处理
func runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
    lock(&pd.lock)
    
    if pd.closing {
        unlock(&pd.lock)
        return
    }
    
    pd.seq++
    if d > 0 {
        // 设置定时器
        if mode == 'r' || mode == 'r'+'w' {
            if pd.rt.f == nil {
                pd.rt.f = netpollReadDeadline
                pd.rt.arg = pd
            }
            pd.rt.when = d
            addtimer(&pd.rt)
        }
        if mode == 'w' || mode == 'r'+'w' {
            if pd.wt.f == nil {
                pd.wt.f = netpollWriteDeadline
                pd.wt.arg = pd
            }
            pd.wt.when = d
            addtimer(&pd.wt)
        }
    } else {
        // 清除定时器
        if mode == 'r' || mode == 'r'+'w' {
            deltimer(&pd.rt)
        }
        if mode == 'w' || mode == 'r'+'w' {
            deltimer(&pd.wt)
        }
    }
    
    unlock(&pd.lock)
}
```

## 网络轮询器与调度器集成

### 1. 调度器中的netpoll

```go
// 调度器中检查网络事件
func findrunnable() (gp *g, inheritTime bool, tryWakeP bool) {
    // ... 其他逻辑
    
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
    
    // ... 继续其他逻辑
}

// 系统监控中的网络检查
func sysmon() {
    // ... 其他逻辑
    
    // 非阻塞地检查网络事件
    if netpollinited() && netpollWaiters.Load() > 0 && lastpoll != 0 && lastpoll+10*1000*1000 < now {
        atomic.Cas64(&sched.lastpoll, uint64(lastpoll), uint64(now))
        list, delta := netpoll(0)
        if !list.empty() {
            incidlelocked(-1)
            injectglist(&list)
            incidlelocked(1)
            netpollAdjustWaiters(delta)
        }
    }
}
```

### 2. goroutine阻塞和唤醒

```go
// 网络操作阻塞goroutine
func runtime_pollWait(pd *pollDesc, mode int) int {
    // 快速检查是否已就绪
    for !netpollblock(pd, int32(mode), false) {
        err := netpollcheckerr(pd, int32(mode))
        if err != 0 {
            return err
        }
        
        // 没有准备好，阻塞当前goroutine
        if err := netpollcheckerr(pd, int32(mode)); err != 0 {
            return err
        }
        
        // 设置等待状态并park
        if netpollblock(pd, int32(mode), true) {
            break
        }
    }
    return 0
}

// 阻塞goroutine等待网络事件
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
    gpp := &pd.rg
    if mode == 'w' {
        gpp = &pd.wg
    }
    
    // 设置等待者
    for {
        old := *gpp
        if old == pdReady {
            *gpp = 0
            return true
        }
        if old != 0 {
            throw("runtime: double wait")
        }
        if waitio || gpp == &pd.rg {
            gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
        }
        
        // 检查是否被唤醒
        old = *gpp
        if old > pdWait {
            return true
        }
    }
}
```

## 性能优化

### 1. 零拷贝优化

```go
// sendfile系统调用支持
func (c *TCPConn) ReadFrom(r io.Reader) (int64, error) {
    if !c.ok() {
        return 0, syscall.EINVAL
    }
    
    // 尝试sendfile优化
    if rf, ok := r.(*os.File); ok {
        return sendFile(c.fd, rf)
    }
    
    // 通用实现
    return genericReadFrom(c, r)
}

// Linux sendfile实现
func sendFile(dstFD *netFD, src *os.File) (written int64, err error) {
    // 获取源文件大小
    fi, err := src.Stat()
    if err != nil {
        return 0, err
    }
    
    remain := fi.Size()
    
    for remain > 0 {
        n, err := syscall.Sendfile(int(dstFD.pfd.Sysfd), int(src.Fd()), nil, int(remain))
        if n > 0 {
            written += int64(n)
            remain -= int64(n)
        }
        if err != nil {
            if err == syscall.EAGAIN {
                if err = dstFD.pfd.waitWrite(); err == nil {
                    continue
                }
            }
            break
        }
        if n == 0 {
            break
        }
    }
    
    return written, err
}
```

### 2. 批量accept优化

```go
// 批量accept连接（某些平台支持）
func (fd *netFD) batchAccept(maxConn int) ([]*netFD, error) {
    var connections []*netFD
    
    for i := 0; i < maxConn; i++ {
        conn, err := fd.accept()
        if err != nil {
            if err == syscall.EAGAIN {
                break // 没有更多连接
            }
            return connections, err
        }
        connections = append(connections, conn)
    }
    
    return connections, nil
}
```

### 3. 连接池

```go
// TCP连接池
type ConnPool struct {
    network string
    address string
    conns   chan *TCPConn
    maxConn int
}

func NewConnPool(network, address string, maxConn int) *ConnPool {
    return &ConnPool{
        network: network,
        address: address,
        conns:   make(chan *TCPConn, maxConn),
        maxConn: maxConn,
    }
}

func (p *ConnPool) Get() (*TCPConn, error) {
    select {
    case conn := <-p.conns:
        // 检查连接是否仍然有效
        if p.isConnAlive(conn) {
            return conn, nil
        }
        conn.Close()
    default:
    }
    
    // 创建新连接
    return DialTCP(p.network, nil, p.address)
}

func (p *ConnPool) Put(conn *TCPConn) {
    select {
    case p.conns <- conn:
    default:
        conn.Close() // 池满了，关闭连接
    }
}

func (p *ConnPool) isConnAlive(conn *TCPConn) bool {
    // 简单的连接存活检查
    conn.SetReadDeadline(time.Now().Add(time.Millisecond))
    defer conn.SetReadDeadline(time.Time{})
    
    var b [1]byte
    n, err := conn.Read(b[:])
    if err != nil && err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
        return false
    }
    return n == 0
}
```

## 错误处理

### 1. 网络错误类型

```go
// 网络操作错误
type OpError struct {
    Op     string
    Net    string
    Source Addr
    Addr   Addr
    Err    error
}

func (e *OpError) Error() string {
    if e == nil {
        return "<nil>"
    }
    s := e.Op
    if e.Net != "" {
        s += " " + e.Net
    }
    if e.Source != nil {
        s += " " + e.Source.String()
    }
    if e.Addr != nil {
        if e.Source != nil {
            s += "->"
        } else {
            s += " "
        }
        s += e.Addr.String()
    }
    s += ": " + e.Err.Error()
    return s
}

func (e *OpError) Unwrap() error { return e.Err }
func (e *OpError) Timeout() bool {
    t, ok := e.Err.(timeout)
    return ok && t.Timeout()
}
func (e *OpError) Temporary() bool {
    t, ok := e.Err.(temporary)
    return ok && t.Temporary()
}
```

### 2. 超时错误处理

```go
// 检查是否为超时错误
func isTimeout(err error) bool {
    if e, ok := err.(interface{ Timeout() bool }); ok {
        return e.Timeout()
    }
    return false
}

// 检查是否为临时错误
func isTemporary(err error) bool {
    if e, ok := err.(interface{ Temporary() bool }); ok {
        return e.Temporary()
    }
    return false
}
```

## 监控和调试

### 1. 网络统计

```go
// 网络统计信息
type NetStats struct {
    ActiveConns    int64 // 活跃连接数
    TotalAccept    int64 // 总accept次数
    TotalRead      int64 // 总读取字节数
    TotalWrite     int64 // 总写入字节数
    ReadOps        int64 // 读操作次数
    WriteOps       int64 // 写操作次数
    NetpollWaits   int64 // netpoll等待次数
    NetpollReturns int64 // netpoll返回次数
}

var netStats NetStats

// 更新统计信息
func updateNetStats() {
    atomic.AddInt64(&netStats.NetpollWaits, 1)
}
```

### 2. 连接跟踪

```go
// 连接跟踪器
type ConnTracker struct {
    conns map[string]*ConnInfo
    mutex sync.RWMutex
}

type ConnInfo struct {
    LocalAddr    string
    RemoteAddr   string
    CreateTime   time.Time
    LastActivity time.Time
    BytesRead    int64
    BytesWritten int64
}

func (ct *ConnTracker) TrackConn(conn *TCPConn) {
    ct.mutex.Lock()
    defer ct.mutex.Unlock()
    
    key := conn.LocalAddr().String() + "->" + conn.RemoteAddr().String()
    ct.conns[key] = &ConnInfo{
        LocalAddr:    conn.LocalAddr().String(),
        RemoteAddr:   conn.RemoteAddr().String(),
        CreateTime:   time.Now(),
        LastActivity: time.Now(),
    }
}

func (ct *ConnTracker) UpdateActivity(conn *TCPConn, bytesRead, bytesWritten int64) {
    ct.mutex.Lock()
    defer ct.mutex.Unlock()
    
    key := conn.LocalAddr().String() + "->" + conn.RemoteAddr().String()
    if info, exists := ct.conns[key]; exists {
        info.LastActivity = time.Now()
        info.BytesRead += bytesRead
        info.BytesWritten += bytesWritten
    }
}
```

## 总结

Go的网络架构通过netpoll机制实现了高性能的事件驱动网络编程模型：

1. **统一抽象**: netFD提供了统一的网络文件描述符抽象
2. **事件驱动**: netpoll将阻塞IO转换为事件驱动的异步模型
3. **调度器集成**: 与GMP调度器深度集成，实现高效的goroutine调度
4. **跨平台支持**: 支持epoll、kqueue、iocp等不同平台的IO多路复用
5. **性能优化**: 零拷贝、批量操作、连接池等优化技术

理解网络架构原理有助于：
- 构建高性能网络应用
- 进行网络性能调优
- 解决网络相关的性能问题
- 选择合适的网络编程模式

掌握netpoll机制是Go网络编程优化的关键。
