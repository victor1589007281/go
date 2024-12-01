// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || js || wasip1 || windows

package net

import (
	"context"
	"internal/poll"
	"net/netip"
	"runtime"
	"syscall"
	_ "unsafe" // for linkname
)

// probe probes IPv4, IPv6 and IPv4-mapped IPv6 communication
// capabilities which are controlled by the IPV6_V6ONLY socket option
// and kernel configuration.
//
// Should we try to use the IPv4 socket interface if we're only
// dealing with IPv4 sockets? As long as the host system understands
// IPv4-mapped IPv6, it's okay to pass IPv4-mapped IPv6 addresses to
// the IPv6 interface. That simplifies our code and is most
// general. Unfortunately, we need to run on kernels built without
// IPv6 support too. So probe the kernel to figure it out.
func (p *ipStackCapabilities) probe() {
	switch runtime.GOOS {
	case "js", "wasip1":
		// Both ipv4 and ipv6 are faked; see net_fake.go.
		p.ipv4Enabled = true
		p.ipv6Enabled = true
		p.ipv4MappedIPv6Enabled = true
		return
	}

	s, err := sysSocket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	switch err {
	case syscall.EAFNOSUPPORT, syscall.EPROTONOSUPPORT:
	case nil:
		poll.CloseFunc(s)
		p.ipv4Enabled = true
	}
	var probes = []struct {
		laddr TCPAddr
		value int
	}{
		// IPv6 communication capability
		{laddr: TCPAddr{IP: ParseIP("::1")}, value: 1},
		// IPv4-mapped IPv6 address communication capability
		{laddr: TCPAddr{IP: IPv4(127, 0, 0, 1)}, value: 0},
	}
	switch runtime.GOOS {
	case "dragonfly", "openbsd":
		// The latest DragonFly BSD and OpenBSD kernels don't
		// support IPV6_V6ONLY=0. They always return an error
		// and we don't need to probe the capability.
		probes = probes[:1]
	}
	for i := range probes {
		s, err := sysSocket(syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
		if err != nil {
			continue
		}
		defer poll.CloseFunc(s)
		syscall.SetsockoptInt(s, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, probes[i].value)
		sa, err := probes[i].laddr.sockaddr(syscall.AF_INET6)
		if err != nil {
			continue
		}
		if err := syscall.Bind(s, sa); err != nil {
			continue
		}
		if i == 0 {
			p.ipv6Enabled = true
		} else {
			p.ipv4MappedIPv6Enabled = true
		}
	}
}

// favoriteAddrFamily returns the appropriate address family for the
// given network, laddr, raddr and mode.
//
// If mode indicates "listen" and laddr is a wildcard, we assume that
// the user wants to make a passive-open connection with a wildcard
// address family, both AF_INET and AF_INET6, and a wildcard address
// like the following:
//
//   - A listen for a wildcard communication domain, "tcp" or
//     "udp", with a wildcard address: If the platform supports
//     both IPv6 and IPv4-mapped IPv6 communication capabilities,
//     or does not support IPv4, we use a dual stack, AF_INET6 and
//     IPV6_V6ONLY=0, wildcard address listen. The dual stack
//     wildcard address listen may fall back to an IPv6-only,
//     AF_INET6 and IPV6_V6ONLY=1, wildcard address listen.
//     Otherwise we prefer an IPv4-only, AF_INET, wildcard address
//     listen.
//
//   - A listen for a wildcard communication domain, "tcp" or
//     "udp", with an IPv4 wildcard address: same as above.
//
//   - A listen for a wildcard communication domain, "tcp" or
//     "udp", with an IPv6 wildcard address: same as above.
//
//   - A listen for an IPv4 communication domain, "tcp4" or "udp4",
//     with an IPv4 wildcard address: We use an IPv4-only, AF_INET,
//     wildcard address listen.
//
//   - A listen for an IPv6 communication domain, "tcp6" or "udp6",
//     with an IPv6 wildcard address: We use an IPv6-only, AF_INET6
//     and IPV6_V6ONLY=1, wildcard address listen.
//
// Otherwise guess: If the addresses are IPv4 then returns AF_INET,
// or else returns AF_INET6. It also returns a boolean value what
// designates IPV6_V6ONLY option.
//
// Note that the latest DragonFly BSD and OpenBSD kernels allow
// neither "net.inet6.ip6.v6only=1" change nor IPPROTO_IPV6 level
// IPV6_V6ONLY socket option setting.
//
// favoriteAddrFamily should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/database64128/tfo-go/v2
//   - github.com/metacubex/tfo-go
//   - github.com/sagernet/tfo-go
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname favoriteAddrFamily
func favoriteAddrFamily(network string, laddr, raddr sockaddr, mode string) (family int, ipv6only bool) {
	switch network[len(network)-1] {
	case '4':
		return syscall.AF_INET, false
	case '6':
		return syscall.AF_INET6, true
	}

	if mode == "listen" && (laddr == nil || laddr.isWildcard()) {
		if supportsIPv4map() || !supportsIPv4() {
			return syscall.AF_INET6, false
		}
		if laddr == nil {
			return syscall.AF_INET, false
		}
		return laddr.family(), false
	}

	if (laddr == nil || laddr.family() == syscall.AF_INET) &&
		(raddr == nil || raddr.family() == syscall.AF_INET) {
		return syscall.AF_INET, false
	}
	return syscall.AF_INET6, false
}

// internetSocket 创建一个网络套接字
//
// 参数说明：
// - ctx: 上下文，用于控制操作的生命周期
// - net: 网络类型（如 "tcp"、"tcp4"、"tcp6"）
// - laddr: 本地地址
// - raddr: 远程地址
// - sotype: 套接字类型（如 SOCK_STREAM 用于 TCP）
// - proto: 协议号
// - mode: 操作模式（"dial" 或 "listen"）
// - ctrlCtxFn: 用于控制底层连接的回调函数
func internetSocket(ctx context.Context, net string, laddr, raddr sockaddr, sotype, proto int, mode string, ctrlCtxFn func(context.Context, string, string, syscall.RawConn) error) (fd *netFD, err error) {
	// 特殊平台处理：在某些操作系统上，如果是拨号模式且远程地址是通配符地址，
	// 则将远程地址转换为本地地址
	switch runtime.GOOS {
	case "aix", "windows", "openbsd", "js", "wasip1":
		if mode == "dial" && raddr.isWildcard() {
			raddr = raddr.toLocal(net)
		}
	}

	// 根据网络类型、本地地址、远程地址和操作模式选择合适的地址族
	// family 可能是 AF_INET（IPv4）或 AF_INET6（IPv6）
	// ipv6only 表示是否仅使用 IPv6
	family, ipv6only := favoriteAddrFamily(net, laddr, raddr, mode)

	// 创建并返回实际的套接字
	return socket(ctx, net, family, sotype, proto, ipv6only, laddr, raddr, ctrlCtxFn)
}

// ipToSockaddrInet4 将 IP 地址和端口号转换为 IPv4 的系统套接字地址结构
//
// 参数说明：
// - ip: IP 地址，可以是 nil 或空，此时会使用 IPv4zero（0.0.0.0）
// - port: 端口号
//
// 返回值：
// - syscall.SockaddrInet4: IPv4 套接字地址结构
// - error: 如果输入的 IP 地址不是合法的 IPv4 地址，则返回错误
func ipToSockaddrInet4(ip IP, port int) (syscall.SockaddrInet4, error) {
	// 如果 IP 地址为空，使用 IPv4 零地址（0.0.0.0）
	if len(ip) == 0 {
		ip = IPv4zero
	}
	// 将 IP 转换为 4 字节的 IPv4 地址
	// 如果输入的不是 IPv4 地址，To4() 将返回 nil
	ip4 := ip.To4()
	if ip4 == nil {
		return syscall.SockaddrInet4{}, &AddrError{Err: "non-IPv4 address", Addr: ip.String()}
	}
	// 创建套接字地址结构并设置端口号
	sa := syscall.SockaddrInet4{Port: port}
	// 将 4 字节的 IPv4 地址复制到套接字地址结构中
	copy(sa.Addr[:], ip4)
	return sa, nil
}

func ipToSockaddrInet6(ip IP, port int, zone string) (syscall.SockaddrInet6, error) {
	// In general, an IP wildcard address, which is either
	// "0.0.0.0" or "::", means the entire IP addressing
	// space. For some historical reason, it is used to
	// specify "any available address" on some operations
	// of IP node.
	//
	// When the IP node supports IPv4-mapped IPv6 address,
	// we allow a listener to listen to the wildcard
	// address of both IP addressing spaces by specifying
	// IPv6 wildcard address.
	if len(ip) == 0 || ip.Equal(IPv4zero) {
		ip = IPv6zero
	}
	// We accept any IPv6 address including IPv4-mapped
	// IPv6 address.
	ip6 := ip.To16()
	if ip6 == nil {
		return syscall.SockaddrInet6{}, &AddrError{Err: "non-IPv6 address", Addr: ip.String()}
	}
	sa := syscall.SockaddrInet6{Port: port, ZoneId: uint32(zoneCache.index(zone))}
	copy(sa.Addr[:], ip6)
	return sa, nil
}

// ipToSockaddr should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/database64128/tfo-go/v2
//   - github.com/metacubex/tfo-go
//   - github.com/sagernet/tfo-go
//
// ipToSockaddr 将 IP 地址转换为系统底层的 socket 地址结构
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
// 参数说明：
// - family: 地址族（syscall.AF_INET 用于 IPv4，syscall.AF_INET6 用于 IPv6）
// - ip: 要转换的 IP 地址
// - port: 端口号
// - zone: IPv6 的区域标识符（对于 IPv4 地址，此参数会被忽略）
//
// 返回值：
// - syscall.Sockaddr: 转换后的系统 socket 地址结构
// - error: 转换过程中可能发生的错误
//
// 注意：此函数虽为内部实现细节，但因历史原因被多个外部包通过 linkname 直接使用，
// 因此函数签名不能改变。参考 go.dev/issue/67401
//
//go:linkname ipToSockaddr
func ipToSockaddr(family int, ip IP, port int, zone string) (syscall.Sockaddr, error) {
	// 根据地址族类型选择不同的处理方式
	switch family {
	case syscall.AF_INET:
		// 处理 IPv4 地址
		sa, err := ipToSockaddrInet4(ip, port)
		if err != nil {
			return nil, err
		}
		// 返回 IPv4 socket 地址结构的指针
		return &sa, nil
	case syscall.AF_INET6:
		// 处理 IPv6 地址，zone 参数用于指定 IPv6 作用域
		sa, err := ipToSockaddrInet6(ip, port, zone)
		if err != nil {
			return nil, err
		}
		// 返回 IPv6 socket 地址结构的指针
		return &sa, nil
	}
	// 如果地址族既不是 IPv4 也不是 IPv6，返回错误
	return nil, &AddrError{Err: "invalid address family", Addr: ip.String()}
}

func addrPortToSockaddrInet4(ap netip.AddrPort) (syscall.SockaddrInet4, error) {
	// ipToSockaddrInet4 has special handling here for zero length slices.
	// We do not, because netip has no concept of a generic zero IP address.
	addr := ap.Addr()
	if !addr.Is4() {
		return syscall.SockaddrInet4{}, &AddrError{Err: "non-IPv4 address", Addr: addr.String()}
	}
	sa := syscall.SockaddrInet4{
		Addr: addr.As4(),
		Port: int(ap.Port()),
	}
	return sa, nil
}

func addrPortToSockaddrInet6(ap netip.AddrPort) (syscall.SockaddrInet6, error) {
	// ipToSockaddrInet6 has special handling here for zero length slices.
	// We do not, because netip has no concept of a generic zero IP address.
	//
	// addr is allowed to be an IPv4 address, because As16 will convert it
	// to an IPv4-mapped IPv6 address.
	// The error message is kept consistent with ipToSockaddrInet6.
	addr := ap.Addr()
	if !addr.IsValid() {
		return syscall.SockaddrInet6{}, &AddrError{Err: "non-IPv6 address", Addr: addr.String()}
	}
	sa := syscall.SockaddrInet6{
		Addr:   addr.As16(),
		Port:   int(ap.Port()),
		ZoneId: uint32(zoneCache.index(addr.Zone())),
	}
	return sa, nil
}
