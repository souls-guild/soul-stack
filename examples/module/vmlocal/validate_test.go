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

func TestCreatedAcceptsAProfileTheCloudWouldAccept(t *testing.T) {
	if errs := validateCreated(createParams(nil, nil)); len(errs) > 0 {
		t.Fatalf("a valid profile was refused: %v", errs)
	}
}

// ★ NIM-778: a param of the wrong type is REFUSED, not coerced.
//
// `profile` is declared Map, so the schema type-checks nothing inside it — these
// ten fields are exactly the ones the type system cannot see. The cloud artifact
// answers 0 for a mistyped number and then reports the field as MISSING, which
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

// ★ A promise that cannot be kept locally is REFUSED, not quietly dropped. A
// silent no-op here is what would make the second implementation stop being a
// check on the contract.
func TestUnsupportablePromisesAreRefused(t *testing.T) {
	cases := []struct {
		field string
		value any
		want  string
	}{
		{"set_external_ip", true, "no external-address pool"},
		{"external_ip_id", "eip-1", "no external-address pool"},
		{"anti_affinity", "spread", "one hypervisor"},
		{"cluster", "cl-1", "no cluster placement"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			errs := validateCreated(createParams(map[string]any{tc.field: tc.value}, nil))
			if !hasError(errs, tc.want) {
				t.Errorf("errors=%v, want a refusal mentioning %q", errs, tc.want)
			}
		})
	}
}

// Asking for nothing is not a promise, so there is nothing to refuse.
func TestAFalseOrEmptyUnsupportedFieldIsAccepted(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{
		"set_external_ip": false,
		"anti_affinity":   "",
		"cluster":         "",
	}, nil))
	if len(errs) > 0 {
		t.Errorf("a profile asking for none of the unsupported features was refused: %v", errs)
	}
}

// rm_external_id is inert here and required anyway: a profile accepted locally
// must be accepted by the cloud, or this artifact is a worse gate than none.
func TestRmExternalIDIsRequiredEvenThoughItIsInert(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{"rm_external_id": nil}, nil))
	if !hasError(errs, "profile.rm_external_id is required") {
		t.Errorf("errors=%v, want rm_external_id required", errs)
	}
	if !hasError(errs, "accepted here is accepted by the cloud") {
		t.Error("the message does not explain why an inert field is required")
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
		t.Errorf("errors=%v, want the UUID refusal the cloud also makes", errs)
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

// ★ The endpoint is the one connection param with a local meaning. A step still
// carrying the cloud's endpoint is told exactly that, rather than failing inside a
// dial with a message about a hostname.
func TestEndpointMustBeALibvirtURI(t *testing.T) {
	p := createParams(nil, nil)
	p[connEndpoint] = "grpc.clv3:80"
	errs := validateCreated(p)
	if !hasError(errs, "is not a libvirt URI") {
		t.Fatalf("errors=%v, want the URI refusal", errs)
	}
	if !hasError(errs, "addressing the local provider with the cloud's connection details") {
		t.Error("the message does not say what actually went wrong")
	}
}

// ★ key_id and secret are accepted and unused, and that is DOCUMENTED rather than
// silent. If this test is ever deleted because "they do nothing", the no-op stops
// being a decision and becomes an accident.
func TestCredentialsAreAcceptedAndUnused(t *testing.T) {
	p := createParams(nil, nil)
	p[connKeyID] = "any-value-at-all"
	p[connSecret] = "any-value-at-all"
	if errs := validateCreated(p); len(errs) > 0 {
		t.Fatalf("credentials that mean nothing locally must still be accepted: %v", errs)
	}

	delete(p, connKeyID)
	if errs := validateCreated(p); !hasError(errs, "key_id is required") {
		t.Errorf("errors=%v: key_id must stay REQUIRED — the cloud requires it, and a laxer surface here greens a scenario the cloud refuses", errs)
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
	if errs := validateVMIDs(p); !hasError(errs, "namespace or namespace_id is required") {
		t.Errorf("errors=%v, want the scope requirement", errs)
	}
}

func TestVMIDsMustBeAListOfNonEmptyStrings(t *testing.T) {
	p := validConn()
	p["vm_ids"] = "70b148c1-76bb-46c7-8f58-3f81783ac7a0"
	if errs := validateVMIDs(p); !hasError(errs, "must be a list of strings") {
		t.Errorf("errors=%v: a bare string must be refused, because the cloud declares a list", errs)
	}

	p["vm_ids"] = []any{"ok", float64(3)}
	if errs := validateVMIDs(p); !hasError(errs, "vm_ids[1] must be a string") {
		t.Errorf("errors=%v, want the element type refusal", errs)
	}
}

func TestUserdataOverTheCloudCapIsRefused(t *testing.T) {
	errs := validateCreated(createParams(nil, map[string]any{
		"userdata": strings.Repeat("x", userdataMaxBytes+1),
	}))
	if !hasError(errs, "over the 32768-byte cap") {
		t.Errorf("errors=%v, want the cap mirrored from the cloud", errs)
	}
}

// ★ NIM-786 structurally: Validate and Apply call the SAME function, so anything
// Validate refuses Apply refuses. This walks the action table rather than naming
// the four functions, so a fifth action cannot be added without being covered.
func TestApplyRefusesEverythingValidateRefuses(t *testing.T) {
	obj := (&VMLocal{}).vm()
	broken := map[string]any{connEndpoint: "grpc.clv3:80"}

	brokenPB, err := structpb.NewStruct(broken)
	if err != nil {
		t.Fatalf("build params: %v", err)
	}

	for _, state := range obj.states() {
		t.Run(state, func(t *testing.T) {
			if errs := obj.actions[state].validate(broken); len(errs) == 0 {
				t.Fatalf("state %q accepted a param set with no credentials and a cloud endpoint", state)
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
