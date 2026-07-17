package utilization

import (
	"strconv"
	"strings"
)

// parseCPUStatLine парсит агрегатную строку `cpu ...` /proc/stat в CPUSample.
// Total суммирует только user..steal (idx 0..7); guest/guest_nice (8,9) ядро уже
// учло в user/nice — повторный счёт завысил бы Total на KVM-хостах и занизил cpu%.
// Idle = idle+iowait (idx 3,4). Мусор/короткая строка → zero-value (best-effort).
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
