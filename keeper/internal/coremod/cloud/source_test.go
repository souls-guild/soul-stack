package cloud_test

// Guards for the inline driver source (NIM-668): `driver` + `credentials` in the
// step instead of a row of the `providers` registry. Three things are guarded
// here — that exactly one source is accepted, that the inline one produces the
// same driver call the registry one does, and that the credential it carries
// reaches the driver and NOTHING else.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	coremodcloud "github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// inlineSecret is a plaintext credential of the kind Vault hands back. It is
// deliberately one long, unmistakable literal: the leak guards search for it as
// a SUBSTRING, so a partial escape (a quoted, truncated or re-encoded copy)
// still trips them.
const inlineSecret = "wb-inline-access-key-DO-NOT-LEAK"

// inlineCredsValue is what reaches Apply under `credentials`. The author writes
// `credentials: vault:secret/cloud/wb-dev`; the vault-resolve phase (ADR-010,
// phase 1) replaces that string with the secret MAP before the module runs, so
// a test passing a string here would exercise a shape production never produces.
func inlineCredsValue() map[string]any {
	return map[string]any{"access_key_id": "AKIAINLINE", "secret_access_key": inlineSecret}
}

// authoredCredsRef is the same cell one phase earlier: what a DEFINITION holds
// under `credentials`, which is the reference the author typed. [Module.Validate]
// reads the step here, before anything resolved; Apply reads it after, where the
// map above has taken its place. Feeding the resolved shape to Validate tests a
// value soul-lint never sees — and hides a Validate that refuses every correct
// definition in the tree.
const authoredCredsRef = "vault:secret/cloud/wb-dev"

// asAuthored moves a case's `credentials` cell back to the authored side, so one
// table of source rules can drive both halves of the contract without pretending
// they read the same shape. Every other param has one shape on both sides.
func asAuthored(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	if _, ok := out["credentials"]; ok {
		out["credentials"] = authoredCredsRef
	}
	return out
}

// inlineSource is the param pair that carries the driver in the step.
func inlineSource() map[string]any {
	return map[string]any{
		"driver":      "wb",
		"credentials": inlineCredsValue(),
	}
}

// stateParams merges a source param set with the params the state needs for its
// own reasons, so a source-rule test fails on the source rule and not on a
// missing vm_ids or an absent allow_downtime.
func stateParams(state string, source map[string]any) map[string]any {
	p := make(map[string]any, len(source)+3)
	for k, v := range source {
		p[k] = v
	}
	switch state {
	case coremodcloud.StateDestroyed:
		p["vm_ids"] = []any{"i-aaa"}
	case coremodcloud.StateResized:
		p["vm_ids"] = []any{"i-aaa"}
		p["desired"] = map[string]any{"cpu_cores": float64(4)}
		p["allow_downtime"] = true
	}
	return p
}

// allStates — every state must answer the source rules identically (ticket point
// 7): a rule that holds for `created` and not for `resized` is a trap, because
// the two are written side by side in the same scenario.
var allStates = []string{coremodcloud.StateCreated, coremodcloud.StateDestroyed, coremodcloud.StateResized}

// applyWith runs one Apply against a module wired with the given plugin host and
// resolver, and returns the stream so the caller can read every event.
func applyWith(t *testing.T, fp *fakePlugins, fr coremodcloud.ProviderResolver, fa *fakeAudit, state string, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply(%s): %v", state, err)
	}
	return stream
}

// sourceRuleCases — the four ways a step can get the source wrong, with the
// words the refusal must carry. A half-set inline pair is reported as the
// incomplete pair it is, not as "no source": the author has already chosen.
var sourceRuleCases = []struct {
	name   string
	source map[string]any
	want   []string
}{
	{
		name:   "both sources",
		source: map[string]any{"provider": "wb-prod", "driver": "wb", "credentials": inlineCredsValue()},
		want:   []string{"mutually exclusive", "provider", "driver", "credentials"},
	},
	{
		name:   "no source",
		source: map[string]any{},
		want:   []string{"no cloud provider source", "provider", "driver", "credentials"},
	},
	// The two halves carry the WHOLE ordered phrase, not the three words it is
	// made of: as sets `{credentials, requires, driver}` and
	// `{driver, requires, credentials}` are the same set, so swapping the two
	// message bodies in source.go would leave both of these green.
	{
		name:   "credentials without driver",
		source: map[string]any{"credentials": inlineCredsValue()},
		want:   []string{`param "credentials" requires "driver"`, "plugin alias"},
	},
	{
		name:   "driver without credentials",
		source: map[string]any{"driver": "wb"},
		want:   []string{`param "driver" requires "credentials"`, "vault reference"},
	},
}

// typeErrorCases — a source param the author DID write, with the wrong type. The
// module owes them one error about that param and nothing else: the source rules
// cannot read a cell they cannot parse, and "no cloud provider source" on top of
// `param "driver": expected string` sends someone holding a `driver:` line off
// to add one.
var typeErrorCases = []struct {
	name   string
	source map[string]any
	want   string
	deny   string
}{
	{"driver alone", map[string]any{"driver": float64(42)}, `param "driver"`, "no cloud provider source"},
	{"driver with credentials", map[string]any{"driver": float64(42), "credentials": inlineCredsValue()}, `param "driver"`, `requires "driver"`},
	{"provider", map[string]any{"provider": float64(42)}, `param "provider"`, "no cloud provider source"},
}

// TestSourceRules_StandDownForATypeError ★ — the guard for the pair above, on
// both halves and every state.
func TestSourceRules_StandDownForATypeError(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	for _, tc := range typeErrorCases {
		for _, state := range allStates {
			t.Run("apply/"+tc.name+"/"+state, func(t *testing.T) {
				ev := applyWith(t, &fakePlugins{}, &fakeResolver{}, &fakeAudit{}, state, stateParams(state, tc.source)).Last()
				if ev == nil || !ev.GetFailed() {
					t.Fatalf("expected failed=true, got %+v", ev)
				}
				if !strings.Contains(ev.GetMessage(), tc.want) {
					t.Errorf("message %q does not name %q", ev.GetMessage(), tc.want)
				}
				if strings.Contains(ev.GetMessage(), tc.deny) {
					t.Errorf("message %q still carries %q, which contradicts the type error", ev.GetMessage(), tc.deny)
				}
			})
			t.Run("validate/"+tc.name+"/"+state, func(t *testing.T) {
				rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
					State:  state,
					Params: mustStruct(t, stateParams(state, asAuthored(tc.source))),
				})
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if rep.Ok {
					t.Fatal("expected Ok=false")
				}
				joined := strings.Join(rep.Errors, "; ")
				if !strings.Contains(joined, tc.want) {
					t.Errorf("errors %q do not name %q", joined, tc.want)
				}
				if strings.Contains(joined, tc.deny) {
					t.Errorf("errors %q still carry %q, which contradicts the type error", joined, tc.deny)
				}
			})
		}
	}
}

// TestApply_SourceExclusivity ★ — the source rules are enforced in Apply, which
// is where a keeper-side core module actually runs (the dispatcher calls Apply
// directly; no ValidateRequest is built for it in production), for every state.
func TestApply_SourceExclusivity(t *testing.T) {
	for _, tc := range sourceRuleCases {
		for _, state := range allStates {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				fp := &fakePlugins{}
				ev := applyWith(t, fp, &fakeResolver{}, &fakeAudit{}, state, stateParams(state, tc.source)).Last()
				if ev == nil || !ev.GetFailed() {
					t.Fatalf("expected failed=true, got %+v", ev)
				}
				for _, want := range tc.want {
					if !strings.Contains(ev.GetMessage(), want) {
						t.Errorf("message %q does not name %q", ev.GetMessage(), want)
					}
				}
				if fp.lastDriver != "" {
					t.Errorf("driver %q was called despite an unresolved source", fp.lastDriver)
				}
			})
		}
	}
}

// TestValidate_SourceExclusivity — soul-lint sees the same rules Apply enforces,
// with the same words. The pair of tests cannot drift: both halves call the same
// functions in source.go. They are fed the same cases in the shape each half
// really receives — see [asAuthored].
func TestValidate_SourceExclusivity(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	for _, tc := range sourceRuleCases {
		for _, state := range allStates {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
					State:  state,
					Params: mustStruct(t, stateParams(state, asAuthored(tc.source))),
				})
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if rep.Ok {
					t.Fatal("expected Ok=false")
				}
				joined := strings.Join(rep.Errors, "; ")
				for _, want := range tc.want {
					if !strings.Contains(joined, want) {
						t.Errorf("errors %q do not name %q", joined, want)
					}
				}
			})
		}
	}
}

// TestApply_RegistryMode_RegionAndSuffixRefused ★ — `region`/`fqdn_suffix`
// belong to the inline source. With `provider` the registry entry already holds
// both, and honouring the step param would move a fleet to another region on a
// line the author could equally have read as documentation. Refused, not
// silently preferred (either way round).
func TestApply_RegistryMode_RegionAndSuffixRefused(t *testing.T) {
	for _, param := range []string{"region", "fqdn_suffix"} {
		for _, state := range allStates {
			if param == "fqdn_suffix" && state != coremodcloud.StateCreated {
				continue // only `created` declares it
			}
			t.Run(param+"/"+state, func(t *testing.T) {
				fp := &fakePlugins{}
				source := map[string]any{"provider": "wb-prod", param: "ru-central1"}
				ev := applyWith(t, fp, &fakeResolver{}, &fakeAudit{}, state, stateParams(state, source)).Last()
				if ev == nil || !ev.GetFailed() {
					t.Fatalf("expected failed=true for provider + %s, got %+v", param, ev)
				}
				if !strings.Contains(ev.GetMessage(), param) || !strings.Contains(ev.GetMessage(), "provider") {
					t.Errorf("message %q must name both %q and \"provider\"", ev.GetMessage(), param)
				}
				if fp.lastDriver != "" {
					t.Errorf("driver %q was called despite a refused param combination", fp.lastDriver)
				}
			})
		}
	}
}

// TestApply_Created_Inline_ZeroRegistryRows ★ — the motivating case: a resolver
// that is not wired at all (nil — no `providers`/`profiles` rows anywhere, and
// nothing to read them with) still provisions, because the step carries the
// whole tuple. `region` lands in the credentials map under the key the registry
// path uses, so a driver reads it from one place either way.
func TestApply_Created_Inline_ZeroRegistryRows(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.wb.internal", PrimaryIp: "10.0.0.1"},
	}}
	params := stateParams(coremodcloud.StateCreated, inlineSource())
	params["region"] = "ru-central1"
	params["profile"] = map[string]any{"platform": "standard-v3", "cores": float64(4)}
	params["count"] = float64(1)

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("unexpected: %+v", ev)
	}
	if fp.lastDriver != "wb" {
		t.Errorf("driver = %q, want wb (the alias the step named)", fp.lastDriver)
	}
	if fp.lastCreds["secret_access_key"] != inlineSecret || fp.lastCreds["access_key_id"] != "AKIAINLINE" {
		t.Errorf("credentials not forwarded to the driver, keys: %v", credKeys(fp.lastCreds))
	}
	if fp.lastCreds["region"] != "ru-central1" {
		t.Errorf("region = %v, want ru-central1 merged into the credentials map", fp.lastCreds["region"])
	}
	if fp.lastProfile["platform"] != "standard-v3" {
		t.Errorf("inline profile spec not handed to the driver as-is: %v", fp.lastProfile)
	}
}

// TestApply_Created_Inline_NoRegion_LeavesCredentialsAlone — an absent `region`
// must not write an empty one over a region the secret itself carries. The
// registry entry always has the column; the step param does not.
func TestApply_Created_Inline_NoRegion_LeavesCredentialsAlone(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{{VmId: "i-aaa", Fqdn: "h1.wb.internal"}}}
	creds := inlineCredsValue()
	creds["region"] = "from-the-secret"
	params := stateParams(coremodcloud.StateCreated, map[string]any{"driver": "wb", "credentials": creds})

	if ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last(); ev.GetFailed() {
		t.Fatalf("unexpected failed: %+v", ev)
	}
	if fp.lastCreds["region"] != "from-the-secret" {
		t.Errorf("region = %v, want the secret's own value kept (no param, nothing to override with)", fp.lastCreds["region"])
	}
}

// TestApply_Destroyed_Inline_ZeroRegistryRows — symmetry (ticket point 7):
// destroy reaches the driver over the inline source with the same tuple.
func TestApply_Destroyed_Inline_ZeroRegistryRows(t *testing.T) {
	fp := &fakePlugins{destroyResp: []string{"i-aaa"}}
	params := stateParams(coremodcloud.StateDestroyed, inlineSource())
	params["region"] = "ru-central1"

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateDestroyed, params).Last()
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("unexpected: %+v", ev)
	}
	if fp.lastDriver != "wb" || fp.lastCreds["region"] != "ru-central1" {
		t.Errorf("driver=%q region=%v, want wb / ru-central1", fp.lastDriver, fp.lastCreds["region"])
	}
	if len(fp.lastDestroyed) != 1 || fp.lastDestroyed[0] != "i-aaa" {
		t.Errorf("destroyed ids = %v, want [i-aaa]", fp.lastDestroyed)
	}
}

// TestApply_Resized_Inline_ZeroRegistryRows — symmetry, resize half.
func TestApply_Resized_Inline_ZeroRegistryRows(t *testing.T) {
	fp := &fakePlugins{resizeResp: []*pluginv1.VmResizeResult{{VmId: "i-aaa"}}}
	params := stateParams(coremodcloud.StateResized, inlineSource())
	params["region"] = "ru-central1"

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateResized, params).Last()
	if ev.GetFailed() {
		t.Fatalf("unexpected failed: %+v", ev)
	}
	if fp.lastDriver != "wb" || fp.lastCreds["region"] != "ru-central1" {
		t.Errorf("driver=%q region=%v, want wb / ru-central1", fp.lastDriver, fp.lastCreds["region"])
	}
	if fp.lastDesired.GetCpuCores() != 4 {
		t.Errorf("desired cpu = %d, want 4", fp.lastDesired.GetCpuCores())
	}
}

// TestApply_Registry_TupleUnchanged — regression on the path that already
// existed: with `provider` the resolver still decides the driver and the
// credentials, the audit still names the registry entry, and nothing from the
// inline branch is mixed in.
func TestApply_Registry_TupleUnchanged(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{{VmId: "i-aaa", Fqdn: "h1.example.com"}}}
	fr := &fakeResolver{driver: "wb", creds: map[string]any{"access_key_id": "AKIAREG", "region": "eu-west-1"}}
	fa := &fakeAudit{}

	ev := applyWith(t, fp, fr, fa, coremodcloud.StateCreated, map[string]any{"provider": "wb-prod"}).Last()
	if ev.GetFailed() {
		t.Fatalf("unexpected failed: %+v", ev)
	}
	if fr.lastProvider != "wb-prod" {
		t.Errorf("resolver got provider=%q, want wb-prod", fr.lastProvider)
	}
	if fp.lastDriver != "wb" || fp.lastCreds["access_key_id"] != "AKIAREG" || fp.lastCreds["region"] != "eu-west-1" {
		t.Errorf("registry tuple changed: driver=%q creds=%v", fp.lastDriver, fp.lastCreds)
	}
	if len(fa.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(fa.events))
	}
	if got := fa.events[0].Payload["provider"]; got != "wb-prod" {
		t.Errorf("audit provider = %v, want the registry name wb-prod", got)
	}
}

// TestApply_Inline_AuditNamesTheDriver ★ — the audit event identifies WHO was
// called where the registry name would be (ticket point 8). `provider` reads as
// the alias inline, `driver` always carries the alias, so the two together tell
// an auditor which source a run used. Credentials appear in neither.
func TestApply_Inline_AuditNamesTheDriver(t *testing.T) {
	for _, tc := range []struct {
		state string
		fp    *fakePlugins
	}{
		{coremodcloud.StateCreated, &fakePlugins{createResp: []*pluginv1.VmInfo{{VmId: "i-aaa", Fqdn: "h1.wb.internal"}}}},
		{coremodcloud.StateDestroyed, &fakePlugins{destroyResp: []string{"i-aaa"}}},
		{coremodcloud.StateResized, &fakePlugins{resizeResp: []*pluginv1.VmResizeResult{{VmId: "i-aaa"}}}},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fa := &fakeAudit{}
			if ev := applyWith(t, tc.fp, nil, fa, tc.state, stateParams(tc.state, inlineSource())).Last(); ev.GetFailed() {
				t.Fatalf("unexpected failed: %+v", ev)
			}
			if len(fa.events) != 1 {
				t.Fatalf("audit events = %d, want 1", len(fa.events))
			}
			payload := fa.events[0].Payload
			if payload["provider"] != "wb" || payload["driver"] != "wb" {
				t.Errorf("payload provider=%v driver=%v, want both the alias wb", payload["provider"], payload["driver"])
			}
			if _, has := payload["credentials"]; has {
				t.Errorf("audit payload carries a credentials key: %v", credKeys(payload))
			}
		})
	}
}

// TestApply_InlineCredentials_NeverLeaveTheModule ★★ — the leak guard, and the
// reason it asserts on the SUBSTRING rather than on a masking call: what matters
// is that the literal is absent from what the module emits, whichever function
// was or was not invoked on the way.
//
// Covered channels are the ones this module writes: every ApplyEvent it sends
// (message + output — run-events, SSE and the register feed all read from these)
// and every audit event it writes (`cloud.provisioned`). Both the success and
// the driver-failure path are exercised, because the failure path is where a
// message gets built out of context and is the likelier leak. The rendered-params
// channel (apply_run_plan → /tasks) is written by the scenario runner, not here,
// and is guarded in keeper/internal/scenario and keeper/internal/render.
func TestApply_InlineCredentials_NeverLeaveTheModule(t *testing.T) {
	hosts := map[string]*fakePlugins{
		"created/ok":             {createResp: []*pluginv1.VmInfo{{VmId: "i-aaa", Fqdn: "h1.wb.internal"}}},
		"created/driver-error":   {createErr: errors.New("driver create failed: quota exceeded")},
		"destroyed/ok":           {destroyResp: []string{"i-aaa"}},
		"destroyed/driver-error": {destroyErr: errors.New("driver destroy failed: forbidden")},
		"resized/ok":             {resizeResp: []*pluginv1.VmResizeResult{{VmId: "i-aaa"}}},
		"resized/driver-error":   {resizeErr: errors.New("driver resize failed: no capacity")},
	}
	for name, fp := range hosts {
		t.Run(name, func(t *testing.T) {
			state := strings.SplitN(name, "/", 2)[0]
			params := stateParams(state, inlineSource())
			params["region"] = "ru-central1"

			fa := &fakeAudit{}
			stream := applyWith(t, fp, nil, fa, state, params)

			// The driver is the one place the credential is allowed to reach.
			// Without this the whole test could pass by never carrying a secret.
			if fp.lastCreds["secret_access_key"] != inlineSecret {
				t.Fatalf("the driver never received the credential — the leak scan below would be vacuous")
			}
			for i, ev := range stream.Events {
				if strings.Contains(ev.GetMessage(), inlineSecret) {
					t.Errorf("secret leaked into event[%d].message: %q", i, ev.GetMessage())
				}
				if out := ev.GetOutput(); out != nil {
					raw, err := json.Marshal(out.AsMap())
					if err != nil {
						t.Fatalf("marshal event output: %v", err)
					}
					if strings.Contains(string(raw), inlineSecret) {
						t.Errorf("secret leaked into event[%d].output: %s", i, raw)
					}
				}
			}
			for i, ev := range fa.events {
				raw, err := json.Marshal(ev)
				if err != nil {
					t.Fatalf("marshal audit event: %v", err)
				}
				if strings.Contains(string(raw), inlineSecret) {
					t.Errorf("secret leaked into audit event[%d]: %s", i, raw)
				}
			}
		})
	}
}

// TestApply_CredentialsAsPlainString_Refused ★ — a string under `credentials`
// means either a literal credential typed into a definition or a `vault:` ref
// nothing resolved. Both are refused, and the message must not echo the value:
// an error text travels to run-events and error_summary, so echoing it here
// would be exactly the leak the param exists to prevent.
func TestApply_CredentialsAsPlainString_Refused(t *testing.T) {
	fp := &fakePlugins{}
	params := stateParams(coremodcloud.StateCreated, map[string]any{"driver": "wb", "credentials": inlineSecret})

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if !ev.GetFailed() {
		t.Fatalf("expected failed=true on a string credentials, got %+v", ev)
	}
	if strings.Contains(ev.GetMessage(), inlineSecret) {
		t.Errorf("the refusal echoed the value: %q", ev.GetMessage())
	}
	if !strings.Contains(ev.GetMessage(), "vault:") {
		t.Errorf("message %q must show the form the author should have written", ev.GetMessage())
	}
	if fp.lastDriver != "" {
		t.Errorf("driver %q was called with an unusable credential", fp.lastDriver)
	}
}

// TestApply_CredentialsAsPlainString_NoDriver_NamesTheMissingHalf ★ — a value
// that failed to parse still counts as CHOOSING the inline source. Otherwise the
// author who wrote `credentials:` and forgot `driver:` was told there was no
// source set at all, and went looking for a param already on their screen.
func TestApply_CredentialsAsPlainString_NoDriver_NamesTheMissingHalf(t *testing.T) {
	fp := &fakePlugins{}
	params := stateParams(coremodcloud.StateCreated, map[string]any{"credentials": inlineSecret})

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if !ev.GetFailed() {
		t.Fatalf("expected failed=true, got %+v", ev)
	}
	msg := ev.GetMessage()
	if strings.Contains(msg, "no cloud provider source") {
		t.Errorf("the step names a source and is told it named none: %q", msg)
	}
	if !strings.Contains(msg, `"driver"`) {
		t.Errorf("message %q must name the missing half", msg)
	}
	if strings.Contains(msg, inlineSecret) {
		t.Errorf("the refusal echoed the value: %q", msg)
	}
}

// TestApply_Created_Inline_SelfOnboard_RequiresSuffixParam ★ — self-onboard
// predicts the FQDN before create, and inline there is no registry column to
// take the suffix from. The refusal points at the step param, not at
// `providers.fqdn_suffix` the author has no row of.
func TestApply_Created_Inline_SelfOnboard_RequiresSuffixParam(t *testing.T) {
	fp := &fakePlugins{}
	fu := &fakeUserdata{}
	params := stateParams(coremodcloud.StateCreated, inlineSource())
	params["name"] = "redis"
	params["self_onboard"] = true

	m := coremodcloud.New(fp, nil, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: coremodcloud.StateCreated, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.GetFailed() {
		t.Fatalf("expected failed=true without fqdn_suffix, got %+v", ev)
	}
	if !strings.Contains(ev.GetMessage(), "fqdn_suffix") {
		t.Errorf("message %q must name the missing param", ev.GetMessage())
	}
	if strings.Contains(ev.GetMessage(), "providers.fqdn_suffix") {
		t.Errorf("inline message points at a registry column the author has no row of: %q", ev.GetMessage())
	}
	if fp.lastDriver != "" {
		t.Errorf("driver %q was called without a predictable FQDN", fp.lastDriver)
	}
}

// TestValidate_Created_Inline_SelfOnboard_RequiresSuffixParam — the same rule
// offline: with an inline driver the suffix is a param of this step, so
// soul-lint can see its absence without resolving anything.
func TestValidate_Created_Inline_SelfOnboard_RequiresSuffixParam(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, nil, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	params := stateParams(coremodcloud.StateCreated, asAuthored(inlineSource()))
	params["name"] = "redis"
	params["self_onboard"] = true

	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: coremodcloud.StateCreated, Params: mustStruct(t, params)})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Fatal("expected Ok=false without fqdn_suffix under an inline driver")
	}
	if !strings.Contains(strings.Join(rep.Errors, "; "), "fqdn_suffix") {
		t.Errorf("errors %v must name fqdn_suffix", rep.Errors)
	}
}

// TestApply_Created_Inline_SelfOnboard_PredictsFromParam ★ — with the param
// present, prediction is identical to the registry path: SID =
// `<name>-<i>.<suffix>`, tokens baked under the predicted FQDNs.
func TestApply_Created_Inline_SelfOnboard_PredictsFromParam(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "redis-0.wb.internal", PrimaryIp: "10.0.0.1"},
		{VmId: "i-bbb", Fqdn: "redis-1.wb.internal", PrimaryIp: "10.0.0.2"},
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n# baked\n"}
	params := stateParams(coremodcloud.StateCreated, inlineSource())
	params["name"] = "redis"
	params["count"] = float64(2)
	params["self_onboard"] = true
	params["fqdn_suffix"] = "wb.internal"

	m := coremodcloud.New(fp, nil, fs, ft, &fakeCascade{}, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: coremodcloud.StateCreated, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("unexpected: %+v", ev)
	}
	if len(fs.inserted) != 2 || fs.inserted[0].SID != "redis-0.wb.internal" {
		t.Fatalf("registered SIDs = %v, want them predicted from the fqdn_suffix param", fs.remaining())
	}
	for _, fqdn := range []string{"redis-0.wb.internal", "redis-1.wb.internal"} {
		if _, ok := fu.lastTokens[fqdn]; !ok {
			t.Errorf("no token baked for predicted FQDN %q (baked: %v)", fqdn, keysOf(fu.lastTokens))
		}
	}
	if fp.lastUserdata != "#cloud-config\n# baked\n" {
		t.Errorf("driver got userdata=%q, want the self-onboard render", fp.lastUserdata)
	}
}

// TestApply_Created_InlineDriver_WithRegistryProfile ★ — the decided rule for
// mixing: `profile: <string>` names an entry of the `profiles` registry, which is
// independent of the `providers` one, so it is ALLOWED alongside an inline
// driver. Refusing it would gut the case the ticket exists for — a fleet that
// keeps VM sizes in one shared registry while every step carries its own
// credentials.
func TestApply_Created_InlineDriver_WithRegistryProfile(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{{VmId: "i-aaa", Fqdn: "h1.wb.internal"}}}
	fr := &fakeResolver{
		profileParams: map[string]any{"platform": "standard-v3"},
		err:           errors.New("Resolve must not be called for an inline driver"),
	}
	params := stateParams(coremodcloud.StateCreated, inlineSource())
	params["profile"] = "redis-small"

	ev := applyWith(t, fp, fr, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if ev.GetFailed() {
		t.Fatalf("mixing a registry profile with an inline driver must be allowed: %+v", ev)
	}
	if fr.lastProfile != "redis-small" {
		t.Errorf("ResolveProfile got %q, want redis-small", fr.lastProfile)
	}
	if fr.lastProvider != "" {
		t.Errorf("the providers registry was consulted (%q) for an inline driver", fr.lastProvider)
	}
	if fp.lastProfile["platform"] != "standard-v3" {
		t.Errorf("resolved profile not handed to the driver: %v", fp.lastProfile)
	}
	if fp.lastDriver != "wb" {
		t.Errorf("driver = %q, want the inline alias wb", fp.lastDriver)
	}
}

// TestApply_Created_ProfileName_NoRegistry_Refused — the one refusal on the
// profile side: a NAME with no registry to look it up in. The message says what
// to write instead, because the inline spec goes in the very same param.
func TestApply_Created_ProfileName_NoRegistry_Refused(t *testing.T) {
	fp := &fakePlugins{}
	params := stateParams(coremodcloud.StateCreated, inlineSource())
	params["profile"] = "redis-small"

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if !ev.GetFailed() {
		t.Fatalf("expected failed=true for a profile NAME without a registry, got %+v", ev)
	}
	if !strings.Contains(ev.GetMessage(), "redis-small") || !strings.Contains(ev.GetMessage(), "profiles registry") {
		t.Errorf("message %q must name the profile and the registry it needs", ev.GetMessage())
	}
	if fp.lastDriver != "" {
		t.Errorf("driver %q was called without a resolved profile", fp.lastDriver)
	}
}

// credKeys lists the keys of a credentials map or an audit payload for a failure
// message — the VALUES may be secret, the key names are not.
func credKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestValidate_Inline_AuthoredVaultRef_Accepted ★ — the form an author actually
// writes must PASS the static half, in every state. Validate runs before
// vault-resolve, so `credentials` is the reference itself there; demanding the
// resolved map would reject every correct definition in the tree while Apply,
// reading the same cell one phase later, accepted it. The two halves disagreeing
// about the one param that carries a secret is the worst place to disagree: the
// author is told their working step is invalid, and the obvious way out is to
// stop writing a reference.
func TestValidate_Inline_AuthoredVaultRef_Accepted(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	for _, state := range allStates {
		t.Run(state, func(t *testing.T) {
			rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  state,
				Params: mustStruct(t, stateParams(state, asAuthored(inlineSource()))),
			})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !rep.Ok {
				t.Fatalf("the authored form was refused: %v", rep.Errors)
			}
		})
	}
}

// TestValidate_CredentialsShape_Refused ★ — what the static half must still
// refuse, and with which words. A plain string is a credential typed into a
// definition. A `${ … }` expression is the trap this param invites: vault-resolve
// walks raw params BEFORE the CEL phase (ADR-010), so a reference an expression
// builds is never resolved and reaches the driver as the reference itself.
// Neither refusal echoes the value it read — an error text travels to
// error_summary.
func TestValidate_CredentialsShape_Refused(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	for _, tc := range []struct {
		name  string
		creds string
		want  string
	}{
		{"a literal credential", inlineSecret, "expected a vault reference"},
		{"a reference built by an expression", "${ vars.cloud.credentials_ref }", "never resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  coremodcloud.StateCreated,
				Params: mustStruct(t, stateParams(coremodcloud.StateCreated, map[string]any{"driver": "wb", "credentials": tc.creds})),
			})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if rep.Ok {
				t.Fatal("expected Ok=false")
			}
			joined := strings.Join(rep.Errors, "; ")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors %q do not say %q", joined, tc.want)
			}
			if strings.Contains(joined, inlineSecret) {
				t.Errorf("the refusal echoed the value: %q", joined)
			}
		})
	}
}

// TestApply_CredentialsUnresolvedRef_NamesTheTrap ★ — the same mistake from the
// other side. A `vault:` reference that survived to Apply means the phase that
// resolves references never saw it, and the message must say THAT. Telling this
// author they wrote a literal credential sends them looking for a value they
// never typed, and the shortest way to silence the error would be to type one.
func TestApply_CredentialsUnresolvedRef_NamesTheTrap(t *testing.T) {
	fp := &fakePlugins{}
	params := stateParams(coremodcloud.StateCreated, map[string]any{"driver": "wb", "credentials": authoredCredsRef})

	ev := applyWith(t, fp, nil, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if !ev.GetFailed() {
		t.Fatalf("expected failed=true on an unresolved ref, got %+v", ev)
	}
	if !strings.Contains(ev.GetMessage(), "never resolved") {
		t.Errorf("message %q must name the phase order, not a literal value", ev.GetMessage())
	}
	if fp.lastDriver != "" {
		t.Errorf("driver %q was called with an unresolved reference", fp.lastDriver)
	}
}

// TestSourceErrors_SurviveTheMasker ★ — a refusal the author never reads is not
// a refusal. Every message this seam produces travels to error_summary and
// status_details, where shared/audit hunts vault-SHAPED substrings by content
// and replaces the WHOLE string it finds one in. So an example reference spelled
// out in full — `vault:secret/cloud/wb-dev` — erases the sentence built around
// it: the author who forgot `credentials:` reads ***MASKED*** and is told
// nothing at the exact moment they need telling. A placeholder
// (`vault:<mount>/<path>`) survives, so the fix is free; this guard is on the
// real strings rather than on a rule about them, and it runs the real masker.
func TestSourceErrors_SurviveTheMasker(t *testing.T) {
	var msgs []string
	for _, tc := range sourceRuleCases {
		ev := applyWith(t, &fakePlugins{}, &fakeResolver{}, &fakeAudit{},
			coremodcloud.StateCreated, stateParams(coremodcloud.StateCreated, tc.source)).Last()
		if ev == nil || !ev.GetFailed() {
			t.Fatalf("%s: expected a failed event to read a message from, got %+v", tc.name, ev)
		}
		msgs = append(msgs, ev.GetMessage())
	}

	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	for _, tc := range sourceRuleCases {
		rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  coremodcloud.StateCreated,
			Params: mustStruct(t, stateParams(coremodcloud.StateCreated, asAuthored(tc.source))),
		})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		msgs = append(msgs, rep.Errors...)
	}

	for _, msg := range msgs {
		if msg == "" {
			t.Fatal("empty message: the guard would pass on nothing")
		}
		masked := audit.MaskSecrets(map[string]any{"_": msg})["_"]
		if masked != msg {
			t.Errorf("the masker rewrote a refusal the author has to read:\n  written: %s\n  read:    %v", msg, masked)
		}
	}
}

// TestApply_Created_NoSource_CostsNothingToFind ★ — a step that named no source
// cannot succeed, and must be refused before anything external happens. The
// registry path had this for free: `provider` was a required string read on the
// first line. Resolving the source is heavier and now sits further down, so the
// cheap half runs first — otherwise a misspelled step renders userdata, which
// reads the CA from Vault, and only then discovers it was never going to run.
func TestApply_Created_NoSource_CostsNothingToFind(t *testing.T) {
	fp := &fakePlugins{}
	fu := &fakeUserdata{out: "#cloud-config\n"}
	fr := &fakeResolver{}
	params := stateParams(coremodcloud.StateCreated, map[string]any{"generate_userdata": true})

	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: coremodcloud.StateCreated, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "no cloud provider source") {
		t.Fatalf("expected the source refusal, got %+v", ev)
	}
	if fu.last != nil {
		t.Error("userdata was rendered (a Vault read) for a step that had no source")
	}
	if fr.lastProvider != "" {
		t.Errorf("the providers registry was read for %q", fr.lastProvider)
	}
}

// TestApply_Created_Registry_BadProfile_LeavesTheProviderRowUnread ★ — order on
// the registry path: the profile is resolved first, so a name that is not in the
// profiles registry is refused without reading a provider row and the Vault
// secret behind it. This ticket added a second source, not a reason for a typo
// to cost a secret read.
func TestApply_Created_Registry_BadProfile_LeavesTheProviderRowUnread(t *testing.T) {
	fp := &fakePlugins{}
	fr := &fakeResolver{profileErr: errors.New("profile not found")}
	params := stateParams(coremodcloud.StateCreated, map[string]any{"provider": "wb-prod"})
	params["profile"] = "standrd-4gb"

	ev := applyWith(t, fp, fr, &fakeAudit{}, coremodcloud.StateCreated, params).Last()
	if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "standrd-4gb") {
		t.Fatalf("expected the profile refusal, got %+v", ev)
	}
	if fr.lastProvider != "" {
		t.Errorf("the provider row %q was read before the profile name was checked", fr.lastProvider)
	}
}
