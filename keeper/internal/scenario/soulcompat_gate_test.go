package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// fakeSoulCap answers the presence source's one question. `lacking` is returned
// verbatim; a non-nil err is the transient-Redis case the gate must fail closed
// on.
type fakeSoulCap struct {
	lacking []string
	err     error
	asked   [][]string
}

func (f *fakeSoulCap) SoulsLackingCapability(_ context.Context, sids []string, _ string) ([]string, error) {
	f.asked = append(f.asked, append([]string(nil), sids...))
	return f.lacking, f.err
}

// The ADR-056 §S5 gate, extracted by NIM-880 so it can run TWICE: once for the
// starting roster and again on the roster a refresh boundary re-resolved.
//
// ★ The all-push skip is the only one, and it is not "the roster is empty". A
// provision-from-zero staged run has no hosts yet, and a nil checker there is
// precisely the case §S5 rejects — an earlier revision of this branch widened
// the skip to `len(streamed) == 0` and silently relaxed that.
func TestGatePassageCapability(t *testing.T) {
	agent := hostFacts("a.example.com", "agent")
	pushHost := hostFacts("p.example.com", "ssh")
	spec := RunSpec{IncarnationName: "redis-prod", ScenarioName: "create"}

	tests := []struct {
		name    string
		hosts   []*topology.HostFacts
		soulCap SoulCapabilityChecker
		wantErr bool
		wantAsk bool
	}{
		{"all-push: nobody to ask, and no checker needed", []*topology.HostFacts{pushHost}, nil, false, false},
		{"EMPTY roster with no checker: still fail-closed", nil, nil, true, false},
		{"all-agent with no checker: fail-closed", []*topology.HostFacts{agent}, nil, true, false},
		{"mixed with no checker: fail-closed", []*topology.HostFacts{agent, pushHost}, nil, true, false},
		{"all-agent, everyone announces", []*topology.HostFacts{agent}, &fakeSoulCap{}, false, true},
		{"all-agent, one lacks", []*topology.HostFacts{agent}, &fakeSoulCap{lacking: []string{"a.example.com"}}, true, true},
		{"checker error: fail-closed", []*topology.HostFacts{agent}, &fakeSoulCap{err: errors.New("redis down")}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{soulCap: tt.soulCap}
			err := r.gatePassageCapability(context.Background(), spec, tt.hosts, 2)
			if tt.wantErr && err == nil {
				t.Fatalf("gatePassageCapability = nil, want a refusal")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("gatePassageCapability = %v, want nil", err)
			}
			fake, _ := tt.soulCap.(*fakeSoulCap)
			if fake == nil {
				return
			}
			if asked := len(fake.asked) > 0; asked != tt.wantAsk {
				t.Errorf("checker asked = %v, want %v", asked, tt.wantAsk)
			}
			for _, batch := range fake.asked {
				for _, sid := range batch {
					if sid == pushHost.SID {
						t.Errorf("the push host was asked about: %v - it announces nothing and would report lacking everything", batch)
					}
				}
			}
		})
	}
}

// ★ The mixed case is what the refresh-boundary call exists for: a run that
// started all-push skips the gate, then picks up an AGENT host mid-run. The
// second call must ask about that host — otherwise passage>0 goes to a host
// nobody confirmed, its Soul echoes 0, and the Passage barrier waits out the
// run timeout.
func TestGatePassageCapability_AsksAboutAHostThatJoinedLater(t *testing.T) {
	fake := &fakeSoulCap{}
	r := &Runner{soulCap: fake}
	spec := RunSpec{IncarnationName: "redis-prod", ScenarioName: "create"}
	started := []*topology.HostFacts{hostFacts("p.example.com", "ssh")}
	grown := append(append([]*topology.HostFacts(nil), started...), hostFacts("joined.example.com", "agent"))

	if err := r.gatePassageCapability(context.Background(), spec, started, 2); err != nil {
		t.Fatalf("up-front gate on an all-push roster: %v", err)
	}
	if len(fake.asked) != 0 {
		t.Fatalf("the all-push starting roster was asked about: %v", fake.asked)
	}

	if err := r.gatePassageCapability(context.Background(), spec, grown, 2); err != nil {
		t.Fatalf("boundary gate on the grown roster: %v", err)
	}
	if len(fake.asked) != 1 || len(fake.asked[0]) != 1 || fake.asked[0][0] != "joined.example.com" {
		t.Fatalf("asked = %v, want exactly the agent host that joined", fake.asked)
	}
}

// The refusal must name the hosts, so an operator upgrading a fleet sees them
// all at once instead of rediscovering the next one on each retry.
func TestGatePassageCapability_RefusalNamesTheHosts(t *testing.T) {
	r := &Runner{soulCap: &fakeSoulCap{lacking: []string{"old-1.example.com", "old-2.example.com"}}}
	err := r.gatePassageCapability(context.Background(), RunSpec{IncarnationName: "i", ScenarioName: "s"},
		[]*topology.HostFacts{hostFacts("old-1.example.com", "agent"), hostFacts("old-2.example.com", "agent")}, 3)
	if err == nil {
		t.Fatal("hosts lacking the passage capability were accepted")
	}
	for _, want := range []string{"old-1.example.com", "old-2.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}
