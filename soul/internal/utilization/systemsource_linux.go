//go:build linux

package utilization

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// systemSource — production [Source] on Linux: /proc/{loadavg,stat,meminfo,
// uptime,mounts} + statfs(2). Best-effort: any read failure → zero-value.
type systemSource struct{}

// NewSystemSource builds a production Source over the real /proc + statfs.
func NewSystemSource() Source { return systemSource{} }

func (systemSource) Load(context.Context) LoadAvg {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) < 3 {
		return LoadAvg{}
	}
	return LoadAvg{One: parseFloat(f[0]), Five: parseFloat(f[1]), Fifteen: parseFloat(f[2])}
}

// CPUSample — the aggregate `cpu ` line of /proc/stat, parsed by [parseCPUStatLine].
func (systemSource) CPUSample(context.Context) CPUSample {
	for _, line := range strings.Split(readFile("/proc/stat"), "\n") {
		if strings.HasPrefix(line, "cpu ") {
			return parseCPUStatLine(line)
		}
	}
	return CPUSample{}
}

func (systemSource) Memory(context.Context) MemInfo {
	var total, avail, swapTotal, swapFree int64
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kb := parseFirstInt(val)
		switch strings.TrimSpace(key) {
		case "MemTotal":
			total = kb
		case "MemAvailable":
			avail = kb
		case "SwapTotal":
			swapTotal = kb
		case "SwapFree":
			swapFree = kb
		}
	}
	return MemInfo{UsedMB: (total - avail) / 1024, TotalMB: total / 1024, SwapUsedMB: (swapTotal - swapFree) / 1024}
}

func (systemSource) Uptime(context.Context) int64 {
	f := strings.Fields(readFile("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	return int64(parseFloat(f[0]))
}

// Disks — mount points from /proc/mounts, minus virtual fstypes, deduped by
// mountpoint; usage via statfs(2). total==0 → skip.
func (systemSource) Disks(context.Context) []Disk {
	var out []Disk
	seen := make(map[string]bool)
	for _, line := range strings.Split(readFile("/proc/mounts"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mount, fstype := fields[1], fields[2]
		if virtualFS(fstype) || seen[mount] {
			continue
		}
		seen[mount] = true
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) != nil {
			continue
		}
		// f_blocks/f_bfree are expressed in f_frsize units (like df); Bsize is the
		// fallback if Frsize is not populated.
		frsize := int64(st.Frsize)
		if frsize == 0 {
			frsize = int64(st.Bsize)
		}
		total := int64(st.Blocks) * frsize / bytesPerMB
		if total == 0 {
			continue
		}
		out = append(out, Disk{
			Mount:   mount,
			UsedMB:  (int64(st.Blocks) - int64(st.Bfree)) * frsize / bytesPerMB,
			TotalMB: total,
		})
	}
	return out
}

const bytesPerMB = 1024 * 1024

// virtualFSTypes — pseudo-filesystems that don't occupy physical storage (plus all
// fuse.* via a prefix check).
var virtualFSTypes = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "proc": true, "sysfs": true, "cgroup": true,
	"cgroup2": true, "overlay": true, "squashfs": true, "devpts": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true, "bpf": true,
	"autofs": true, "ramfs": true, "nsfs": true, "fusectl": true, "configfs": true,
	"hugetlbfs": true,
}

func virtualFS(fstype string) bool {
	return strings.HasPrefix(fstype, "fuse.") || virtualFSTypes[fstype]
}

// readFile — os.ReadFile, "" on any error (best-effort).
func readFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parseFirstInt — the first number from a string like "  16384 kB". 0 on garbage.
func parseFirstInt(s string) int64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
