package config

import "golang.org/x/sys/unix"

func memoryBytes() uint64 {
	n, _ := unix.SysctlUint64("hw.memsize")
	return n
}
