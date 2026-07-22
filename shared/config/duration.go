package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MaxDurationDays is the upper bound for the `<N>d` convention, ruling out
// int64 overflow in `days * 24h`. Equivalent to `math.MaxInt64 / int64(24h)`
// ≈ 106 751 (≈ 292 years). Go `time.ParseDuration` itself already rejects values
// that don't fit into int64 nanoseconds, so a separate check is needed only in
// our `d` extension.
var MaxDurationDays = int(math.MaxInt64 / int64(24*time.Hour))

// ParseDuration implements the Soul Stack `duration` convention: Go
// `time.ParseDuration` (`1s`/`500ms`/`1h30m`) **or** a `<N>d` suffix for days
// (`30d` = 720h). The composite form `1d2h` is not supported — explicit fail,
// because the convention does not promise it (docs/keeper/config.md → "Type
// conventions"). A `+`/`-` sign before `<N>d` is also rejected, to keep the
// convention symmetric with Go duration (negative durations make no sense in a
// config, and an explicit `+` is just noise). The upper bound for `<N>d` is
// [MaxDurationDays] (int64-overflow guard).
//
// The single entry point for all consumers of the convention (keeper.yml semantic
// validation, the Reaper runner, etc.). Do not duplicate with local copies.
func ParseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		body := s[:len(s)-1]
		if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
			return 0, fmt.Errorf("invalid <N>d duration %q: sign prefix not allowed", s)
		}
		// Accept only an unsigned integer before `d`; guards against a composite
		// form like `1h2d` (which doesn't end in `d`, but just in case) and
		// against junk inside.
		days, err := strconv.Atoi(body)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid <N>d duration %q", s)
		}
		if days > MaxDurationDays {
			return 0, fmt.Errorf("invalid <N>d duration %q: value too large; max is ~292 years (%d days)", s, MaxDurationDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
