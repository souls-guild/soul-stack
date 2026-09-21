package push

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestLiveTeleportRun — an env-gated live harness for the proxy-client
// ownership fix: Dial through a real Teleport Proxy + TWO sequential Run
// calls on one session (the regression already broke the first Run: `ssh:
// unexpected packet in response to channel open: <nil>`). Defaults — the dev
// stand (keeper.dev.yml push.teleport). Run:
//
//	TELEPORT_LIVE=1 TELEPORT_LIVE_SID=<node-name> \
//	  go test -count=1 -run TestLiveTeleportRun -v ./internal/push/
func TestLiveTeleportRun(t *testing.T) {
	if os.Getenv("TELEPORT_LIVE") == "" {
		t.Skip("TELEPORT_LIVE not set - live Teleport harness skipped")
	}
	sid := os.Getenv("TELEPORT_LIVE_SID")
	if sid == "" {
		t.Fatal("TELEPORT_LIVE_SID (target node name) is required")
	}

	dialer, err := NewTeleportDialer(TeleportDialerConfig{
		ProxyAddr:      envOr("TELEPORT_LIVE_PROXY", "teleport.example.com:443"),
		IdentityFile:   envOr("TELEPORT_LIVE_IDENTITY", "/tmp/keeper-dev/keeper-push.identity"),
		Cluster:        envOr("TELEPORT_LIVE_CLUSTER", "teleport-example"),
		UseSystemTrust: true,
		AlpnUpgrade:    true,
	})
	if err != nil {
		t.Fatalf("NewTeleportDialer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sess, err := dialer(ctx, DialConfig{
		Host:    sid,
		Port:    22,
		User:    envOr("TELEPORT_LIVE_USER", "root"),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial %s: %v", sid, err)
	}
	defer sess.Close()

	for i := 1; i <= 2; i++ {
		out, rerr := sess.Run(ctx, "echo teleport-live-ok", nil)
		if rerr != nil {
			t.Fatalf("Run #%d: %v", i, rerr)
		}
		if !strings.Contains(out, "teleport-live-ok") {
			t.Fatalf("Run #%d: unexpected stdout %q", i, out)
		}
	}
}
