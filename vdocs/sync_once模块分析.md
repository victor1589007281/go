# **Sync Once 单次执行模块深度分析**

## **1. 模块概述**

**sync.Once** 是Go语言中用于确保某个函数只被执行一次的同步原语。它提供了一种线程安全的单例模式实现，常用于初始化操作、资源分配等场景。

## **2. 模块结构与架构**

```mermaid
graph TB
    A["Once<br/>单次执行器"] --> B["核心字段"]
    A --> C["操作接口"]
    
    B --> B1["done atomic.Uint32<br/>完成标记"]
    B --> B2["m Mutex<br/>互斥锁"]
    B --> B3["noCopy<br/>防复制标记"]
    
    C --> C1["Do(f func())<br/>执行函数"]
    
    B1 --> D["状态值"]
    D --> D1["0: 未执行"]
    D --> D2["1: 已完成"]

    style A fill:#e1f5fe
    style B fill:#f3e5f5
    style C fill:#e8f5e8
    style B1 fill:#ccffcc
    style B2 fill:#ffffcc
    style B3 fill:#ffcccc
    style C1 fill:#ccccff
    style D fill:#ffecb3
    style D1 fill:#e8f5e8
    style D2 fill:#ccffcc
```

## **3. 核心数据结构**

### **3.1 Once结构定义**

```go
type Once struct {
    _ noCopy
    
    // done字段放在首位以优化热路径
    // 在某些架构上可以获得更紧凑的指令
    done atomic.Uint32
    m    Mutex
}
```

### **3.2 字段布局优化**

```mermaid
graph TB
    A["内存布局优化"] --> B["done字段在前"]
    A --> C["架构优化考虑"]
    
    B --> B1["热路径优先"]
    B --> B2["缓存行对齐"]
    B --> B3["减少内存访问"]
    
    C --> C1["AMD64/386架构<br/>更紧凑指令"]
    C --> C2["其他架构<br/>减少偏移计算"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ffffcc
    style B1 fill:#e8f5e8
    style B2 fill:#e8f5e8
    style B3 fill:#e8f5e8
    style C1 fill:#ffecb3
    style C2 fill:#ffecb3
```

## **4. 核心算法实现**

### **4.1 Do方法实现流程**

```mermaid
flowchart TD
    A["Do(f func())调用"] --> B["done.Load()"]
    B --> C{done == 0?}
    C -->|**否**| D["已执行，直接返回"]
    C -->|**是**| E["doSlow(f)"]
    
    E --> F["m.Lock()"]
    F --> G["defer m.Unlock()"]
    G --> H["再次检查done"]
    H --> I{done == 0?}
    I -->|**否**| J["其他goroutine已执行"]
    I -->|**是**| K["defer done.Store(1)"]
    K --> L["f()"]
    L --> M["执行完成"]
    
    J --> N["释放锁并返回"]
    M --> N

    style A fill:#e1f5fe
    style D fill:#ccffcc
    style E fill:#f3e5f5
    style F fill:#ffffcc
    style K fill:#ffecb3
    style L fill:#ccccff
    style M fill:#ccffcc
    style N fill:#e8f5e8
```

### **4.2 双重检查锁定模式**

```go
func (o *Once) Do(f func()) {
    // 快速路径：检查是否已执行
    if o.done.Load() == 0 {
        // 慢速路径：使用锁确保只执行一次
        o.doSlow(f)
    }
}

func (o *Once) doSlow(f func()) {
    o.m.Lock()
    defer o.m.Unlock()
    
    // 双重检查：可能其他goroutine已经执行了
    if o.done.Load() == 0 {
        defer o.done.Store(1)  // 确保在f()完成后设置
        f()
    }
}
```

## **5. 设计关键点**

### **5.1 为什么需要双重检查**

```mermaid
sequenceDiagram
    participant G1 as Goroutine 1
    participant G2 as Goroutine 2
    participant O as Once
    
    Note over ParticipantA,ParticipantB: 双重检查的必要性
    
    par 并发调用Do
        G1->>O: Do(f1)
        Note right of G1: done.Load() == 0
        G1->>O: 进入doSlow
    and
        G2->>O: Do(f2)
        Note right of G2: done.Load() == 0
        G2->>O: 进入doSlow
    end
    
    G1->>O: 获取锁m.Lock()
    Note right of G1: G1先获取锁
    
    G2->>O: 等待锁m.Lock()
    Note right of G2: G2被阻塞
    
    G1->>O: 检查done == 0
    G1->>G1: 执行f1()
    G1->>O: done.Store(1)
    G1->>O: m.Unlock()
    
    G2->>O: 获取锁
    G2->>O: 检查done == 1
    Note right of G2: 发现已执行，跳过f2
    G2->>O: m.Unlock()
```

### **5.2 Store操作的延迟执行**

```go
// ❌ 错误的实现
if o.done.CompareAndSwap(0, 1) {
    f()  // 如果f()panic，done已经被设置为1
}

// ✅ 正确的实现
if o.done.Load() == 0 {
    defer o.done.Store(1)  // 确保f()完成后才设置
    f()
}
```

## **6. 时序交互分析**

### **6.1 正常执行时序**

```mermaid
sequenceDiagram
    participant G1 as 第一个调用者
    participant G2 as 后续调用者
    participant O as Once
    participant F as 目标函数
    
    G1->>O: Do(init)
    O->>O: done.Load() == 0
    O->>O: 进入doSlow
    O->>O: m.Lock()
    O->>O: done.Load() == 0
    O->>F: 执行init()
    F-->>O: 执行完成
    O->>O: done.Store(1)
    O->>O: m.Unlock()
    O-->>G1: 返回
    
    G2->>O: Do(init)
    O->>O: done.Load() == 1
    O-->>G2: 直接返回(快速路径)
```

### **6.2 异常处理时序**

```mermaid
sequenceDiagram
    participant G1 as 第一个调用者
    participant G2 as 第二个调用者
    participant O as Once
    participant F as Panic函数
    
    Note over G1,G2: 函数执行时panic的处理
    
    G1->>O: Do(panicFunc)
    O->>O: m.Lock()
    O->>F: 执行panicFunc()
    F-->>O: panic!
    Note right of O: defer done.Store(1)仍会执行
    O->>O: done.Store(1)
    O->>O: m.Unlock()
    O-->>G1: panic传播给G1
    
    G2->>O: Do(normalFunc)
    O->>O: done.Load() == 1
    O-->>G2: 直接返回，不执行normalFunc
    
    Note over G1,G2: Once认为已经"执行过"，后续调用被跳过
```

## **7. 使用场景与模式**

### **7.1 典型使用场景**

```mermaid
graph TB
    A["Once使用场景"] --> B["单例初始化"]
    A --> C["配置加载"]
    A --> D["资源初始化"]
    A --> E["昂贵计算"]
    
    B --> B1["数据库连接"]
    B --> B2["日志组件"]
    B --> B3["缓存实例"]
    
    C --> C1["配置文件读取"]
    C --> C2["环境变量解析"]
    C --> C3["默认值设置"]
    
    D --> D1["内存池创建"]
    D --> D2["网络连接"]
    D --> D3["文件句柄"]
    
    E --> E1["预计算结果"]
    E --> E2["查找表构建"]
    E --> E3["编译正则表达式"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ffffcc
    style D fill:#ccccff
    style E fill:#ffecb3
    style B1 fill:#e8f5e8
    style B2 fill:#e8f5e8
    style B3 fill:#e8f5e8
    style C1 fill:#f9f9e9
    style C2 fill:#f9f9e9
    style C3 fill:#f9f9e9
    style D1 fill:#e6f3ff
    style D2 fill:#e6f3ff
    style D3 fill:#e6f3ff
    style E1 fill:#fff0e6
    style E2 fill:#fff0e6
    style E3 fill:#fff0e6
```

### **7.2 最佳实践模式**

```go
// ✅ 推荐：全局Once + 全局变量
var (
    instance *Singleton
    once     sync.Once
)

func GetInstance() *Singleton {
    once.Do(func() {
        instance = &Singleton{
            // 初始化代码
        }
    })
    return instance
}

// ✅ 推荐：包级初始化
var (
    config *Config
    configOnce sync.Once
)

func GetConfig() *Config {
    configOnce.Do(func() {
        config = loadConfigFromFile()
    })
    return config
}
```

## **8. Linux底层支持机制**

### **8.1 原子操作映射**

```mermaid
graph TB
    A["Once操作"] --> B["原子操作"]
    A --> C["Mutex操作"]
    
    B --> B1["done.Load()<br/>原子读取"]
    B --> B2["done.Store()<br/>原子写入"]
    
    C --> C1["m.Lock()<br/>互斥锁获取"]
    C --> C2["m.Unlock()<br/>互斥锁释放"]
    
    B --> D["CPU指令"]
    C --> E["系统调用"]
    
    D --> D1["MOVL(读取)"]
    D --> D2["MOVL(写入)"]
    
    E --> E1["futex系统调用"]
    E --> E2["内核调度器"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ffffcc
    style D fill:#ccccff
    style E fill:#ffecb3
    style B1 fill:#e8f5e8
    style B2 fill:#e8f5e8
    style C1 fill:#f9f9e9
    style C2 fill:#f9f9e9
    style D1 fill:#e6f3ff
    style D2 fill:#e6f3ff
    style E1 fill:#fff0e6
    style E2 fill:#fff0e6
```

### **8.2 内存屏障保证**

| **操作** | **内存屏障类型** | **保证** |
|---------|----------------|---------|
| **done.Load()** | **Acquire语义** | **后续读写不会重排到Load前** |
| **done.Store()** | **Release语义** | **前面的f()执行对后续可见** |
| **m.Lock()** | **Acquire屏障** | **临界区代码不会重排到锁外** |
| **m.Unlock()** | **Release屏障** | **临界区修改对其他goroutine可见** |

## **9. 性能特性分析**

### **9.1 性能优势**

```mermaid
graph TB
    A["Once性能特点"] --> B["快速路径优化"]
    A --> C["最小内存占用"]
    A --> D["无锁读取"]
    
    B --> B1["首次检查成功后<br/>只需一个原子读操作"]
    B --> B2["CPU分支预测友好"]
    
    C --> C1["8字节(64位)<br/>4字节done + 4字节mutex"]
    C --> C2["缓存行友好"]
    
    D --> D1["执行完成后<br/>无同步开销"]
    D --> D2["高并发读取<br/>性能优异"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ffffcc
    style D fill:#ccccff
    style B1 fill:#e8f5e8
    style B2 fill:#e8f5e8
    style C1 fill:#f9f9e9
    style C2 fill:#f9f9e9
    style D1 fill:#e6f3ff
    style D2 fill:#e6f3ff
```

### **9.2 性能基准测试**

| **场景** | **操作耗时** | **内存分配** | **说明** |
|---------|-------------|-------------|---------|
| **首次执行** | **~100ns + f()时间** | **0** | **包含锁获取开销** |
| **后续执行** | **~1-2ns** | **0** | **只需原子读取** |
| **高并发读** | **~1ns/goroutine** | **0** | **无竞争，纯CPU操作** |

## **10. 高级用法与扩展**

### **10.1 条件执行模式**

```go
// 基于条件的Once执行
type ConditionalOnce struct {
    once sync.Once
    cond func() bool
}

func (co *ConditionalOnce) Do(f func()) {
    if co.cond() {
        co.once.Do(f)
    }
}
```

### **10.2 重置功能扩展**

```go
// 可重置的Once（注意：不是标准库功能）
type ResettableOnce struct {
    done int32
    mu   sync.Mutex
}

func (ro *ResettableOnce) Do(f func()) {
    if atomic.LoadInt32(&ro.done) == 0 {
        ro.mu.Lock()
        defer ro.mu.Unlock()
        if ro.done == 0 {
            f()
            atomic.StoreInt32(&ro.done, 1)
        }
    }
}

func (ro *ResettableOnce) Reset() {
    ro.mu.Lock()
    defer ro.mu.Unlock()
    atomic.StoreInt32(&ro.done, 0)
}
```

## **11. 常见陷阱与最佳实践**

### **11.1 典型错误模式**

| **错误类型** | **问题描述** | **解决方案** |
|-------------|-------------|-------------|
| **函数参数变化** | **不同参数调用同一Once** | **每个不同初始化使用不同Once** |
| **递归调用** | **f()内部调用同一Once.Do** | **重新设计避免递归** |
| **panic恢复** | **期望panic后重试** | **Once不支持重试，需要其他方案** |
| **复制Once** | **结构体包含Once被复制** | **使用指针或noCopy检查** |

### **11.2 最佳实践原则**

```mermaid
graph TB
    A["Once最佳实践"] --> B["设计原则"]
    A --> C["使用模式"]
    A --> D["错误避免"]
    
    B --> B1["一个Once一个目的"]
    B --> B2["避免复杂初始化逻辑"]
    B --> B3["考虑失败情况"]
    
    C --> C1["全局变量模式"]
    C --> C2["包级函数封装"]
    C --> C3["lazy初始化"]
    
    D --> D1["不要复制Once"]
    D --> D2["避免递归调用"]
    D --> D3["处理panic情况"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ffffcc
    style D fill:#ffcccc
    style B1 fill:#e8f5e8
    style B2 fill:#e8f5e8
    style B3 fill:#e8f5e8
    style C1 fill:#f9f9e9
    style C2 fill:#f9f9e9
    style C3 fill:#f9f9e9
    style D1 fill:#ffe6e6
    style D2 fill:#ffe6e6
    style D3 fill:#ffe6e6
```

## **12. 内存模型保证**

### **12.1 Happens-Before关系**

```mermaid
sequenceDiagram
    participant G1 as First Caller
    participant F as Function f
    participant G2 as Later Caller
    participant M as Memory
    
    Note over G1,G2: Once内存模型保证
    
    G1->>F: 执行f()内的操作
    F->>M: 内存写入
    F-->>G1: f()返回
    Note over G1,G2: f()完成 "synchronizes before"
    
    G2->>G2: Do()调用
    Note over G1,G2: 任何后续Do()调用
    G2->>M: 读取f()的结果
    Note over G1,G2: 保证能看到f()的所有修改
```

## **13. 与其他同步原语对比**

### **13.1 功能对比**

| **特性** | **Once** | **Mutex** | **Channel** | **atomic** |
|---------|----------|-----------|-------------|------------|
| **单次执行** | **✅ 专用** | **❌ 需要额外逻辑** | **❌ 复杂** | **❌ 需要额外逻辑** |
| **性能** | **✅ 优秀** | **⚠️ 中等** | **⚠️ 中等** | **✅ 优秀** |
| **易用性** | **✅ 简单** | **⚠️ 需要设计** | **⚠️ 复杂** | **❌ 复杂** |
| **灵活性** | **❌ 单一目的** | **✅ 通用** | **✅ 通用** | **✅ 灵活** |

## **14. 局限性分析**

### **14.1 设计局限**

- **不可重置**: 一旦执行完成，无法重新触发
- **不支持条件**: 无法基于运行时条件决定是否执行
- **panic后无法恢复**: 函数panic后，Once仍认为已执行
- **单一函数**: 只能执行一个函数，不支持多个不同函数

### **14.2 适用性边界**

```mermaid
graph LR
    A["初始化复杂度"] --> B["简单初始化"]
    A --> C["中等复杂度"]
    A --> D["复杂初始化"]
    A --> E["动态初始化"]
    
    B --> F["✅ 完美适合"]
    C --> G["✅ 适合"]
    D --> H["⚠️ 需要考虑错误处理"]
    E --> I["❌ 不适合"]

    style A fill:#e1f5fe
    style B fill:#ccffcc
    style C fill:#ccffcc
    style D fill:#ffffcc
    style E fill:#ffcccc
    style F fill:#ccffcc
    style G fill:#ccffcc
    style H fill:#ffffcc
    style I fill:#ffcccc
```

## **15. 总结**

sync.Once是Go语言中实现单次执行语义的精巧同步原语：

- **🎯 专用设计**: 专门为单次执行场景优化
- **⚡ 高性能**: 快速路径优化，执行后无开销
- **🔒 线程安全**: 基于双重检查锁定模式
- **💡 简单易用**: API简洁，零值可用
- **🛡️ 内存安全**: 严格的happens-before保证

**适用于初始化、单例创建等需要确保只执行一次的场景，是Go并发编程工具箱中的重要组件。**
