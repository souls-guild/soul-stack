//go:build linux

package soulprint

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// osVersion is unused on Linux (version comes from /etc/os-release
// VERSION_ID); defined only to keep the platform-branch signature uniform.
func osVersion(_ context.Context) string { return "" }

// kernelInfo — kernel version via uname(2). release is the bare version
// (5.15.0), version is the same with the distro suffix from uname.release
// (5.15.0-101-generic).
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

// baseKernelRelease strips the distro suffix: "5.15.0-101-generic" → "5.15.0".
// Takes the part before the first hyphen (linux kernel versioning: X.Y.Z-suffix).
func baseKernelRelease(rel string) string {
	if i := strings.IndexByte(rel, '-'); i > 0 {
		return rel[:i]
	}
	return rel
}

// cpuInfo — count from the number of "processor" entries in /proc/cpuinfo
// (logical CPUs); model/vendor from the first entry (model name / vendor_id).
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

// memoryInfo — total/available/swap from /proc/meminfo. Values there are in
// KB — converted to MB (integer division). available = MemAvailable (a more
// accurate estimate than MemFree); swap = SwapTotal.
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

// parseMeminfoKB extracts the number from a string like "  16384256 kB". 0 on garbage input.
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

// charsToString builds a string from a C int8/uint8 array (Utsname fields),
// truncating at the first NUL. The element type differs across arches (int8
// on amd64, uint8 on arm64) — hence generic over signed/unsigned byte.
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
