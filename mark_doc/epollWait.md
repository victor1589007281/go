# golang epoll wait的使用
## 代码路径
```text

netpoll (delay int64) (gList, int32)(src/runtime/netpoll_epoll.go)
├── n, errno := syscall.EpollWait (epfd, events [:], int32 (len (events)), waitms) // 调用 epoll_wait 系统调用，获取就绪的 events
├── δ += netpollready (&toRun, pd, mode) // 遍历 events，唤醒堵塞的协程(src/runtime/netpoll.go)
│ ├── rg = netpollunblock (pd, 'r', true, &δ) // 解除协程的堵塞状态
│ ├── wg = netpollunblock (pd, 'w', true, &δ)
│ ├── toRun.push (rg) // 把解除状态的协程加入到
│ └── toRun.push (wg)

```

## 解读
### 哪里调用它(src/runtime/proc.go)
简单来说就是调度的主循环逻辑中，会负责epoll的处理
- GC STW后恢复相关协程
  - func startTheWorldWithSema(now int64, w worldStop) int64 {
- 调度的时候：寻找一个可运行的协程
  - func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
- 检查当前 P 是否有非后台工作可以执行
  - func pollWork() bool {
- 系统监控的主循环，负责管理空闲状态下的工作
  - func sysmon() {

## events关联
注册的时候，会序列化 pollDesc 的指针和标记，返回的时候反序列化
注册fd的时候，把pollDesc的信息存到epoll中
ev.data 中是就绪的网络 socket 的文件描述符。根据网络就绪 fd 拿到 pollDesc
```text
注册时:
pollDesc ──序列化──> epoll event data

返回时:
epoll event data ──反序列化──> pollDesc

           用户空间              内核空间
     ┌─────────────┐        ┌────────────┐
     │  pollDesc   │        │            │
     │  指针+标记   │───────>│  epoll     │
     └─────────────┘        │            │
           ↑               │  实例      │
           │               │            │
           │               └────────────┘
           │                    │
     ┌─────────────┐           │
     │epoll event  │<──────────┘
     │data字段     │
     └─────────────┘


``` 