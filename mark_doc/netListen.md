# 网络包的Listen()函数的逻辑
## 代码路径
```go
listener, _ := net.Listen("tcp", "127.0.0.1:6666")
```
```text
Listen
├── src/net/dial.go
└── lc.Listen (ListenConfig)
    ├── src/net/dial.go
    └── sl.listenTCP (sysListener)
        ├── src/net/tcpsock_posix.go
        └── sl.listenTCPProto
            ├── src/net/tcpsock_posix.go
            └── internetSocket
                ├── src/net/ipsock_posix.go
                └── socket
                    ├── src/net/sock_posix.go
                    ├── sysSocket(family, sotype, proto)
                    ├── setDefaultSockopts(s, family, sotype, ipv6only)
                    ├── newFD(s, family, sotype, net) // 创建socket,获取socket fd
                    └── fd.listenStream
                        ├── src/net/sock_posix.go
                        ├── syscall.Bind(fd.pfd.Sysfd, lsa) // Bind
                        ├── listenFunc(fd.pfd.Sysfd, backlog)
                        └── fd.init
                            ├── src/net/fd_unix.go
                            └── fd.pfd.Init(fd.net, true)
                                ├── src/internal/poll/fd_unix.go
                                ├── fd.pd.init(fd) // 进入运行时，不同操作系统进入不同的方法
                                ├── serverInit.Do(runtime_pollServerInit)
                                │   ├── src/runtime/netpoll.go
                                │   └── netpollGenericInit
                                │       ├── src/runtime/netpoll.go
                                │       └── netpollinit // 进入Linux系统的方法
                                │           ├── src/runtime/netpoll_epoll.go
                                │           ├── syscall.EpollCreate1 // 创建epoll,返回fd
                                │           ├── syscall.Eventfd // 创建eventfd，用于唤醒epoll_wait
                                │           └── syscall.EpollCtl // 将eventfd添加到epoll实例中
                                └── runtime_pollOpen
                                    ├── src/runtime/netpoll_epoll.go
                                    ├── ev.Events = EPOLLIN|EPOLLOUT|EPOLLRDHUP|EPOLLET
                                    └── syscall.EpollCtl // 将socket fd添加到epoll实例中
```