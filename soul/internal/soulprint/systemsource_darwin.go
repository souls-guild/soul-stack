//go:build darwin

package soulprint

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// systemsource_darwin — best-effort сбор фактов на macOS (dev-машина).
// Значения через sysctl / sw_vers; при недоступности утилит — zero-value.
// Это не production-target (ADR-018: основная цель — Linux), поэтому покрытие
// узкое и без жёсткой обработки ошибок.

func osVersion(ctx context.Context) string {
	return strings.TrimSpace(runSysctlCmd(ctx, "sw_vers", "-productVersion"))
}

func kernelInfo(ctx context.Context) KernelInfo {
	rel := strings.TrimSpace(runSysctlCmd(ctx, "uname", "-r"))
	return KernelInfo{Version: rel, Release: rel}
}

func cpuInfo(ctx context.Context) CPUInfo {
	info := CPUInfo{Count: int32(runtime.NumCPU())}
	if n, err := strconv.Atoi(strings.TrimSpace(runSysctl(ctx, "hw.logicalcpu"))); err == nil && n > 0 {
		info.Count = int32(n)
	}
	info.Model = strings.TrimSpace(runSysctl(ctx, "machdep.cpu.brand_string"))
	info.Vendor = strings.TrimSpace(runSysctl(ctx, "machdep.cpu.vendor"))
	if info.Vendor == "" && strings.Contains(strings.ToLower(info.Model), "apple") {
		info.Vendor = "Apple"
	}
	return info
}

func memoryInfo(ctx context.Context) MemoryInfo {
	var info MemoryInfo
	if b, err := strconv.ParseInt(strings.TrimSpace(runSysctl(ctx, "hw.memsize")), 10, 64); err == nil {
		info.TotalMB = b / (1024 * 1024)
	}
	return info
}

// runSysctl — `sysctl -n <key>`, "" при ошибке.
func runSysctl(ctx context.Context, key string) string {
	return runSysctlCmd(ctx, "sysctl", "-n", key)
}

// runSysctlCmd — обёртка над одной командой; "" при любой ошибке запуска.
func runSysctlCmd(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
