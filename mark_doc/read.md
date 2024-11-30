# read读取数据
## 代码路径
```go
	_, _ = conn.Read(buf[:])
```
```text
conn.Read()(src/net/net.go) //读取数据到buf
├── ---	n, err := c.fd.Read(b)(src/net/fd_posix.go) // 从底层文件描述符读取数据
│   ├── -------	n, err = fd.pfd.Read(p)(src/internal/poll/fd_unix.go) // 调用 poll.FD 的 Read 方法
│   │   ├── ------------   err := fd.pd.prepareRead(fd.isFile)
│   │   ├── ------------   n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p) //执行系统调用syscall.read(),循环执行，直到遇到EOF
│   │   │   ├── ------------			if err == syscall.EAGAIN && fd.pd.pollable() { //如果系统调用中断，则把fd重新加入到epoll实例中，然后堵塞自身协程
│   │   │   │   ├── ------------				if err = fd.pd.waitRead(fd.isFile); err == nil {
│   │   │   │   └── ------------   err = fd.eofError(n, err) // 检查 EOF 错误
```