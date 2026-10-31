//go:build !darwin && !linux

package config

func memoryBytes() uint64 { return 0 }
