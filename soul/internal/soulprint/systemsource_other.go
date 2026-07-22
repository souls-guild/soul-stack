//go:build !linux && !darwin

package soulprint

import (
	"context"
	"runtime"
)

// systemsource_other is the fallback for platforms outside Linux/macOS
// (windows, etc). System-fact collection isn't implemented (ADR-018's primary
// target is Linux); returns zero-value except count from runtime.NumCPU
// (always available).

func osVersion(_ context.Context) string { return "" }

func kernelInfo(_ context.Context) KernelInfo { return KernelInfo{} }

func cpuInfo(_ context.Context) CPUInfo {
	return CPUInfo{Count: int32(runtime.NumCPU())}
}

func memoryInfo(_ context.Context) MemoryInfo { return MemoryInfo{} }
