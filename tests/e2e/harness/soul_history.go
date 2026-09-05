//go:build e2e

// Soul run-history helper. It lived in harness/drift.go until NIM-446 deleted
// the drift circuit; the two were never related — the file just happened to
// hold both — so the surviving half moved here under its own name.

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// SoulHistoryItem / SoulHistoryReply are the wire types of
// GET /v1/souls/{sid}/history — the declarations keeper's handler returns.
// The harness used to restate them field by field (NIM-776).
type (
	SoulHistoryItem  = wire.SoulHistoryItem
	SoulHistoryReply = wire.SoulHistoryReply
)

// SoulHistory calls GET /v1/souls/{sid}/history with an optional type query
// filter (scenario|errand; empty = both sources) and returns the parsed
// response. Any non-200 — t.Fatal with the body.
func (s *Stack) SoulHistory(t *testing.T, sid, typeFilter string) SoulHistoryReply {
	t.Helper()
	c := s.opClient(t)
	path := "/v1/souls/" + sid + "/history"
	if typeFilter != "" {
		path += "?type=" + typeFilter
	}
	resp, status, err := c.get(context.Background(), path)
	if err != nil {
		t.Fatalf("SoulHistory %s: http: %v", sid, err)
	}
	if status != http.StatusOK {
		t.Fatalf("SoulHistory %s: status %d, expected 200; body=%s", sid, status, string(resp))
	}
	var reply SoulHistoryReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("SoulHistory %s: decode reply: %v (body=%s)", sid, err, string(resp))
	}
	return reply
}
