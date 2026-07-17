package utilization

import (
	"strconv"
	"strings"
)

// parseCPUStatLine parses the aggregate `cpu ...` line of /proc/stat into a CPUSample.
// Total sums only user..steal (idx 0..7); guest/guest_nice (8,9) are already
// accounted for by the kernel in user/nice — double-counting would inflate Total on
// KVM hosts and understate cpu%. Idle = idle+iowait (idx 3,4). Garbage/short line → zero-value (best-effort).
func parseCPUStatLine(line string) CPUSample {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "cpu" {
		return CPUSample{}
	}
	var s CPUSample
	for i, f := range fields[1:] {
		if i > 7 {
			break
		}
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		s.Total += n
		if i == 3 || i == 4 {
			s.Idle += n
		}
	}
	return s
}
