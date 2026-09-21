package push

import (
	"context"
	"strings"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

func TestTransportOverrideFrom(t *testing.T) {
	cases := []struct {
		name    string
		tname   string
		params  map[string]any
		want    TransportOverride
		wantOK  bool
		wantErr string
	}{
		{name: "no transport named", tname: ""},
		{name: "the agent transport is not the push flow's", tname: config.TransportAgent},
		{name: "ssh with no params", tname: config.TransportSSH, wantOK: true},
		{
			name:   "every param",
			tname:  config.TransportSSH,
			params: map[string]any{"ssh_provider": "vault-bastion", "user": "deploy", "port": uint64(2222)},
			want:   TransportOverride{Provider: "vault-bastion", User: "deploy", Port: 2222},
			wantOK: true,
		},
		{
			name:   "port as the float a JSON round-trip produces",
			tname:  config.TransportSSH,
			params: map[string]any{"port": float64(2222)},
			want:   TransportOverride{Port: 2222},
			wantOK: true,
		},
		{
			name:    "a param the transport does not take",
			tname:   config.TransportSSH,
			params:  map[string]any{"primary_ip": "10.0.0.7"},
			wantErr: "unknown param",
		},
		{
			name:    "a param of the wrong type",
			tname:   config.TransportSSH,
			params:  map[string]any{"user": 42},
			wantErr: "expected a string",
		},
		{
			name:    "a fractional port",
			tname:   config.TransportSSH,
			params:  map[string]any{"port": 22.5},
			wantErr: "expected an integer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := TransportOverrideFrom(tc.tname, tc.params)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("override = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTransportOverride_Apply is the precedence rule the owner decided:
// the TASK beats `souls.ssh_target`. Field by field, because the override is
// sparse — a task that named only a user must not blank the registry's port.
func TestTransportOverride_Apply(t *testing.T) {
	registry := SSHTarget{Host: "a.example.com", Port: 22, User: "root", SoulPath: HostSoulBinaryPath}

	t.Run("the task wins where it spoke", func(t *testing.T) {
		got := TransportOverride{User: "deploy", Port: 2222}.Apply(registry)
		if got.User != "deploy" || got.Port != 2222 {
			t.Fatalf("target = %+v, want user=deploy port=2222", got)
		}
		// The fields the task did not name are untouched, including the one
		// NIM-869 pinned to the delivery path.
		if got.Host != registry.Host || got.SoulPath != registry.SoulPath {
			t.Errorf("target = %+v, want host/soul_path from the registry", got)
		}
	})

	t.Run("the registry keeps what the task left alone", func(t *testing.T) {
		got := TransportOverride{User: "deploy"}.Apply(registry)
		if got.Port != 22 {
			t.Errorf("port = %d, want the registry's 22", got.Port)
		}
	})

	t.Run("an empty override changes nothing", func(t *testing.T) {
		var o TransportOverride
		if !o.Empty() {
			t.Fatal("the zero override is not Empty()")
		}
		if got := o.Apply(registry); got != registry {
			t.Errorf("target = %+v, want it untouched (%+v)", got, registry)
		}
	})
}

// TestRouteSource_TaskLabel pins the label itself: it is what a run summary
// carries, and an incident review reads it as the answer to "which source did
// this run actually go to".
func TestRouteSource_TaskLabel(t *testing.T) {
	if got := SourceTask.String(); got != "task" {
		t.Fatalf("SourceTask.String() = %q, want task", got)
	}
	// The four levels must stay distinguishable; a collision would make the
	// summary field useless exactly when it matters.
	seen := map[string]bool{}
	for _, s := range []RouteSource{SourceSoul, SourceCoven, SourceCluster, SourceTask} {
		if seen[s.String()] {
			t.Fatalf("two route sources share the label %q", s.String())
		}
		seen[s.String()] = true
	}
}

// TestDispatcher_RouteOverrideReachesTheDial — the override has to reach the
// SSH connection itself, on BOTH sessions the run opens.
//
// Cleanup is the half that was missing: it re-resolves the target from the
// registry, so a run whose task said `user: deploy` applied as deploy and then
// cleaned up as root — a second session to the same host on a different account,
// failing with nothing but a warn log while the summary still read `deploy`. The
// stale `/var/lib/soul-stack/{bin,modules}` then stay on the host forever.
func TestDispatcher_RouteOverrideReachesTheDial(t *testing.T) {
	override := TransportOverride{User: "deploy", Port: 2222}

	newDisp := func(t *testing.T, got *DialConfig, cleaner Cleaner) *SshDispatcher {
		t.Helper()
		return newTestDispatcher(t, Deps{
			Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
			Targets:   &mockTargets{target: sshTarget()},
			Souls:     &mockSouls{s: sshSoul()},
			Cleaner:   cleaner,
			Dial: func(_ context.Context, cfg DialConfig) (Session, error) {
				*got = cfg
				return &mockSession{stdout: successStdout(t, "ap-override-1")}, nil
			},
		})
	}

	t.Run("SendApply", func(t *testing.T) {
		var got DialConfig
		disp := newDisp(t, &got, nil)
		if _, err := disp.SendApply(context.Background(), "host-1.example.com",
			Route{Provider: testProviderName, Override: override},
			&keeperv1.ApplyRequest{ApplyId: "ap-override-1"}, nil); err != nil {
			t.Fatalf("SendApply: %v", err)
		}
		if got.User != "deploy" || got.Port != 2222 {
			t.Errorf("dialled %s@:%d, want deploy@:2222 — the registry says soul@:22", got.User, got.Port)
		}
	})

	t.Run("Cleanup", func(t *testing.T) {
		var got DialConfig
		disp := newDisp(t, &got, &orderingCleaner{})
		if err := disp.Cleanup(context.Background(), "host-1.example.com",
			Route{Provider: testProviderName, Override: override}); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if got.User != "deploy" || got.Port != 2222 {
			t.Errorf("cleanup dialled %s@:%d, want deploy@:2222 — it must land on the same account the apply used", got.User, got.Port)
		}
	})
}
