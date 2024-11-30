# net.Accept()源码解读
net.Accept()函数用于从监听连接队列中获取一个连接，并将其返回给调用者。
## 代码路径
```go
	for {
		// 接收请求
		conn, _ := listener.Accept()
		// 启动一个协程来处理
		go process(conn)
	}
```
```text
Accept (TCPListener)
├── src/net/tcpsock.go
└── l.accept
    ├── src/net/tcpsock_posix.go
    └── ln.fd.accept
        ├── src/net/fd_unix.go
        └── fd.pfd.Accept
            ├── src/internal/poll/fd_unix.go
            ├── accept(fd.Sysfd)
            │   ├── src/internal/poll/sys_cloexec.go
            │   ├── AcceptFunc(s) // 调用系统调用Accept()
            │   └── syscall.SetNonblock(ns, true) // 设置新socket为非阻塞
            └── fd.pd.waitRead (当没有新连接时)
                ├── src/internal/poll/fd_poll_runtime.go
                └── pd.wait
                    ├── runtime_pollWait
                    │   ├── src/runtime/netpoll.go
                    │   └── poll_runtime_pollWait
                    │       └── netpollblock
                    │           └── gopark // 阻塞当前goroutine
                    └── 处理新连接
                        ├── newFD // 创建新的文件描述符
                        ├── netfd.init // 将新fd加入epoll实例
                        └── newTCPConn // 创建TCP连接对象
```
## 解读
- 调用syscall.Accept()系统调用来获取一个连接。
- 如果没有获取到连接，用户协程会被挂起堵塞。调度器中的epoll_wait协程会在有socket到达的时候，唤醒用户协程
- 用户协程的socket获得新的socket的连接，把收到的socket fd加入到epoll实例进行管理。同时把这个交给process函数进行处理
```text
goroutine(用户代码)                  netpoll线程(运行时)
     |                                    |
     |--netpollblock()                   |
     |(将自己挂起)                        |
     |                                   |
     |                            epoll_wait()
     |                                   |
     |                            (发现I/O就绪)
     |                                   |
     |                            (唤醒对应goroutine)
     |<----------------------------------|
     |(继续执行)                          |
```
```text
                 findrunnable()                schedule()
                      |                            |
                      |                            |
               netpoll(0)                    netpoll(true)
               非阻塞检查                     阻塞等待
                      |                            |
                      |                            |
                  epoll_wait               epoll_wait
                 立即返回结果              阻塞直到有事件
                      |                            |
                      |                            |
              唤醒就绪goroutine          唤醒就绪goroutine
```