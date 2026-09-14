package mcp

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestNewServer_RefusesAWiringWithoutRBAC — /mcp/events has no other way to learn
// that an Archon was revoked: no RejectRevoked runs on this listener (NIM-551) and
// the initiator branch admits a subscriber holding no permission at all. A nil
// checker would make sseStillAuthorized skip the revocation question entirely and
// an initiator's stream would never end, so the wiring is refused at construction
// rather than degraded at runtime.
func TestNewServer_RefusesAWiringWithoutRBAC(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	deps := ServerDeps{
		JWTVerifier: sseTestVerifier(t),
		Handler:     &Handler{},
		Bus:         applybus.NewBus(logger),
		Logger:      logger,
	}

	if _, err := NewServer(config.KeeperListenSimple{Addr: "127.0.0.1:0"}, deps); err == nil {
		t.Fatal("NewServer accepted a wiring with no RBAC — the event stream would then never " +
			"refuse a revoked Archon, which is the whole of NIM-858")
	} else if !strings.Contains(err.Error(), "RBAC") {
		t.Errorf("error = %q, want it to name RBAC", err)
	}

	// The control: the same wiring WITH a checker is accepted, so the refusal is
	// about the missing dependency and not about something else in deps.
	deps.RBAC = allowAllRBAC{}
	if _, err := NewServer(config.KeeperListenSimple{Addr: "127.0.0.1:0"}, deps); err != nil {
		t.Fatalf("NewServer refused a complete wiring: %v", err)
	}
}
