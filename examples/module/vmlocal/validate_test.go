// What the artifact refuses, and why each refusal is worth having.
package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func hasError(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func createParams(profileEdits map[string]any, own map[string]any) map[string]any {
	p := validConn()
	prof := validProfile()
	for k, v := range profileEdits {
		if v == nil {
			delete(prof, k)
			continue
		}
		prof[k] = v
	}
	p["profile"] = prof
	p["name"] = "batch"
	for k, v := range own {
		p[k] = v
	}
	return p
}

func TestCreatedAcceptsAValidProfile(t *testing.T) {
	if errs := validateCreated(createParams(nil, nil)); len(errs) > 0 {
		t.Fatalf("a valid profile was refused: %v", errs)
	}
}

// ★ NIM-778: a param of the wrong type is REFUSED, not coerced.
//
// `profile` is declared Map, so the schema type-checks nothing inside it — these
// fields are exactly the ones the type system cannot see. Coercing instead would
// answer 0 for a mistyped number and then report the field as MISSING, which
// sends an operator looking for a field they did in fact write.
func TestMistypedProfileFieldsAreRefusedByType(t *testing.T) {
	cases := []struct {
		name  string
		edits map[string]any
		want  string
	}{
		{"cpu_size as a string", map[string]any{"cpu_size": "2"}, "profile.cpu_size must be an integer, got string"},
		{"ram_size as a string", map[string]any{"ram_size": "2G"}, "profile.ram_size must be an integer, got string"},
		{"cpu_size fractional", map[string]any{"cpu_size": 2.5}, "profile.cpu_size must be a whole number"},
		{"image_name as a number", map[string]any{"image_name": float64(12), "image_id": nil}, "profile.image_name must be a string, got float64"},
		{"deletion_protection as a string", map[string]any{"deletion_protection": "yes"}, "profile.deletion_protection must be a boolean, got string"},
		{"labels holding a number", map[string]any{"labels": map[string]any{runLabelKey: float64(7)}}, "must be a string, got float64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateCreated(createParams(tc.edits, nil))
			if !hasError(errs, tc.want) {
				t.Errorf("errors=%v, want one containing %q", errs, tc.want)
			}
			// The failure mode this guards: the wrong type silently becoming a
			// zero, reported as an absent field.
			if hasError(errs, "is required and must be > 0") && !hasError(errs, "got string") && !hasError(errs, "whole number") {
				t.Error("a mistyped value was reported as missing — that is the coercion bug, not a type refusal")
			}
		})
	}
}

// ★ A key vmlocal does not read is REFUSED, not quietly dropped.
//
// This is the guard that keeps the profile honest, and it has to be in code:
// `profile` is a Map, so the platform type-checks nothing inside it and an
// ignored key looks exactly like an honoured one to the operator who wrote it.
func TestUnknownProfileFieldsAreRefused(t *testing.T) {
	for _, field := range []string{"set_external_ip", "external_ip_id", "anti_affinity", "cluster", "image_version", "rm_external_id", "namespace_id"} {
		t.Run(field, func(t *testing.T) {
			errs := validateCreated(createParams(map[string]any{field: "x"}, nil))
			if !hasError(errs, "profile."+field+" is not a vmlocal profile field") {
				t.Errorf("errors=%v, want %q refused", errs, field)
			}
		})
	}
}

// ★ A key is refused for BEING THERE, not for asking for something. The old
// denylist let `cluster: ""` and `set_external_ip: false` through on the grounds
// that a default asks for nothing — which was right while the point was to stay
// runnable against a contract that had those fields. It is wrong now: a key
// vmlocal does not read is a key vmlocal does not read, whatever its value, and
// an author who wrote one has a wrong idea about this artifact either way.
func TestAnUnknownProfileKeyIsRefusedAtAnyValue(t *testing.T) {
	// Built directly rather than through createParams, whose nil means "drop the
	// key" — the case that matters most here is a key PRESENT and null, which is
	// what a bare `cluster:` in YAML becomes.
	for name, value := range map[string]any{
		"empty string": "", "false": false, "zero": float64(0), "null": nil,
	} {
		prof := validProfile()
		prof["cluster"] = value
		if _, errs := parseProfile(prof); !hasError(errs, "profile.cluster is not a vmlocal profile field") {
			t.Errorf("%s: errors=%v, want the key refused", name, errs)
		}
	}
}

// The refusal names the whole vocabulary, because "not a field" without the list
// leaves an author guessing which spelling was wanted.
func TestTheRefusalNamesTheVocabulary(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{"cpu_sizes": 2}, nil))
	if !hasError(errs, strings.Join(profileVocabulary, ", ")) {
		t.Errorf("errors=%v, want every accepted field listed", errs)
	}
}

func TestImageIDAndImageNameAreExclusive(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{
		"image_id":   "70b148c1-76bb-46c7-8f58-3f81783ac7a0",
		"image_name": "debian-12",
	}, nil))
	if !hasError(errs, "keep one") {
		t.Errorf("errors=%v, want a refusal of both at once", errs)
	}
}

func TestNetworkIDMustBeAUUID(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{"network_id": "default"}, nil))
	if !hasError(errs, "profile.network_id must be a UUID") {
		t.Errorf("errors=%v, want the UUID refusal", errs)
	}
}

// Without a batch identity a rerun cannot recognise its own machines, and tops up
// on top of a full batch.
func TestBatchIdentityIsRequired(t *testing.T) {
	p := createParams(nil, nil)
	delete(p, "name")
	if errs := validateCreated(p); !hasError(errs, "would spawn orphans") {
		t.Errorf("errors=%v, want the batch-identity refusal", errs)
	}

	p = createParams(map[string]any{"labels": map[string]any{runLabelKey: "run-7"}}, nil)
	delete(p, "name")
	if errs := validateCreated(p); len(errs) > 0 {
		t.Errorf("a run label alone should satisfy the identity requirement: %v", errs)
	}
}

// ★ A step pointed at some other provider's endpoint is told exactly that,
// rather than failing inside a dial with a message about a hostname.
func TestEndpointMustBeALibvirtURI(t *testing.T) {
	p := createParams(nil, nil)
	p[connEndpoint] = "grpc.example:80"
	errs := validateCreated(p)
	if !hasError(errs, "is not a libvirt URI") {
		t.Fatalf("errors=%v, want the URI refusal", errs)
	}
	if !hasError(errs, "some other provider's connection details") {
		t.Error("the message does not say what actually went wrong")
	}
}

func TestResizedRefusesCPUOrRAMWithoutDowntimeConsent(t *testing.T) {
	base := validConn()
	base["vm_ids"] = []any{"70b148c1-76bb-46c7-8f58-3f81783ac7a0"}

	base["cpu_cores"] = float64(4)
	if errs := validateResized(base); !hasError(errs, "needs allow_downtime: true") {
		t.Errorf("errors=%v, want the downtime gate", errs)
	}

	base["allow_downtime"] = true
	if errs := validateResized(base); len(errs) > 0 {
		t.Errorf("with consent the resize should validate: %v", errs)
	}

	// A disk-only resize is online and needs no consent.
	delete(base, "cpu_cores")
	delete(base, "allow_downtime")
	base["disk_gb"] = float64(20)
	if errs := validateResized(base); len(errs) > 0 {
		t.Errorf("a disk-only resize must not need allow_downtime: %v", errs)
	}
}

func TestResizedNeedsATarget(t *testing.T) {
	p := validConn()
	p["vm_ids"] = []any{"70b148c1-76bb-46c7-8f58-3f81783ac7a0"}
	if errs := validateResized(p); !hasError(errs, "at least one of cpu_cores / ram_mb / disk_gb") {
		t.Errorf("errors=%v, want a refusal of a resize that targets nothing", errs)
	}
}

// The namespace is the scope this artifact may touch. Without it, a probe would
// report machines it did not create.
func TestActionsOnExistingVMsRequireANamespace(t *testing.T) {
	p := validConn()
	delete(p, connNamespace)
	p["vm_ids"] = []any{"70b148c1-76bb-46c7-8f58-3f81783ac7a0"}
	if errs := validateVMIDs(p); !hasError(errs, "namespace is required") {
		t.Errorf("errors=%v, want the scope requirement", errs)
	}
}

func TestVMIDsMustBeAListOfNonEmptyStrings(t *testing.T) {
	p := validConn()
	p["vm_ids"] = "70b148c1-76bb-46c7-8f58-3f81783ac7a0"
	if errs := validateVMIDs(p); !hasError(errs, "must be a list of strings") {
		t.Errorf("errors=%v: a bare string must be refused, because vm_ids is declared a list", errs)
	}

	p["vm_ids"] = []any{"ok", float64(3)}
	if errs := validateVMIDs(p); !hasError(errs, "vm_ids[1] must be a string") {
		t.Errorf("errors=%v, want the element type refusal", errs)
	}
}

func TestUserdataOverTheCapIsRefused(t *testing.T) {
	errs := validateCreated(createParams(nil, map[string]any{
		"userdata": strings.Repeat("x", userdataMaxBytes+1),
	}))
	if !hasError(errs, "over the 32768-byte cap") {
		t.Errorf("errors=%v, want the seed cap enforced", errs)
	}
}

// ★ NIM-786 structurally: Validate and Apply call the SAME function, so anything
// Validate refuses Apply refuses. This walks the action table rather than naming
// the four functions, so a fifth action cannot be added without being covered.
func TestApplyRefusesEverythingValidateRefuses(t *testing.T) {
	obj := (&VMLocal{}).vm()
	broken := map[string]any{connEndpoint: "grpc.example:80"}

	brokenPB, err := structpb.NewStruct(broken)
	if err != nil {
		t.Fatalf("build params: %v", err)
	}

	for _, state := range obj.states() {
		t.Run(state, func(t *testing.T) {
			if errs := obj.actions[state].validate(broken); len(errs) == 0 {
				t.Fatalf("state %q accepted a param set whose endpoint is not a libvirt URI", state)
			}
			// Drive it through the object so the dispatch path is the one tested.
			reply, err := obj.Validate(context.Background(), &pluginv1.ValidateRequest{State: state, Params: brokenPB})
			if err != nil {
				t.Fatalf("Validate returned a transport error: %v", err)
			}
			if reply.GetOk() {
				t.Errorf("state %q: Validate said ok", state)
			}

			s := &applyStream{}
			if err := obj.Apply(&pluginv1.ApplyRequest{State: state, Params: brokenPB}, s); err != nil {
				t.Fatalf("Apply returned a transport error: %v", err)
			}
			if last := s.last(); last == nil || !last.GetFailed() {
				t.Errorf("state %q: Apply did not fail on params Validate refuses", state)
			}
		})
	}
}

// A state the object does not serve is refused by name, and the message says
// which object as well as which state.
func TestUnknownStateIsRefusedByName(t *testing.T) {
	obj := (&VMLocal{}).vm()
	reply, err := obj.Validate(context.Background(), &pluginv1.ValidateRequest{State: "provisioned"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if reply.GetOk() {
		t.Fatal("an unknown state was accepted")
	}
	if !hasError(reply.GetErrors(), `unknown state "provisioned" for object "vm"`) {
		t.Errorf("errors=%v, want the object named alongside the state", reply.GetErrors())
	}
}
