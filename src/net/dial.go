package net

import (
	"context"
	"internal/bytealg"
	"internal/godebug"
	"internal/nettrace"
	"syscall"
	"time"
)

const (
	// defaultTCPKeepAliveIdle is a default constant value for TCP_KEEPIDLE.
	// See go.dev/issue/31510 for details.
	defaultTCPKeepAliveIdle = 15 * time.Second

	// defaultTCPKeepAliveInterval is a default constant value for TCP_KEEPINTVL.
	// It is the same as defaultTCPKeepAliveIdle, see go.dev/issue/31510 for details.
	defaultTCPKeepAliveInterval = 15 * time.Second

	// defaultTCPKeepAliveCount is a default constant value for TCP_KEEPCNT.
	defaultTCPKeepAliveCount = 9

	// For the moment, MultiPath TCP is not used by default
	// See go.dev/issue/56539
	defaultMPTCPEnabled = false
)

var multipathtcp = godebug.New("multipathtcp")

// mptcpStatus is a tristate for Multipath TCP, see go.dev/issue/56539
type mptcpStatus uint8

const (
	// The value 0 is the system default, linked to defaultMPTCPEnabled
	mptcpUseDefault mptcpStatus = iota
	mptcpEnabled
	mptcpDisabled
)

func (m *mptcpStatus) get() bool {
	switch *m {
	case mptcpEnabled:
		return true
	case mptcpDisabled:
		return false
	}

	// If MPTCP is forced via GODEBUG=multipathtcp=1
	if multipathtcp.Value() == "1" {
		multipathtcp.IncNonDefault()

		return true
	}

	return defaultMPTCPEnabled
}

func (m *mptcpStatus) set(use bool) {
	if use {
		*m = mptcpEnabled
	} else {
		*m = mptcpDisabled
	}
}

// A Dialer contains options for connecting to an address.
//
// The zero value for each field is equivalent to dialing
// without that option. Dialing with the zero value of Dialer
// is therefore equivalent to just calling the [Dial] function.
//
// It is safe to call Dialer's methods concurrently.
type Dialer struct {
	// Timeout is the maximum amount of time a dial will wait for
	// a connect to complete. If Deadline is also set, it may fail
	// earlier.
	//
	// The default is no timeout.
	//
	// When using TCP and dialing a host name with multiple IP
	// addresses, the timeout may be divided between them.
	//
	// With or without a timeout, the operating system may impose
	// its own earlier timeout. For instance, TCP timeouts are
	// often around 3 minutes.
	//
	// Timeout 是拨号等待连接完成的最大时间。如果同时设置了 Deadline，
	// 可能会更早失败。
	//
	// 默认值为零，表示没有超时限制。
	//
	// 当使用 TCP 连接具有多个 IP 地址的主机名时，超时时间可能会在这些地址之间分配。
	//
	// 无论是否设置超时，操作系统可能会施加自己的更早的超时限制。
	// 例如，TCP 超时通常在 3 分钟左右。
	Timeout time.Duration

	// Deadline is the absolute point in time after which dials
	// will fail. If Timeout is set, it may fail earlier.
	// Zero means no deadline, or dependent on the operating system
	// as with the Timeout option.
	//
	// Deadline 是拨号失败的绝对时间点。如果设置了 Timeout，可能会更早失败。
	// 零值表示没有截止时间，或者依赖于操作系统的超时设置。
	Deadline time.Time

	// LocalAddr is the local address to use when dialing an
	// address. The address must be of a compatible type for the
	// network being dialed.
	// If nil, a local address is automatically chosen.
	//
	// LocalAddr 是进行拨号时使用的本地地址。该地址类型必须与正在拨号的
	// 网络类型兼容。
	// 如果为 nil，将自动选择一个本地地址。
	LocalAddr Addr

	// DualStack previously enabled RFC 6555 Fast Fallback
	// support, also known as "Happy Eyeballs", in which IPv4 is
	// tried soon if IPv6 appears to be misconfigured and
	// hanging.
	//
	// Deprecated: Fast Fallback is enabled by default. To
	// disable, set FallbackDelay to a negative value.
	//
	// DualStack 以前用于启用 RFC 6555 快速回退支持，也称为"Happy Eyeballs"，
	// 当 IPv6 似乎配置错误或挂起时，会快速尝试 IPv4。
	//
	// 已弃用：快速回退默认已启用。要禁用，请将 FallbackDelay 设置为负值。
	DualStack bool

	// FallbackDelay specifies the length of time to wait before
	// spawning a RFC 6555 Fast Fallback connection. That is, this
	// is the amount of time to wait for IPv6 to succeed before
	// assuming that IPv6 is misconfigured and falling back to
	// IPv4.
	//
	// If zero, a default delay of 300ms is used.
	// A negative value disables Fast Fallback support.
	//
	// FallbackDelay 指定在启动 RFC 6555 快速回退连接之前等待的时间长度。
	// 即在假定 IPv6 配置错误并回退到 IPv4 之前，等待 IPv6 成功的时间。
	//
	// 如果为零，使用默认的 300 毫秒延迟。
	// 负值将禁用快速回退支持。
	FallbackDelay time.Duration

	// KeepAlive specifies the interval between keep-alive
	// probes for an active network connection.
	//
	// KeepAlive is ignored if KeepAliveConfig.Enable is true.
	//
	// If zero, keep-alive probes are sent with a default value
	// (currently 15 seconds), if supported by the protocol and operating
	// system. Network protocols or operating systems that do
	// not support keep-alive ignore this field.
	// If negative, keep-alive probes are disabled.
	//
	// KeepAlive 指定活动网络连接的保活探测间隔。
	//
	// 如果 KeepAliveConfig.Enable 为 true，则忽略此字段。
	//
	// 如果为零，且协议和操作系统支持，将使用默认值（当前为 15 秒）发送保活探测。
	// 不支持保活的网络协议或操作系统会忽略此字段。
	// 如果为负值，则禁用保活探测。
	KeepAlive time.Duration

	// KeepAliveConfig specifies the keep-alive probe configuration
	// for an active network connection, when supported by the
	// protocol and operating system.
	//
	// If KeepAliveConfig.Enable is true, keep-alive probes are enabled.
	// If KeepAliveConfig.Enable is false and KeepAlive is negative,
	// keep-alive probes are disabled.
	//
	// KeepAliveConfig 指定活动网络连接的保活探测配置，
	// 当协议和操作系统支持时生效。
	//
	// 如果 KeepAliveConfig.Enable 为 true，则启用保活探测。
	// 如果 KeepAliveConfig.Enable 为 false 且 KeepAlive 为负值，
	// 则禁用保活探测。
	KeepAliveConfig KeepAliveConfig

	// Resolver optionally specifies an alternate resolver to use.
	//
	// Resolver 可选地指定要使用的替代解析器。
	Resolver *Resolver

	// Cancel is an optional channel whose closure indicates that
	// the dial should be canceled. Not all types of dials support
	// cancellation.
	//
	// Deprecated: Use DialContext instead.
	//
	// Cancel 是一个可选的通道，其关闭表示应取消拨号。
	// 并非所有类型的拨号都支持取消。
	//
	// 已弃用：请使用 DialContext 替代。
	Cancel <-chan struct{}

	// If Control is not nil, it is called after creating the network
	// connection but before actually dialing.
	//
	// Network and address parameters passed to Control function are not
	// necessarily the ones passed to Dial. For example, passing "tcp" to Dial
	// will cause the Control function to be called with "tcp4" or "tcp6".
	//
	// Control is ignored if ControlContext is not nil.
	//
	// Control 如果不为 nil，将在创建网络连接之后但在实际拨号之前调用。
	//
	// 传递给 Control 函数的网络和地址参数不一定是传递给 Dial 的参数。
	// 例如，向 Dial 传递 "tcp" 将导致使用 "tcp4" 或 "tcp6" 调用 Control 函数。
	//
	// 如果 ControlContext 不为 nil，则忽略 Control。
	Control func(network, address string, c syscall.RawConn) error

	// If ControlContext is not nil, it is called after creating the network
	// connection but before actually dialing.
	//
	// Network and address parameters passed to ControlContext function are not
	// necessarily the ones passed to Dial. For example, passing "tcp" to Dial
	// will cause the ControlContext function to be called with "tcp4" or "tcp6".
	//
	// If ControlContext is not nil, Control is ignored.
	//
	// ControlContext 如果不为 nil，将在创建网络连接之后但在实际拨号之前调用。
	//
	// 传递给 ControlContext 函数的网络和地址参数不一定是传递给 Dial 的参数。
	// 例如，向 Dial 传递 "tcp" 将导致使用 "tcp4" 或 "tcp6" 调用 ControlContext 函数。
	//
	// 如果 ControlContext 不为 nil，则忽略 Control。
	ControlContext func(ctx context.Context, network, address string, c syscall.RawConn) error

	// If mptcpStatus is set to a value allowing Multipath TCP (MPTCP) to be
	// used, any call to Dial with "tcp(4|6)" as network will use MPTCP if
	// supported by the operating system.
	//
	// 如果 mptcpStatus 设置为允许使用多路径 TCP (MPTCP) 的值，
	// 则使用 "tcp(4|6)" 作为网络的任何 Dial 调用都将在操作系统支持的情况下使用 MPTCP。
	mptcpStatus mptcpStatus
}

func (d *Dialer) dualStack() bool { return d.FallbackDelay >= 0 }

func minNonzeroTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

// deadline returns the earliest of:
//   - now+Timeout
//   - d.Deadline
//   - the context's deadline
//
// Or zero, if none of Timeout, Deadline, or context's deadline is set.
func (d *Dialer) deadline(ctx context.Context, now time.Time) (earliest time.Time) {
	if d.Timeout != 0 { // including negative, for historical reasons
		earliest = now.Add(d.Timeout)
	}
	if d, ok := ctx.Deadline(); ok {
		earliest = minNonzeroTime(earliest, d)
	}
	return minNonzeroTime(earliest, d.Deadline)
}

func (d *Dialer) resolver() *Resolver {
	if d.Resolver != nil {
		return d.Resolver
	}
	return DefaultResolver
}

// partialDeadline returns the deadline to use for a single address,
// when multiple addresses are pending.
func partialDeadline(now, deadline time.Time, addrsRemaining int) (time.Time, error) {
	if deadline.IsZero() {
		return deadline, nil
	}
	timeRemaining := deadline.Sub(now)
	if timeRemaining <= 0 {
		return time.Time{}, errTimeout
	}
	// Tentatively allocate equal time to each remaining address.
	timeout := timeRemaining / time.Duration(addrsRemaining)
	// If the time per address is too short, steal from the end of the list.
	const saneMinimum = 2 * time.Second
	if timeout < saneMinimum {
		if timeRemaining < saneMinimum {
			timeout = timeRemaining
		} else {
			timeout = saneMinimum
		}
	}
	return now.Add(timeout), nil
}

func (d *Dialer) fallbackDelay() time.Duration {
	if d.FallbackDelay > 0 {
		return d.FallbackDelay
	} else {
		return 300 * time.Millisecond
	}
}

func parseNetwork(ctx context.Context, network string, needsProto bool) (afnet string, proto int, err error) {
	i := bytealg.LastIndexByteString(network, ':')
	if i < 0 { // no colon
		switch network {
		case "tcp", "tcp4", "tcp6":
		case "udp", "udp4", "udp6":
		case "ip", "ip4", "ip6":
			if needsProto {
				return "", 0, UnknownNetworkError(network)
			}
		case "unix", "unixgram", "unixpacket":
		default:
			return "", 0, UnknownNetworkError(network)
		}
		return network, 0, nil
	}
	afnet = network[:i]
	switch afnet {
	case "ip", "ip4", "ip6":
		protostr := network[i+1:]
		proto, i, ok := dtoi(protostr)
		if !ok || i != len(protostr) {
			proto, err = lookupProtocol(ctx, protostr)
			if err != nil {
				return "", 0, err
			}
		}
		return afnet, proto, nil
	}
	return "", 0, UnknownNetworkError(network)
}

// resolveAddrList resolves addr using hint and returns a list of
// addresses. The result contains at least one address when error is
// nil.
func (r *Resolver) resolveAddrList(ctx context.Context, op, network, addr string, hint Addr) (addrList, error) {
	afnet, _, err := parseNetwork(ctx, network, true)
	if err != nil {
		return nil, err
	}
	if op == "dial" && addr == "" {
		return nil, errMissingAddress
	}
	switch afnet {
	case "unix", "unixgram", "unixpacket":
		addr, err := ResolveUnixAddr(afnet, addr)
		if err != nil {
			return nil, err
		}
		if op == "dial" && hint != nil && addr.Network() != hint.Network() {
			return nil, &AddrError{Err: "mismatched local address type", Addr: hint.String()}
		}
		return addrList{addr}, nil
	}
	addrs, err := r.internetAddrList(ctx, afnet, addr)
	if err != nil || op != "dial" || hint == nil {
		return addrs, err
	}
	var (
		tcp      *TCPAddr
		udp      *UDPAddr
		ip       *IPAddr
		wildcard bool
	)
	switch hint := hint.(type) {
	case *TCPAddr:
		tcp = hint
		wildcard = tcp.isWildcard()
	case *UDPAddr:
		udp = hint
		wildcard = udp.isWildcard()
	case *IPAddr:
		ip = hint
		wildcard = ip.isWildcard()
	}
	naddrs := addrs[:0]
	for _, addr := range addrs {
		if addr.Network() != hint.Network() {
			return nil, &AddrError{Err: "mismatched local address type", Addr: hint.String()}
		}
		switch addr := addr.(type) {
		case *TCPAddr:
			if !wildcard && !addr.isWildcard() && !addr.IP.matchAddrFamily(tcp.IP) {
				continue
			}
			naddrs = append(naddrs, addr)
		case *UDPAddr:
			if !wildcard && !addr.isWildcard() && !addr.IP.matchAddrFamily(udp.IP) {
				continue
			}
			naddrs = append(naddrs, addr)
		case *IPAddr:
			if !wildcard && !addr.isWildcard() && !addr.IP.matchAddrFamily(ip.IP) {
				continue
			}
			naddrs = append(naddrs, addr)
		}
	}
	if len(naddrs) == 0 {
		return nil, &AddrError{Err: errNoSuitableAddress.Error(), Addr: hint.String()}
	}
	return naddrs, nil
}

// MultipathTCP reports whether MPTCP will be used.
//
// This method doesn't check if MPTCP is supported by the operating
// system or not.
func (d *Dialer) MultipathTCP() bool {
	return d.mptcpStatus.get()
}

// SetMultipathTCP directs the [Dial] methods to use, or not use, MPTCP,
// if supported by the operating system. This method overrides the
// system default and the GODEBUG=multipathtcp=... setting if any.
//
// If MPTCP is not available on the host or not supported by the server,
// the Dial methods will fall back to TCP.
func (d *Dialer) SetMultipathTCP(use bool) {
	d.mptcpStatus.set(use)
}

// Dial connects to the address on the named network.
//
// Known networks are "tcp", "tcp4" (IPv4-only), "tcp6" (IPv6-only),
// "udp", "udp4" (IPv4-only), "udp6" (IPv6-only), "ip", "ip4"
// (IPv4-only), "ip6" (IPv6-only), "unix", "unixgram" and
// "unixpacket".
//
// For TCP and UDP networks, the address has the form "host:port".
// The host must be a literal IP address, or a host name that can be
// resolved to IP addresses.
// The port must be a literal port number or a service name.
// If the host is a literal IPv6 address it must be enclosed in square
// brackets, as in "[2001:db8::1]:80" or "[fe80::1%zone]:80".
// The zone specifies the scope of the literal IPv6 address as defined
// in RFC 4007.
// The functions [JoinHostPort] and [SplitHostPort] manipulate a pair of
// host and port in this form.
// When using TCP, and the host resolves to multiple IP addresses,
// Dial will try each IP address in order until one succeeds.
//
// Examples:
//
//	Dial("tcp", "golang.org:http")
//	Dial("tcp", "192.0.2.1:http")
//	Dial("tcp", "198.51.100.1:80")
//	Dial("udp", "[2001:db8::1]:domain")
//	Dial("udp", "[fe80::1%lo0]:53")
//	Dial("tcp", ":80")
//
// For IP networks, the network must be "ip", "ip4" or "ip6" followed
// by a colon and a literal protocol number or a protocol name, and
// the address has the form "host". The host must be a literal IP
// address or a literal IPv6 address with zone.
// It depends on each operating system how the operating system
// behaves with a non-well known protocol number such as "0" or "255".
//
// Examples:
//
//	Dial("ip4:1", "192.0.2.1")
//	Dial("ip6:ipv6-icmp", "2001:db8::1")
//	Dial("ip6:58", "fe80::1%lo0")
//
// For TCP, UDP and IP networks, if the host is empty or a literal
// unspecified IP address, as in ":80", "0.0.0.0:80" or "[::]:80" for
// TCP and UDP, "", "0.0.0.0" or "::" for IP, the local system is
// assumed.
//
// For Unix networks, the address must be a file system path.
func Dial(network, address string) (Conn, error) {
	var d Dialer
	return d.Dial(network, address)
}

// DialTimeout acts like [Dial] but takes a timeout.
//
// The timeout includes name resolution, if required.
// When using TCP, and the host in the address parameter resolves to
// multiple IP addresses, the timeout is spread over each consecutive
// dial, such that each is given an appropriate fraction of the time
// to connect.
//
// See func Dial for a description of the network and address
// parameters.
func DialTimeout(network, address string, timeout time.Duration) (Conn, error) {
	d := Dialer{Timeout: timeout}
	return d.Dial(network, address)
}

// sysDialer contains a Dial's parameters and configuration.
type sysDialer struct {
	Dialer
	network, address string
	testHookDialTCP  func(ctx context.Context, net string, laddr, raddr *TCPAddr) (*TCPConn, error)
}

// Dial connects to the address on the named network.
//
// See func Dial for a description of the network and address
// parameters.
//
// Dial uses [context.Background] internally; to specify the context, use
// [Dialer.DialContext].
func (d *Dialer) Dial(network, address string) (Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext connects to the address on the named network using
// the provided context.
//
// The provided Context must be non-nil. If the context expires before
// the connection is complete, an error is returned. Once successfully
// connected, any expiration of the context will not affect the
// connection.
//
// When using TCP, and the host in the address parameter resolves to multiple
// network addresses, any dial timeout (from d.Timeout or ctx) is spread
// over each consecutive dial, such that each is given an appropriate
// fraction of the time to connect.
// For example, if a host has 4 IP addresses and the timeout is 1 minute,
// the connect to each single address will be given 15 seconds to complete
// before trying the next one.
//
// See func [Dial] for a description of the network and address
// parameters.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (Conn, error) {
	// DialContext 使用提供的上下文连接到指定网络地址
	if ctx == nil {
		panic("nil context")
	}

	// 获取连接截止时间
	deadline := d.deadline(ctx, time.Now())
	if !deadline.IsZero() {
		testHookStepTime()
		// 如果号器的截止间早于上下文的截止时间，创建新的子上下文
		if d, ok := ctx.Deadline(); !ok || deadline.Before(d) {
			subCtx, cancel := context.WithDeadline(ctx, deadline)
			defer cancel()
			ctx = subCtx
		}
	}

	// 处理已弃用的 Cancel 字段
	if oldCancel := d.Cancel; oldCancel != nil {
		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// 启动 goroutine 监听取消信号
		go func() {
			select {
			case <-oldCancel:
				cancel()
			case <-subCtx.Done():
			}
		}()
		ctx = subCtx
	}

	// Shadow the nettrace (if any) during resolve so Connect events don't fire for DNS lookups.
	// 在解析过程中屏蔽网络追踪，避免 DNS 查询触发连接事件
	resolveCtx := ctx
	if trace, _ := ctx.Value(nettrace.TraceKey{}).(*nettrace.Trace); trace != nil {
		shadow := *trace
		shadow.ConnectStart = nil
		shadow.ConnectDone = nil
		resolveCtx = context.WithValue(resolveCtx, nettrace.TraceKey{}, &shadow)
	}

	// 解析地址列表
	addrs, err := d.resolver().resolveAddrList(resolveCtx, "dial", network, address, d.LocalAddr)
	if err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
	}

	// 创建系统拨号器
	sd := &sysDialer{
		Dialer:  *d,
		network: network,
		address: address,
	}

	// 对于 TCP 连接，将地址分为主要地址和后备地址
	var primaries, fallbacks addrList
	if d.dualStack() && network == "tcp" {
		// 如果启用了双栈模式，将地址按 IPv4 和 IPv6 分类
		primaries, fallbacks = addrs.partition(isIPv4)
	} else {
		// 否则所有地址都作为主要地址
		primaries = addrs
	}

	// 并行尝试连接主要地址和后备地址
	return sd.dialParallel(ctx, primaries, fallbacks)
}

// dialParallel races two copies of dialSerial, giving the first a
// head start. It returns the first established connection and
// closes the others. Otherwise it returns an error from the first
// primary address.
// dialParallel 并行执行两个 dialSerial 副本，让第一个先启动。
// 返回第一个建立的连接并关闭其他连接。如果都失败则返回第一个主要地址的错误。
func (sd *sysDialer) dialParallel(ctx context.Context, primaries, fallbacks addrList) (Conn, error) {
	// 如果没有后备地址，直接使用主要地址进行串行拨号
	if len(fallbacks) == 0 {
		return sd.dialSerial(ctx, primaries)
	}

	// 创建一个通道用��通知其他 goroutine 函数已返回
	returned := make(chan struct{})
	defer close(returned)

	// 定义拨号结果的结构体
	type dialResult struct {
		Conn         // 连接
		error        // 错误
		primary bool // 是否为主要地址
		done    bool // 是否完成
	}
	// 创建无缓冲通道用于接收拨号结果
	results := make(chan dialResult)

	// 定义启动拨号竞争者的函数
	startRacer := func(ctx context.Context, primary bool) {
		// 根据是否为主要地址选择地址列表
		ras := primaries
		if !primary {
			ras = fallbacks
		}
		// 执行串行拨号
		c, err := sd.dialSerial(ctx, ras)
		select {
		case results <- dialResult{Conn: c, error: err, primary: primary, done: true}:
		case <-returned:
			// 如果函数已返回，关闭建立的连接
			if c != nil {
				c.Close()
			}
		}
	}

	// 存储主要和后备拨号的结果
	var primary, fallback dialResult

	// 启动主要地址的拨号
	primaryCtx, primaryCancel := context.WithCancel(ctx)
	defer primaryCancel()
	go startRacer(primaryCtx, true)

	// 启动后备地址拨号的定时器
	fallbackTimer := time.NewTimer(sd.fallbackDelay())
	defer fallbackTimer.Stop()

	// 等待拨号结果
	for {
		select {
		case <-fallbackTimer.C:
			// ��时器到期，启动后备地址的拨号
			fallbackCtx, fallbackCancel := context.WithCancel(ctx)
			defer fallbackCancel()
			go startRacer(fallbackCtx, false)

		case res := <-results:
			// 如果拨号成功，直接返回连接
			if res.error == nil {
				return res.Conn, nil
			}
			// 存储拨号结果
			if res.primary {
				primary = res
			} else {
				fallback = res
			}
			// 如果主要和后备地址都已完成且都失败，返回主要地址的错误
			if primary.done && fallback.done {
				return nil, primary.error
			}
			// 如果主要地址拨号失败且定时器还未触发，立即启动后备地址拨号
			if res.primary && fallbackTimer.Stop() {
				// If we were able to stop the timer, that means it
				// was running (hadn't yet started the fallback), but
				// we just got an error on the primary path, so start
				// the fallback immediately (in 0 nanoseconds).
				fallbackTimer.Reset(0)
			}
		}
	}
}

// dialSerial connects to a list of addresses in sequence, returning
// either the first successful connection, or the first error.
// dialSerial 按顺序连接地址列表中的地址，返回第一个成功的连接或第一个错误
// 参数:
// - ctx: 上下文，用于控制连接超时和取消
// - ras: 要尝试连接的地址列表
func (sd *sysDialer) dialSerial(ctx context.Context, ras addrList) (Conn, error) {
	// 保存第一个错误，因为第一个错误通常最具参考价值
	var firstErr error

	// 遍历所有地址尝试连接
	for i, ra := range ras {
		// 检查上下文是否已取消
		select {
		case <-ctx.Done():
			return nil, &OpError{Op: "dial", Net: sd.network, Source: sd.LocalAddr, Addr: ra, Err: mapErr(ctx.Err())}
		default:
		}

		// 使用原始上下文
		dialCtx := ctx

		// 如果上下文设置了截止时间
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			// 计算当前地址可用的部分截止时间
			// 剩余时间会根据剩余待尝试的地址数量进行分配
			partialDeadline, err := partialDeadline(time.Now(), deadline, len(ras)-i)
			if err != nil {
				// 如果已超时，且这是第一个错误，则记录该错误
				if firstErr == nil {
					firstErr = &OpError{Op: "dial", Net: sd.network, Source: sd.LocalAddr, Addr: ra, Err: err}
				}
				break
			}

			// 如果部分截止时间早于总截止时间，创建新的上下文
			if partialDeadline.Before(deadline) {
				var cancel context.CancelFunc
				dialCtx, cancel = context.WithDeadline(ctx, partialDeadline)
				defer cancel()
			}
		}

		// 尝试连接单个地址
		c, err := sd.dialSingle(dialCtx, ra)
		if err == nil {
			// 连接成功，直接返回
			return c, nil
		}
		// 如果这是第一个错误，记录下来
		if firstErr == nil {
			firstErr = err
		}
	}

	// 如果没有任何错误记录（可能是地址列表为空），
	// 返回缺少地址的错误
	if firstErr == nil {
		firstErr = &OpError{Op: "dial", Net: sd.network, Source: nil, Addr: nil, Err: errMissingAddress}
	}
	return nil, firstErr
}

// dialSingle 尝试建立并返回一个到目标地址的单个连接
// 参数:
// - ctx: 上下文，用于控制连接超时和取消
// - ra: 目标远程地址
// 返回:
// - c: 建立的连接
// - err: 错误信息
func (sd *sysDialer) dialSingle(ctx context.Context, ra Addr) (c Conn, err error) {
	// 从上下文中获取网络追踪对象
	trace, _ := ctx.Value(nettrace.TraceKey{}).(*nettrace.Trace)
	if trace != nil {
		// 将远程地址转换为字符串
		raStr := ra.String()
		// 如果设置了连接开始回调，调用它
		if trace.ConnectStart != nil {
			trace.ConnectStart(sd.network, raStr)
		}
		// 如果设置了连接完成回调，在函数返回时调用它
		if trace.ConnectDone != nil {
			defer func() { trace.ConnectDone(sd.network, raStr, err) }()
		}
	}

	// 获取本地地址
	la := sd.LocalAddr

	// 根据远程地址类型选择不同的拨号方法
	switch ra := ra.(type) {
	case *TCPAddr:
		// TCP 地址
		la, _ := la.(*TCPAddr)
		if sd.MultipathTCP() {
			// 如果启用了多路径 TCP，使用 MPTCP 拨号
			c, err = sd.dialMPTCP(ctx, la, ra)
		} else {
			// 否则使用普��� TCP 拨号
			c, err = sd.dialTCP(ctx, la, ra)
		}
	case *UDPAddr:
		// UDP 地址
		la, _ := la.(*UDPAddr)
		c, err = sd.dialUDP(ctx, la, ra)
	case *IPAddr:
		// IP 地址
		la, _ := la.(*IPAddr)
		c, err = sd.dialIP(ctx, la, ra)
	case *UnixAddr:
		// Unix 域套接字地址
		la, _ := la.(*UnixAddr)
		c, err = sd.dialUnix(ctx, la, ra)
	default:
		return nil, &OpError{Op: "dial", Net: sd.network, Source: la, Addr: ra, Err: &AddrError{Err: "unexpected address type", Addr: sd.address}}
	}

	// 如果拨号过程中发生错误，返回带有详细错误信息的 OpError
	if err != nil {
		return nil, &OpError{Op: "dial", Net: sd.network, Source: la, Addr: ra, Err: err} // c is non-nil interface containing nil pointer
	}

	// 返回成功建立的连接
	return c, nil
}

// ListenConfig contains options for listening to an address.
type ListenConfig struct {
	// If Control is not nil, it is called after creating the network
	// connection but before binding it to the operating system.
	//
	// Network and address parameters passed to Control method are not
	// necessarily the ones passed to Listen. For example, passing "tcp" to
	// Listen will cause the Control function to be called with "tcp4" or "tcp6".
	Control func(network, address string, c syscall.RawConn) error

	// KeepAlive specifies the keep-alive period for network
	// connections accepted by this listener.
	//
	// KeepAlive is ignored if KeepAliveConfig.Enable is true.
	//
	// If zero, keep-alive are enabled if supported by the protocol
	// and operating system. Network protocols or operating systems
	// that do not support keep-alive ignore this field.
	// If negative, keep-alive are disabled.
	KeepAlive time.Duration

	// KeepAliveConfig specifies the keep-alive probe configuration
	// for an active network connection, when supported by the
	// protocol and operating system.
	//
	// If KeepAliveConfig.Enable is true, keep-alive probes are enabled.
	// If KeepAliveConfig.Enable is false and KeepAlive is negative,
	// keep-alive probes are disabled.
	KeepAliveConfig KeepAliveConfig

	// If mptcpStatus is set to a value allowing Multipath TCP (MPTCP) to be
	// used, any call to Listen with "tcp(4|6)" as network will use MPTCP if
	// supported by the operating system.
	mptcpStatus mptcpStatus
}

// MultipathTCP reports whether MPTCP will be used.
//
// This method doesn't check if MPTCP is supported by the operating
// system or not.
func (lc *ListenConfig) MultipathTCP() bool {
	return lc.mptcpStatus.get()
}

// SetMultipathTCP directs the [Listen] method to use, or not use, MPTCP,
// if supported by the operating system. This method overrides the
// system default and the GODEBUG=multipathtcp=... setting if any.
//
// If MPTCP is not available on the host or not supported by the client,
// the Listen method will fall back to TCP.
func (lc *ListenConfig) SetMultipathTCP(use bool) {
	lc.mptcpStatus.set(use)
}

// Listen announces on the local network address.
//
// See func Listen for a description of the network and address
// parameters.
//
// 在本地网络地址上创建监听器
//
// 关于 network 和 address 参数的详细说明，请参考 Listen 函数的文档
func (lc *ListenConfig) Listen(ctx context.Context, network, address string) (Listener, error) {
	// 使用默认解析器解析地址，获取可用的地址列表
	addrs, err := DefaultResolver.resolveAddrList(ctx, "listen", network, address, nil)
	if err != nil {
		// 如果解析失败，返回带有详细错误信息的 OpError
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
	}

	// 创建系统监听器对象，包含配置信息和网络参数
	sl := &sysListener{
		ListenConfig: *lc,
		network:      network,
		address:      address,
	}

	var l Listener
	// 获取第一个地址（优先使用 IPv4）
	la := addrs.first(isIPv4)

	// 根据地址类型选择不同的监听方式
	switch la := la.(type) {
	case *TCPAddr:
		// TCP 地址：根据是否启用多路径 TCP 选择不同的监听方法
		if sl.MultipathTCP() {
			l, err = sl.listenMPTCP(ctx, la)
		} else {
			l, err = sl.listenTCP(ctx, la)
		}
	case *UnixAddr:
		// Unix 域套接字地址
		l, err = sl.listenUnix(ctx, la)
	default:
		// 不支持的地址类型，返回错误
		return nil, &OpError{Op: "listen", Net: sl.network, Source: nil, Addr: la, Err: &AddrError{Err: "unexpected address type", Addr: address}}
	}

	// 如果创建监听器过程中发生错误，返回带有详细错误信息的 OpError
	if err != nil {
		return nil, &OpError{Op: "listen", Net: sl.network, Source: nil, Addr: la, Err: err} // l is non-nil interface containing nil pointer
	}

	// 返回创建的监听器
	return l, nil
}

// ListenPacket announces on the local network address.
//
// See func ListenPacket for a description of the network and address
// parameters.
func (lc *ListenConfig) ListenPacket(ctx context.Context, network, address string) (PacketConn, error) {
	addrs, err := DefaultResolver.resolveAddrList(ctx, "listen", network, address, nil)
	if err != nil {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
	}
	sl := &sysListener{
		ListenConfig: *lc,
		network:      network,
		address:      address,
	}
	var c PacketConn
	la := addrs.first(isIPv4)
	switch la := la.(type) {
	case *UDPAddr:
		c, err = sl.listenUDP(ctx, la)
	case *IPAddr:
		c, err = sl.listenIP(ctx, la)
	case *UnixAddr:
		c, err = sl.listenUnixgram(ctx, la)
	default:
		return nil, &OpError{Op: "listen", Net: sl.network, Source: nil, Addr: la, Err: &AddrError{Err: "unexpected address type", Addr: address}}
	}
	if err != nil {
		return nil, &OpError{Op: "listen", Net: sl.network, Source: nil, Addr: la, Err: err} // c is non-nil interface containing nil pointer
	}
	return c, nil
}

// sysListener contains a Listen's parameters and configuration.
type sysListener struct {
	ListenConfig
	network, address string
}

// Listen announces on the local network address.
//
// The network must be "tcp", "tcp4", "tcp6", "unix" or "unixpacket".
//
// For TCP networks, if the host in the address parameter is empty or
// a literal unspecified IP address, Listen listens on all available
// unicast and anycast IP addresses of the local system.
// To only use IPv4, use network "tcp4".
// The address can use a host name, but this is not recommended,
// because it will create a listener for at most one of the host's IP
// addresses.
// If the port in the address parameter is empty or "0", as in
// "127.0.0.1:" or "[::1]:0", a port number is automatically chosen.
// The [Addr] method of [Listener] can be used to discover the chosen
// port.
//
// See func [Dial] for a description of the network and address
// parameters.
//
// Listen uses context.Background internally; to specify the context, use
// [ListenConfig.Listen].
//
// 在本地网络地址上创建并返回一个监听器
//
// network 参数必须是 "tcp"、"tcp4"、"tcp6"、"unix" 或 "unixpacket"
//
// 对于 TCP 网络：
// - 如果 address 参数中的主机为空或未指定 IP 地址，将监听本地系统所有可用的单播和任播 IP 地址
// - 如果只想使用 IPv4，需要指定 network 为 "tcp4"
// - address 可以使用主机名，但不推荐，因为这只会为主机的其中一个 IP 地址创建监听器
// - 如果 address 参数中的端口为空或 "0"（如 "127.0.0.1:" 或 "[::1]:0"），将自动选择一个端口号
// - 可以通过监听器的 Addr 方法获取实际选择的端口号
//
// 该函数内部使用 context.Background()；如果需要指定上下文，请使用 ListenConfig.Listen
func Listen(network, address string) (Listener, error) {
	// 创建一个默认的 ListenConfig 配置对象
	var lc ListenConfig
	// 使用默认的后台上下文调用 ListenConfig.Listen 方法
	// 该方法会实际创建并返回一个网络监听器
	return lc.Listen(context.Background(), network, address)
}

// ListenPacket announces on the local network address.
//
// The network must be "udp", "udp4", "udp6", "unixgram", or an IP
// transport. The IP transports are "ip", "ip4", or "ip6" followed by
// a colon and a literal protocol number or a protocol name, as in
// "ip:1" or "ip:icmp".
//
// For UDP and IP networks, if the host in the address parameter is
// empty or a literal unspecified IP address, ListenPacket listens on
// all available IP addresses of the local system except multicast IP
// addresses.
// To only use IPv4, use network "udp4" or "ip4:proto".
// The address can use a host name, but this is not recommended,
// because it will create a listener for at most one of the host's IP
// addresses.
// If the port in the address parameter is empty or "0", as in
// "127.0.0.1:" or "[::1]:0", a port number is automatically chosen.
// The LocalAddr method of [PacketConn] can be used to discover the
// chosen port.
//
// See func [Dial] for a description of the network and address
// parameters.
//
// ListenPacket uses context.Background internally; to specify the context, use
// [ListenConfig.ListenPacket].
func ListenPacket(network, address string) (PacketConn, error) {
	var lc ListenConfig
	return lc.ListenPacket(context.Background(), network, address)
}
