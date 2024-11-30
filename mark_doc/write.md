# Write写入数据
## 代码路径
```go
	_, _ = conn.Write([]byte("I am server!"))

```
```text
conn.Write()(src/net/net.go) //把buf的数据发送出去
├── n, err := c.fd.Write(b)(src/net/fd_posix.go) // 向底层文件描述符写入数据
│   ├── n, err = fd.pfd.Write(p)(src/internal/poll/fd_unix.go) // 调用 poll.FD 的 Write 方法
│   │   ├── err := fd.pd.prepareWrite(fd.isFile)
│   │   ├── n, err := ignoringEINTRIO(syscall.Write, fd.Sysfd, p[nn:max]) //执行系统调用syscall.Write(),循环执行，直到写完
│   │   │   ├── if err == syscall.EAGAIN && fd.pd.pollable() { //如果系统调用中断，则把fd重新加入到epoll实例中，然后堵塞自身协程
│   │   │   │   ├── if err = fd.pd.waitWrite(fd.isFile) (src/internal/poll/fd_poll_runtime.go)
│   │   │   │   │   ├── pd.wait('w', isFile) // 等待FD变成可写状态
│   │   │   │   │   ├── res := runtime_pollWait(pd.runtimeCtx, mode) // 等待文件描述符就绪
│   │   │   │   │   │   ├── func poll_runtime_pollWait(pd *pollDesc, mode int) int (src/runtime/netpoll.go)
│   │   │   │   │   │   │   ├── for !netpollblock(pd, int32(mode), false) { //阻塞当前 goroutine 直到 I/O 就绪或超时/关闭
│   │   │   │   │   │   │   │   ├── gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceBlockNet, 5)
```