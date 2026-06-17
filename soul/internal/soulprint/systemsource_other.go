//go:build !linux && !darwin

package soulprint

import (
	"context"
	"runtime"
)

// systemsource_other — fallback для платформ вне Linux/macOS (windows и пр.).
// Сбор системных фактов не реализован (ADR-018: основная цель — Linux);
// возвращаем zero-value, кроме count из runtime.NumCPU (всегда доступен).

func osVersion(_ context.Context) string { return "" }

func kernelInfo(_ context.Context) KernelInfo { return KernelInfo{} }

func cpuInfo(_ context.Context) CPUInfo {
	return CPUInfo{Count: int32(runtime.NumCPU())}
}

func memoryInfo(_ context.Context) MemoryInfo { return MemoryInfo{} }
