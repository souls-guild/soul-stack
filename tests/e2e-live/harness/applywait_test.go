package harness

import "testing"

// TestApplySettled pins the decision logic of WaitApplySuccess without live infra:
// the main regression is the NIM-45 race (keeper-success alone while apply is
// in flight must NOT complete the run) and keeper-only (bracket released →
// completes without waiting for soul).
func TestApplySettled(t *testing.T) {
	cases := []struct {
		name     string
		rows     []ApplyRunRow
		inFlight bool
		wantDone bool
		wantFail string // expected failSID ("" = no terminal failure)
	}{
		{"no rows yet", nil, true, false, ""},
		// ★MAIN REGRESSION NIM-45: keeper row success, but soul is not yet
		// planned (apply in flight) — must NOT complete.
		{"race: keeper-success, apply in flight", []ApplyRunRow{{"keeper", "success"}}, true, false, ""},
		{"keeper-only: keeper-success, bracket released", []ApplyRunRow{{"keeper", "success"}}, false, true, ""},
		{"keeper-success + soul-running", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "running"}}, true, false, ""},
		// All success, but bracket is held: host barrier/commit hasn't passed yet
		// (multi-passage safety) — must not complete.
		{"all success, bracket held", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}}, true, false, ""},
		{"all success, bracket released", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}}, false, true, ""},
		{"multi-soul: some planned", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}, {"soul-b.dev", "planned"}}, true, false, ""},
		{"multi-soul: all success, bracket released", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "success"}, {"soul-b.dev", "success"}}, false, true, ""},
		{"terminal failure: soul", []ApplyRunRow{{"keeper", "success"}, {"soul-a.dev", "failed"}}, true, false, "soul-a.dev"},
		{"terminal failure: cancelled", []ApplyRunRow{{"keeper", "cancelled"}}, true, false, "keeper"},
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
