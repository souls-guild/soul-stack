package applyrun

import (
	"reflect"
	"slices"
	"testing"
)

// TestRunStatusesIsTheDecidedSet pins the closed set itself: every other guard is
// checked against RunStatuses, so a value dropped from it would take its own guards
// down with it. partial_failed is in the set although nothing produces it yet
// (NIM-948): the public schema carries it ahead of its writer.
func TestRunStatusesIsTheDecidedSet(t *testing.T) {
	want := []RunStatus{"applying", "success", "failed", "partial_failed", "cancelled"}
	if got := RunStatuses(); !slices.Equal(got, want) {
		t.Errorf("RunStatuses() = %v, want %v", got, want)
	}
}

// TestRunsStatusCountsGivesEveryRunStatusItsOwnBucket — /v1/runs/stats promises a
// counter per aggregate status ("zeros included — the enum is closed"). A status
// add() does not route lands in Total only, so the buckets stop summing to Total;
// one routed into another status's field is counted under the wrong name.
func TestRunsStatusCountsGivesEveryRunStatusItsOwnBucket(t *testing.T) {
	field := map[RunStatus]string{
		RunStatusApplying:      "Applying",
		RunStatusSuccess:       "Success",
		RunStatusFailed:        "Failed",
		RunStatusPartialFailed: "PartialFailed",
		RunStatusCancelled:     "Cancelled",
	}
	for _, s := range RunStatuses() {
		want, ok := field[s]
		if !ok {
			t.Errorf("%q: no bucket named for it", s)
			continue
		}
		var c RunsStatusCounts
		c.add(s, 1)
		var hit []string
		v := reflect.ValueOf(c)
		for i := range v.NumField() {
			if name := v.Type().Field(i).Name; name != "Total" && v.Field(i).Int() != 0 {
				hit = append(hit, name)
			}
		}
		if c.Total != 1 || !slices.Equal(hit, []string{want}) {
			t.Errorf("%q: Total = %d, buckets %v; want Total 1 and only %s", s, c.Total, hit, want)
		}
	}
	if n := reflect.TypeFor[RunsStatusCounts]().NumField() - 1; n != len(RunStatuses()) {
		t.Errorf("RunsStatusCounts has %d buckets for %d run statuses", n, len(RunStatuses()))
	}

	var c RunsStatusCounts
	c.add("no_such_status", 2)
	if c != (RunsStatusCounts{Total: 2}) {
		t.Errorf("unknown status: %+v, want it in Total only", c)
	}
}

// TestValidRunStatusIsTheClosedSet — the /v1/runs filter accepts partial_failed
// ahead of anything producing it (a query that finds nothing, not a 422), and
// nothing outside the run-status enum, host-row statuses included.
func TestValidRunStatusIsTheClosedSet(t *testing.T) {
	for _, s := range []RunStatus{"applying", "success", "failed", "partial_failed", "cancelled"} {
		if !ValidRunStatus(s) {
			t.Errorf("ValidRunStatus(%q) = false", s)
		}
	}
	for _, s := range []RunStatus{"", "partial", "succeeded", "orphaned", "no_match", "error_locked"} {
		if ValidRunStatus(s) {
			t.Errorf("ValidRunStatus(%q) = true", s)
		}
	}
}
