package bootstrap_test

import (
	"context"
	"strings"
	"testing"

	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeAudit struct {
	events []*audit.Event
}

func (a *fakeAudit) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// containsTokenDeep recursively searches for a token substring in any string
// value within a structure (output / audit payload) — a safeguard against a
// secret leak.
func containsTokenDeep(v any, token string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, token)
	case map[string]any:
		for _, vv := range t {
			if containsTokenDeep(vv, token) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if containsTokenDeep(vv, token) {
				return true
			}
		}
	}
	return false
}

// TestDeliveredStateIsRefusedNotIgnored guards the removal of
// `core.bootstrap.delivered` (NIM-834) at the shape that matters: a scenario
// still carrying that address must FAIL, on both the validate and the apply
// path.
//
// Silently succeeding is the dangerous outcome, not a noisy error. The step it
// replaced was what put the Soul binary on a fresh VM, wrote its token and
// redeemed it; a run that skips it reaches `core.soul.registered` and blocks on
// `await_online` until the run timeout, with nothing in the output saying that
// the host was never installed. The failure the operator then debugs is the
// barrier, several minutes and one wrong subject away from the cause.
//
// The refusal is deliberately the generic unknown-state message rather than a
// special case for `delivered`: the address is gone, not deprecated, and
// soul-lint already refuses it offline against the core manifest
// (TestBootstrapDeliveredIsNotInTheCatalog).
func TestDeliveredStateIsRefusedNotIgnored(t *testing.T) {
	m := &coremodbootstrap.Module{Issuer: &fakeIssuer{}, Audit: &fakeAudit{}}

	// The params a pre-NIM-834 scenario carried, so the refusal is proven to
	// come from the state and not from a params mismatch that happens to fail.
	params := mustStruct(t, map[string]any{
		"hosts": []any{map[string]any{
			"sid":             "vm1.example.com",
			"primary_ip":      "10.0.0.5",
			"bootstrap_token": "tok-vm1",
		}},
		"ssh_provider": "teleport",
		"install":      true,
	})

	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: "delivered", Params: params})
	if err != nil {
		t.Fatalf("Validate returned a transport error: %v", err)
	}
	if rep.Ok {
		t.Error("Validate accepted state \"delivered\": a scenario carrying the removed address would render clean and skip host installation silently")
	}
	if len(rep.Errors) == 0 || !strings.Contains(rep.Errors[0], "unknown state") {
		t.Errorf("Validate errors = %v, want an unknown-state refusal", rep.Errors)
	}

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "delivered", Params: params}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	last := stream.Last()
	if last == nil {
		t.Fatal("Apply emitted no final event on state \"delivered\"")
	}
	if !last.GetFailed() {
		t.Fatal("Apply did not fail on state \"delivered\": the step reports success and the run proceeds to the onboarding barrier over hosts nobody installed")
	}
	if !strings.Contains(last.GetMessage(), "unknown state") {
		t.Errorf("Apply failure details = %q, want an unknown-state refusal", last.GetMessage())
	}
}
