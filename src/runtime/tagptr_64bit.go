// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || arm64 || loong64 || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x || wasm

package runtime

import (
	"internal/goarch"
	"internal/goos"
	"unsafe"
)

const (
	// addrBits is the number of bits needed to represent a virtual address.
	//
	// See heapAddrBits for a table of address space sizes on
	// various architectures. 48 bits is enough for all
	// architectures except s390x.
	//
	// On AMD64, virtual addresses are 48-bit (or 57-bit) numbers sign extended to 64.
	// We shift the address left 16 to eliminate the sign extended part and make
	// room in the bottom for the count.
	//
	// On s390x, virtual addresses are 64-bit. There's not much we
	// can do about this, so we just hope that the kernel doesn't
	// get to really high addresses and panic if it does.
	addrBits = 48

	// In addition to the 16 bits taken from the top, we can take 3 from the
	// bottom, because node must be pointer-aligned, giving a total of 19 bits
	// of count.
	tagBits = 64 - addrBits + 3

	// On AIX, 64-bit addresses are split into 36-bit segment number and 28-bit
	// offset in segment.  Segment numbers in the range 0x0A0000000-0x0AFFFFFFF(LSA)
	// are available for mmap.
	// We assume all tagged addresses are from memory allocated with mmap.
	// We use one bit to distinguish between the two ranges.
	aixAddrBits = 57
	aixTagBits  = 64 - aixAddrBits + 3

	// riscv64 SV57 mode gives 56 bits of userspace VA.
	// tagged pointer code supports it,
	// but broader support for SV57 mode is incomplete,
	// and there may be other issues (see #54104).
	riscv64AddrBits = 56
	riscv64TagBits  = 64 - riscv64AddrBits + 3
)

// The number of bits stored in the numeric tag of a taggedPointer
const taggedPointerBits = (goos.IsAix * aixTagBits) + (goarch.IsRiscv64 * riscv64TagBits) + ((1 - goos.IsAix) * (1 - goarch.IsRiscv64) * tagBits)

// taggedPointerPack created a taggedPointer from a pointer and a tag.
// Tag bits that don't fit in the result are discarded.
// 将指针和标记值打包成一个标记指针。
// 超出可用位数的标记位会被丢弃。
func taggedPointerPack(ptr unsafe.Pointer, tag uintptr) taggedPointer {
	// Check if the operating system is AIX
	// 检查是否为AIX操作系统
	if GOOS == "aix" {
		// Ensure the architecture is ppc64, otherwise throw an error
		// 确保架构为ppc64，否则抛出错误
		if GOARCH != "ppc64" {
			throw("check this code for aix on non-ppc64")
		}
		// Shift the pointer left by the number of bits not used by the address
		// and combine it with the tag, ensuring the tag fits within the allowed bits
		// 将指针左移未被地址使用的位数，并与标记值组合
		// 确保标记值在允许的位数范围内
		return taggedPointer(uint64(uintptr(ptr))<<(64-aixAddrBits) | uint64(tag&(1<<aixTagBits-1)))
	}
	// Check if the architecture is RISC-V 64-bit
	// 检查是否为RISC-V 64位架构
	if GOARCH == "riscv64" {
		// Similar to AIX, shift the pointer and combine with the tag
		// 类似于AIX的处理方式，移位指针并与标记值组合
		return taggedPointer(uint64(uintptr(ptr))<<(64-riscv64AddrBits) | uint64(tag&(1<<riscv64TagBits-1)))
	}
	// Default case for other architectures
	// Shift the pointer and combine with the tag
	// 其他架构的默认处理方式
	// 移位指针并与标记值组合
	return taggedPointer(uint64(uintptr(ptr))<<(64-addrBits) | uint64(tag&(1<<tagBits-1)))
}

// Pointer returns the pointer from a taggedPointer.
func (tp taggedPointer) pointer() unsafe.Pointer {
	if GOARCH == "amd64" {
		// amd64 systems can place the stack above the VA hole, so we need to sign extend
		// val before unpacking.
		return unsafe.Pointer(uintptr(int64(tp) >> tagBits << 3))
	}
	if GOOS == "aix" {
		return unsafe.Pointer(uintptr((tp >> aixTagBits << 3) | 0xa<<56))
	}
	if GOARCH == "riscv64" {
		return unsafe.Pointer(uintptr(tp >> riscv64TagBits << 3))
	}
	return unsafe.Pointer(uintptr(tp >> tagBits << 3))
}

// Tag returns the tag from a taggedPointer.
func (tp taggedPointer) tag() uintptr {
	return uintptr(tp & (1<<taggedPointerBits - 1))
}
