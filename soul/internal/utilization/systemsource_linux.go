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

// Network — sum of monotonic /proc/net/dev counters over physical interfaces
// (lo and virtual-prefix ifaces skipped). Unparsable line → skip; total failure
// → zero NetSample.
func (systemSource) Network(context.Context) NetSample {
	var s NetSample
	for _, line := range strings.Split(readFile("/proc/net/dev"), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // header rows have no colon
		}
		name = strings.TrimSpace(name)
		if virtualIface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 { // 8 receive + 8 transmit fields
			continue
		}
		s.RxBytes += parseUint(f[0])
		s.TxBytes += parseUint(f[8])
		s.ErrDrops += parseUint(f[2]) + parseUint(f[3]) + parseUint(f[10]) + parseUint(f[11])
	}
	return s
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
		inodesUsed, inodesTotal := inodeUsage(st.Files, st.Ffree)
		out = append(out, Disk{
			Mount:       mount,
			UsedMB:      (int64(st.Blocks) - int64(st.Bfree)) * frsize / bytesPerMB,
			TotalMB:     total,
			InodesUsed:  inodesUsed,
			InodesTotal: inodesTotal,
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

// virtualIfacePrefixes — name prefixes of non-physical interfaces (bridges,
// container veths, tunnels, overlays) excluded from throughput aggregation.
var virtualIfacePrefixes = []string{
	"docker", "veth", "br-", "virbr", "tap", "tun", "cni", "flannel",
	"kube", "vnet", "vxlan", "dummy", "ifb", "bond",
}

// virtualIface — true for the loopback and any virtual-prefix interface.
func virtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
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

// inodeUsage — inode used/total from statvfs f_files/f_ffree. files==0 → the FS
// doesn't report inodes; ffree>files → inconsistent counters (drvfs/9p on WSL
// report a fake tiny files with a huge ffree) → "no inode data" (0,0), never a
// negative used.
func inodeUsage(files, ffree uint64) (used, total int64) {
	if files == 0 || ffree > files {
		return 0, 0
	}
	return int64(files - ffree), int64(files)
}

// parseUint — a monotonic counter field; 0 on garbage (best-effort).
func parseUint(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
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
