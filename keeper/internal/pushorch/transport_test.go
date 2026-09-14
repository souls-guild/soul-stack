package pushorch

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// TestResolveProviders_TaskBeatsEverything is the owner's precedence decision
// at the level that implements it: a provider named by the task is above the
// per-job preset and above all three router levels, and the router is not even
// called. Calling it anyway would be a PG read per SID whose answer is thrown
// away — and, worse, a run that could still fail `provider_not_routed` on a
// host the task had already answered for.
func TestResolveProviders_TaskBeatsEverything(t *testing.T) {
	called := false
	run := &PushRun{deps: Deps{
		Router: &trackingRouter{inner: &fakeRouter{name: "router-provider"}, called: &called},
		Logger: quietLogger(),
	}}
	sids := []string{"sid-1", "sid-2"}

	routes, sources, fails := run.resolveProviders(
		context.Background(), sids,
		ApplyRequest{SSHProvider: "preset-provider"},
		config.TransportSSH, push.TransportOverride{Provider: "task-provider", User: "deploy", Port: 2222},
		quietLogger(),
	)
	if called {
		t.Error("the router was consulted although the task named a provider")
	}
	if len(fails) != 0 {
		t.Fatalf("fails = %v, want none", fails)
	}
	for _, sid := range sids {
		if routes[sid].Provider != "task-provider" {
			t.Errorf("routes[%s].Provider = %q, want task-provider (it must beat the α-compat preset too)", sid, routes[sid].Provider)
		}
		if sources[sid] != push.SourceTask {
			t.Errorf("sources[%s] = %v, want SourceTask", sid, sources[sid])
		}
		if routes[sid].Override.User != "deploy" || routes[sid].Override.Port != 2222 {
			t.Errorf("routes[%s].Override = %+v, want user=deploy port=2222", sid, routes[sid].Override)
		}
	}
}

// The provider and the connection fields are independent axes. A task that
// named only a user must keep it even though the router picked the provider —
// the bug in the other direction drops the override silently and the run dials
// as root.
func TestResolveProviders_OverrideRidesARoutedProvider(t *testing.T) {
	run := &PushRun{deps: Deps{
		Router: &fakeRouter{name: "router-provider"},
		Logger: quietLogger(),
	}}
	routes, sources, fails := run.resolveProviders(
		context.Background(), []string{"sid-1"},
		ApplyRequest{},
		config.TransportSSH, push.TransportOverride{User: "deploy"},
		quietLogger(),
	)
	if len(fails) != 0 {
		t.Fatalf("fails = %v, want none", fails)
	}
	if routes["sid-1"].Provider != "router-provider" {
		t.Errorf("Provider = %q, want the router's", routes["sid-1"].Provider)
	}
	if sources["sid-1"] == push.SourceTask {
		t.Error("route_source = task although the router picked the provider")
	}
	if routes["sid-1"].Override.User != "deploy" {
		t.Errorf("Override.User = %q, want deploy", routes["sid-1"].Override.User)
	}
}

// TestSummarize_EffectiveTransportValuesAreVisible is the OTHER half of the
// NIM-870 decision, and the reason the decision was affordable: the key makes a
// third source of truth for provider/user/port, so a run that used it must say
// so. Without these fields an incident review reads `souls.ssh_target` and sees
// a row the run never went to.
func TestSummarize_EffectiveTransportValuesAreVisible(t *testing.T) {
	_, summary := summarize([]hostResult{{
		sid:           "a.example.com",
		provider:      "task-provider",
		routeSource:   push.SourceTask,
		transportName: config.TransportSSH,
		override:      push.TransportOverride{Provider: "task-provider", User: "deploy", Port: 2222},
		ok:            true,
		status:        "success",
	}})
	hosts := summary["hosts"].([]map[string]any)
	if len(hosts) != 1 {
		t.Fatalf("len(hosts) = %d, want 1", len(hosts))
	}
	want := map[string]any{
		"transport":    config.TransportSSH,
		"ssh_provider": "task-provider",
		"route_source": "task",
		"ssh_user":     "deploy",
		"ssh_port":     2222,
	}
	for k, v := range want {
		if got := hosts[0][k]; got != v {
			t.Errorf("hosts[0][%q] = %v, want %v", k, got, v)
		}
	}
}

// The scalar form has to be visible too. It overrides nothing, so it was once
// invisible in the summary — which made the docs' "a run that used the key says
// so" false for exactly the simplest spelling of the key.
func TestSummarize_ScalarTransportIsStillVisible(t *testing.T) {
	_, summary := summarize([]hostResult{{
		sid:           "a.example.com",
		provider:      "cluster-provider",
		routeSource:   push.SourceCluster,
		transportName: config.TransportSSH,
		ok:            true,
		status:        "success",
	}})
	hosts := summary["hosts"].([]map[string]any)
	if hosts[0]["transport"] != config.TransportSSH {
		t.Errorf("transport = %v, want ssh — the scalar form names a transport even though it overrides nothing", hosts[0]["transport"])
	}
	for _, k := range []string{"ssh_user", "ssh_port"} {
		if _, present := hosts[0][k]; present {
			t.Errorf("hosts[0][%q] is present although the task overrode nothing", k)
		}
	}
}

// A run that did not write the key must not sprout transport fields: an
// operator reading `ssh_user` in a summary has to be able to conclude that a
// task overrode it, and a field present on every run says nothing.
func TestSummarize_NoOverrideNoTransportFields(t *testing.T) {
	_, summary := summarize([]hostResult{{
		sid:         "a.example.com",
		provider:    "cluster-provider",
		routeSource: push.SourceCluster,
		ok:          true,
		status:      "success",
	}})
	hosts := summary["hosts"].([]map[string]any)
	for _, k := range []string{"transport", "ssh_user", "ssh_port"} {
		if _, present := hosts[0][k]; present {
			t.Errorf("hosts[0][%q] is present although the task overrode nothing", k)
		}
	}
	if hosts[0]["route_source"] != "cluster" {
		t.Errorf("route_source = %v, want cluster", hosts[0]["route_source"])
	}
}

func TestTransportOverrideOf(t *testing.T) {
	cases := []struct {
		name     string
		plans    []render.DispatchPlan
		wantName string
		want     push.TransportOverride
		wantErr  string
	}{
		{name: "no plan names a transport", plans: []render.DispatchPlan{{}, {}}},
		{
			name: "one decision across the whole expansion",
			plans: []render.DispatchPlan{
				{TransportName: config.TransportSSH, TransportParams: map[string]any{"user": "deploy"}},
				{TransportName: config.TransportSSH, TransportParams: map[string]any{"user": "deploy"}},
			},
			wantName: config.TransportSSH,
			want:     push.TransportOverride{User: "deploy"},
		},
		{
			// The scalar form: it NAMES a transport and overrides nothing. The
			// name has to come back anyway — it is what puts `transport: ssh`
			// into the run summary, and without it that run is indistinguishable
			// from one that never wrote the key.
			name:     "the scalar form names a transport and overrides nothing",
			plans:    []render.DispatchPlan{{TransportName: config.TransportSSH}},
			wantName: config.TransportSSH,
		},
		{
			// A push run is the ssh transport. `agent` is the pull stream, and
			// falling through to SSH on it would run the task over a transport
			// the author explicitly did not ask for.
			name:    "the agent transport cannot carry a push run",
			plans:   []render.DispatchPlan{{TransportName: config.TransportAgent}},
			wantErr: "cannot carry a push run",
		},
		{
			name: "plans disagreeing is an error, not first-wins",
			plans: []render.DispatchPlan{
				{TransportName: config.TransportSSH},
				{TransportName: config.TransportAgent},
			},
			wantErr: "disagree on transport",
		},
		{
			name:    "an unusable param fails the run",
			plans:   []render.DispatchPlan{{TransportName: config.TransportSSH, TransportParams: map[string]any{"port": "2222"}}},
			wantErr: "expected an integer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, got, err := transportOverrideOf(tc.plans)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if got != tc.want {
				t.Errorf("override = %+v, want %+v", got, tc.want)
			}
		})
	}
}
