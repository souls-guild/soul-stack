//go:build integration

// Maintenance windows and freezes through the real router, RBAC and audit
// writer against Postgres (NIM-1072): the status block a GET returns is the
// calculator over the STORED rows, a freeze inside a slot moves next_start to
// the freeze's end, reads and writes are gated by different permissions, and
// every write leaves its audit row with the fields in the payload.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

const (
	maintInc    = "redis-maint"
	maintWriter = "archon-maint-writer"
	maintReader = "archon-maint-reader"
)

func maintDo(t *testing.T, method, url, tok string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type maintStatusReply struct {
	Items []struct {
		ID   string   `json:"id"`
		Days []string `json:"days"`
	} `json:"items"`
	Status struct {
		OpenNow      bool      `json:"open_now"`
		NextStart    time.Time `json:"next_start"`
		ActiveFreeze *struct {
			ID string `json:"id"`
		} `json:"active_freeze"`
	} `json:"status"`
}

func TestIntegration_MaintenanceWindowsEndToEnd(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, maintWriter, "")
	seedOperator(t, maintReader, "")
	seedIncarnationFull(t, maintInc, "redis", maintWriter, nil, map[string]any{})

	base, stop := startServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "maint-writer", Operators: []string{maintWriter}, Permissions: []string{"incarnation.get", "incarnation.maintenance-set"}},
		{Name: "maint-reader", Operators: []string{maintReader}, Permissions: []string{"incarnation.get"}},
	}})
	defer stop()
	writer := newValidTokenFor(t, maintWriter, []string{"maint-writer"})
	reader := newValidTokenFor(t, maintReader, []string{"maint-reader"})
	windows := base + "/v1/incarnations/" + maintInc + "/maintenance-windows"
	freezes := base + "/v1/incarnations/" + maintInc + "/freezes"

	// A daily three-hour slot that began one to two hours ago, so it is open now.
	now := time.Now().UTC()
	start := now.Truncate(time.Hour).Add(-time.Hour)
	window := map[string]any{
		"days":             []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		"start_time":       start.Format("15:04"),
		"duration_minutes": 180,
		"timezone":         "UTC",
		"comment":          "daily",
	}

	if code, body := maintDo(t, http.MethodPost, windows, reader, window); code != http.StatusForbidden {
		t.Fatalf("reader POST window: %d %s, want 403 — reading does not grant writing", code, body)
	}
	code, body := maintDo(t, http.MethodPost, windows, writer, window)
	if code != http.StatusCreated {
		t.Fatalf("writer POST window: %d %s, want 201", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &created)

	var st maintStatusReply
	code, body = maintDo(t, http.MethodGet, windows, reader, nil)
	if code != http.StatusOK {
		t.Fatalf("reader GET windows: %d %s", code, body)
	}
	_ = json.Unmarshal(body, &st)
	if len(st.Items) != 1 || st.Items[0].ID != created.ID || !st.Status.OpenNow || st.Status.ActiveFreeze != nil {
		t.Fatalf("GET after create = %s, want one window, open now, no freeze", body)
	}
	if d := st.Status.NextStart.Sub(now); d < 0 || d > time.Minute {
		t.Errorf("next_start while open = %s, want the moment of the read (~%s)", st.Status.NextStart, now)
	}

	// A freeze inside the running slot: closed now, next start is the freeze's
	// end because the slot is still running then.
	freezeEnd := now.Add(30 * time.Minute).Truncate(time.Second)
	code, body = maintDo(t, http.MethodPost, freezes, writer, map[string]any{
		"starts_at": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"ends_at":   freezeEnd.Format(time.RFC3339),
		"comment":   "incident",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST freeze: %d %s, want 201", code, body)
	}
	var freeze struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &freeze)

	st = maintStatusReply{}
	_, body = maintDo(t, http.MethodGet, windows, reader, nil)
	_ = json.Unmarshal(body, &st)
	if st.Status.OpenNow || !st.Status.NextStart.Equal(freezeEnd) || st.Status.ActiveFreeze == nil || st.Status.ActiveFreeze.ID != freeze.ID {
		t.Errorf("GET during the freeze = %s, want open_now=false, next_start=%s, active_freeze=%s",
			body, freezeEnd.Format(time.RFC3339), freeze.ID)
	}

	// Refusals: a field the validation refuses, a malformed id, an unknown id.
	bad := map[string]any{"days": []string{"mon"}, "start_time": "02:00", "duration_minutes": 60, "timezone": "Mars/Olympus"}
	if code, body := maintDo(t, http.MethodPut, windows+"/"+created.ID, writer, bad); code != http.StatusUnprocessableEntity {
		t.Errorf("PUT unknown zone: %d %s, want 422", code, body)
	}
	if code, body := maintDo(t, http.MethodDelete, windows+"/not-a-uuid", writer, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("DELETE malformed id: %d %s, want 422", code, body)
	}
	if code, body := maintDo(t, http.MethodDelete, windows+"/0b9f7c1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f", writer, nil); code != http.StatusNotFound {
		t.Errorf("DELETE unknown id: %d %s, want 404", code, body)
	}

	window["start_time"] = "03:00"
	if code, body := maintDo(t, http.MethodPut, windows+"/"+created.ID, writer, window); code != http.StatusOK {
		t.Errorf("PUT window: %d %s, want 200", code, body)
	}
	if code, body := maintDo(t, http.MethodDelete, freezes+"/"+freeze.ID, writer, nil); code != http.StatusNoContent {
		t.Errorf("DELETE freeze: %d %s, want 204", code, body)
	}
	if code, _ := maintDo(t, http.MethodDelete, freezes+"/"+freeze.ID, writer, nil); code != http.StatusNotFound {
		t.Errorf("second DELETE freeze: %d, want 404", code)
	}
	if code, body := maintDo(t, http.MethodGet, freezes, reader, nil); code != http.StatusOK || !bytes.Contains(body, []byte(`"items":[]`)) {
		t.Errorf("GET freezes after delete: %d %s, want 200 and no items", code, body)
	}

	// The trail: one row per successful write, by the writer, with the fields.
	type row struct {
		event   string
		payload map[string]any
	}
	rows, err := integrationPool.Query(context.Background(),
		`SELECT event_type, payload FROM audit_log WHERE archon_aid = $1 ORDER BY created_at, audit_id`, maintWriter)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	var trail []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.event, &r.payload); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		trail = append(trail, r)
	}
	rows.Close()
	want := []string{"maintenance_window.created", "freeze.created", "maintenance_window.updated", "freeze.deleted"}
	if len(trail) != len(want) {
		t.Fatalf("audit trail = %v, want %v", trail, want)
	}
	for i, r := range trail {
		if r.event != want[i] || r.payload["incarnation_id"] != maintInc {
			t.Errorf("audit[%d] = %s %v, want %s on %s", i, r.event, r.payload, want[i], maintInc)
		}
	}
	upd := trail[2].payload
	prev, _ := upd["previous"].(map[string]any)
	if upd["start_time"] != "03:00" || prev == nil || prev["start_time"] != start.Format("15:04") {
		t.Errorf("maintenance_window.updated payload = %v, want start_time 03:00 and previous.start_time %s",
			upd, start.Format("15:04"))
	}
	if trail[3].payload["comment"] != "incident" {
		t.Errorf("freeze.deleted payload = %v, want the deleted freeze's fields", trail[3].payload)
	}
}

// TestIntegration_OnceWindowsAndMixedBodies (NIM-1137): a once window lives in
// the same list with only its own fields, a future one defers the status to its
// start, one that has ended no longer counts, and a body mixing the two kinds is
// 422 naming the field of the other kind.
func TestIntegration_OnceWindowsAndMixedBodies(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, maintWriter, "")
	seedIncarnationFull(t, maintInc, "redis", maintWriter, nil, map[string]any{})
	base, stop := startServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "maint-writer", Operators: []string{maintWriter}, Permissions: []string{"incarnation.get", "incarnation.maintenance-set"}},
	}})
	defer stop()
	writer := newValidTokenFor(t, maintWriter, []string{"maint-writer"})
	windows := base + "/v1/incarnations/" + maintInc + "/maintenance-windows"

	now := time.Now().UTC().Truncate(time.Second)
	for name, tc := range map[string]struct {
		body  map[string]any
		field string
	}{
		"once with days": {map[string]any{"kind": "once", "starts_at": now.Format(time.RFC3339),
			"ends_at": now.Add(time.Hour).Format(time.RFC3339), "days": []string{"mon"}}, "'days'"},
		"kind omitted with bounds": {map[string]any{"days": []string{"mon"}, "start_time": "02:00", "duration_minutes": 60,
			"timezone": "UTC", "starts_at": now.Format(time.RFC3339)}, "'starts_at'"},
		"once ending at its start": {map[string]any{"kind": "once", "starts_at": now.Format(time.RFC3339),
			"ends_at": now.Format(time.RFC3339)}, "'ends_at'"},
	} {
		if code, body := maintDo(t, http.MethodPost, windows, writer, tc.body); code != http.StatusUnprocessableEntity ||
			!bytes.Contains(body, []byte(tc.field)) {
			t.Errorf("%s: %d %s, want 422 naming %s", name, code, body, tc.field)
		}
	}

	past := map[string]any{"kind": "once", "starts_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"ends_at": now.Add(-time.Hour).Format(time.RFC3339), "comment": "done"}
	if code, body := maintDo(t, http.MethodPost, windows, writer, past); code != http.StatusCreated {
		t.Fatalf("POST past once window: %d %s", code, body)
	}
	var st maintStatusReply
	_, body := maintDo(t, http.MethodGet, windows, writer, nil)
	_ = json.Unmarshal(body, &st)
	if len(st.Items) != 1 || !st.Status.OpenNow {
		t.Fatalf("GET with only a past once window = %s, want it listed and open_now=true", body)
	}

	start := now.Add(2 * time.Hour)
	code, body := maintDo(t, http.MethodPost, windows, writer, map[string]any{
		"kind": "once", "starts_at": start.Format(time.RFC3339), "ends_at": start.Add(time.Hour).Format(time.RFC3339),
	})
	if code != http.StatusCreated {
		t.Fatalf("POST future once window: %d %s", code, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	if created["kind"] != "once" || created["starts_at"] != start.Format(time.RFC3339) {
		t.Errorf("created = %s, want kind once with starts_at", body)
	}
	for _, k := range []string{"days", "start_time", "duration_minutes", "timezone"} {
		if _, ok := created[k]; ok {
			t.Errorf("created once window carries the weekly field %q: %s", k, body)
		}
	}
	st = maintStatusReply{}
	_, body = maintDo(t, http.MethodGet, windows, writer, nil)
	_ = json.Unmarshal(body, &st)
	if len(st.Items) != 2 || st.Status.OpenNow || !st.Status.NextStart.Equal(start) {
		t.Errorf("GET with a future once window = %s, want closed until %s", body, start.Format(time.RFC3339))
	}
}
