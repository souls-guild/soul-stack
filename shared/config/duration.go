package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MaxDurationDays — верхняя граница для convention `<N>d`, исключающая
// int64-overflow при `days * 24h`. Эквивалент `math.MaxInt64 / int64(24h)`
// ≈ 106 751 (≈ 292 года). Любой Go-`time.ParseDuration` уже сам отвергает
// значения, не помещающиеся в int64-наносекунды, поэтому отдельная проверка
// нужна только в нашей надстройке для `d`.
var MaxDurationDays = int(math.MaxInt64 / int64(24*time.Hour))

// ParseDuration реализует convention `duration` Soul Stack:
// Go-`time.ParseDuration` (`1s`/`500ms`/`1h30m`) **или** суффикс `<N>d` для
// дней (`30d` = 720h). Композитная форма `1d2h` не поддерживается — explicit
// fail, потому что convention этого не обещает (docs/keeper/config.md →
// «Конвенции типов»). Знак `+`/`-` перед `<N>d` также отвергается, чтобы
// convention оставалась симметричной с Go-duration (отрицательные duration
// в конфиге смысла не имеют, а явный `+` — лишний шум). Верхняя граница для
// `<N>d` — [MaxDurationDays] (защита от int64-overflow).
//
// Единая точка входа для всех потребителей convention (semantic-валидация
// keeper.yml, Reaper-runner и т.п.). Не дублировать локальными копиями.
func ParseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		body := s[:len(s)-1]
		if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
			return 0, fmt.Errorf("invalid <N>d duration %q: sign prefix not allowed", s)
		}
		// Принимаем только беззнаковое целое перед `d`; защищает от
		// композитной формы вроде `1h2d` (которая не заканчивается на `d`,
		// но на всякий случай) и от примесей внутри.
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
