package harness

import "testing"

// TestApplySettled пиннит решающую логику WaitApplySuccess без live-инфры:
// главный регресс — NIM-45-гонка (только keeper-success при apply-в-полёте НЕ
// завершает прогон) и keeper-only (брекет снят → завершает без ожидания soul).
func TestApplySettled(t *testing.T) {
	cases := []struct {
		name     string
		rows     []ApplyRunRow
		inFlight bool
		wantDone bool
		wantFail string // ожидаемый failSID ("" = терминал-фейла нет)
	}{
		{"строк ещё нет", nil, true, false, ""},
		// ★ГЛАВНЫЙ РЕГРЕСС NIM-45: keeper-строка success, но soul ещё не
		// распланирована (apply в полёте) — завершать НЕЛЬЗЯ.
		{"гонка: keeper-success, apply в полёте", []ApplyRunRow{{"keeper", "success"}}, true, false, ""},
		{"keeper-only: keeper-success, брекет снят", []ApplyRunRow{{"keeper", "success"}}, false, true, ""},
		{"keeper-success + soul-running", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "running"}}, true, false, ""},
		// Все success, но брекет держит: барьер/commit хостов ещё не прошёл
		// (multi-passage-безопасность) — не завершать.
		{"все success, брекет держит", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}}, true, false, ""},
		{"все success, брекет снят", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}}, false, true, ""},
		{"multi-soul: часть planned", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}, {"soul-b.dev", "planned"}}, true, false, ""},
		{"multi-soul: все success, брекет снят", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}, {"soul-b.dev", "success"}}, false, true, ""},
		{"терминал-фейл soul", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "failed"}}, true, false, "soul-a.dev"},
		{"терминал-фейл cancelled", []ApplyRunRow{{"keeper", "cancelled"}}, true, false, "keeper"},
		{"__run__ sentinel failed (no_hosts)", []ApplyRunRow{{"__run__", "failed"}}, false, false, "__run__"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done, failSID, _ := applySettled(tc.rows, tc.inFlight)
			if done != tc.wantDone {
				t.Errorf("done=%v, want %v (rows=%v inFlight=%v)", done, tc.wantDone, tc.rows, tc.inFlight)
			}
			if failSID != tc.wantFail {
				t.Errorf("failSID=%q, want %q", failSID, tc.wantFail)
			}
		})
	}
}
