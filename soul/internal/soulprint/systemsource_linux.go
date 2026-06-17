//go:build linux

package soulprint

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// osVersion на Linux не используется (version берётся из /etc/os-release
// VERSION_ID); определён только для единообразия сигнатуры platform-веток.
func osVersion(_ context.Context) string { return "" }

// kernelInfo — версия ядра через uname(2). release — голая версия (5.15.0),
// version — то же с дистрибутив-suffix-ом из uname.release (5.15.0-101-generic).
func kernelInfo(_ context.Context) KernelInfo {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return KernelInfo{}
	}
	rel := charsToString(u.Release[:])
	return KernelInfo{
		Version: rel,
		Release: baseKernelRelease(rel),
	}
}

// baseKernelRelease обрезает дистрибутив-suffix: "5.15.0-101-generic" → "5.15.0".
// Берём часть до первого дефиса (linux kernel versioning: X.Y.Z-suffix).
func baseKernelRelease(rel string) string {
	if i := strings.IndexByte(rel, '-'); i > 0 {
		return rel[:i]
	}
	return rel
}

// cpuInfo — count из числа "processor"-записей /proc/cpuinfo (logical CPUs);
// model/vendor из первой записи (model name / vendor_id).
func cpuInfo(_ context.Context) CPUInfo {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return CPUInfo{}
	}
	var info CPUInfo
	for _, line := range strings.Split(string(raw), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "processor":
			info.Count++
		case "model name":
			if info.Model == "" {
				info.Model = val
			}
		case "vendor_id":
			if info.Vendor == "" {
				info.Vendor = val
			}
		}
	}
	return info
}

// memoryInfo — total/available/swap из /proc/meminfo. Значения там в КБ —
// конвертируем в МБ (целочисленно). available = MemAvailable (более точная
// оценка, чем MemFree); swap = SwapTotal.
func memoryInfo(_ context.Context) MemoryInfo {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemoryInfo{}
	}
	var info MemoryInfo
	for _, line := range strings.Split(string(raw), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kb := parseMeminfoKB(val)
		switch strings.TrimSpace(key) {
		case "MemTotal":
			info.TotalMB = kb / 1024
		case "MemAvailable":
			info.AvailableMB = kb / 1024
		case "SwapTotal":
			info.SwapMB = kb / 1024
		}
	}
	return info
}

// parseMeminfoKB извлекает число из строки вида "  16384256 kB". 0 при мусоре.
func parseMeminfoKB(val string) int64 {
	fields := strings.Fields(val)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// charsToString собирает строку из C-массива int8/uint8 (Utsname поля),
// обрезая по первому NUL. На разных arch тип элемента отличается (int8 на
// amd64, uint8 на arm64) — поэтому generic по signed/unsigned byte.
func charsToString[T ~int8 | ~uint8](ca []T) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
