package mcp

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- sigil fakes ---
//
// Narrow fakes for [sigil.Store] / [sigil.SlotReader]: verifies TRANSPORT —
// that the plugin-tool correctly calls sigil.Service, maps sentinels to MCP
// codes, and writes audit. Sigil business invariants (signing, single-slot
// lookup, CRUD) are covered by the sigil package's unit/integration tests;
// this file covers only the MCP boundary.

type fakeSigilStore struct {
	inserted   *sigil.Sigil
	insertErr  error
	revokeErr  error
	listResult []*sigil.Sigil
	listErr    error
}

func (s *fakeSigilStore) Insert(_ context.Context, rec *sigil.Sigil) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = rec
	return nil
}

func (s *fakeSigilStore) Revoke(_ context.Context, _, _, _, _ string) error {
	return s.revokeErr
}

func (s *fakeSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) {
	return s.listResult, s.listErr
}

type fakeSigilSlots struct {
	slot      *pluginhost.SlotContents
	err       error
	commit    string
	commitErr error
}

func (s fakeSigilSlots) ReadSlot(_, _ string) (*pluginhost.SlotContents, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.slot, nil
}

func (s fakeSigilSlots) SlotCommitSHA(_, _ string) (string, error) {
	if s.commitErr != nil {
		return "", s.commitErr
	}
	// Default: a successful slot carries a synthetic commit_sha (A1-S4 —
	// current-target). Empty commit only when explicitly set.
	if s.commit == "" && s.slot != nil {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}
	return s.commit, nil
}

// fixtureSHA256 — valid 64-hex digest of the binary for the Allow flow
// (Signer.Sign requires exactly 64 lower-hex characters).
var fixtureSHA256 = hex.EncodeToString(func() []byte { d := sha256.Sum256([]byte("cloud-binary")); return d[:] }())

// sigilSlotFixture — valid cache slot (binary + manifest) for the Allow flow.
func sigilSlotFixture() *pluginhost.SlotContents {
	return &pluginhost.SlotContents{
		BinaryPath:    "/cache/cloud-hetzner/soul-cloud-hetzner",
		BinarySHA256:  fixtureSHA256,
		ManifestBytes: []byte("kind: cloud_driver\nprotocol_version: 1\nnamespace: cloud\nname: hetzner\n"),
	}
}

// --- harness ---

// newSigilHandler builds a Handler with SigilSvc over fakeSigilStore/fakeSigilSlots.
// store=nil → SigilSvc stays nil (for nil-guard tests).
func newSigilHandler(t *testing.T, rbacCfg *rbactest.Config, store sigil.Store, slots sigil.SlotReader) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc:   opSvc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        logger,
		IncarnationDB: &fakePool{},
	}
	if store != nil {
		signer, err := sigil.NewSigner(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
		if err != nil {
			t.Fatalf("sigil.NewSigner: %v", err)
		}
		svc, err := sigil.NewService(sigil.ServiceDeps{Signer: signer, Store: store, Slots: slots, Logger: logger})
		if err != nil {
			t.Fatalf("sigil.NewService: %v", err)
		}
		deps.SigilSvc = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// sigilAdminCfg — RBAC config granting archon-alice all plugin.*-permissions.
func sigilAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "plugin-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"plugin.allow", "plugin.revoke", "plugin.list",
			}},
		},
	}
}

// --- tests: manifest / catalog ---

func TestPluginTools_InManifest(t *testing.T) {
	want := []string{"keeper.plugin.allow", "keeper.plugin.revoke", "keeper.plugin.list"}
	for _, name := range want {
		e, ok := toolByName(name)
		if !ok {
			t.Errorf("%s missing from catalogManifest", name)
			continue
		}
		if e.status != toolStatusImplemented {
			t.Errorf("%s status = %d, want Implemented", name, e.status)
		}
	}
}

// --- tests: nil-guard ---

func TestPluginTools_NilGuard(t *testing.T) {
	h, _ := newSigilHandler(t, sigilAdminCfg(), nil, nil) // SigilSvc == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.plugin.allow", `{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`},
		{"keeper.plugin.revoke", `{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`},
		{"keeper.plugin.list", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

// --- tests: RBAC ---

func TestPluginTools_RBACForbidden(t *testing.T) {
	// archon-alice without plugin.*-permissions (empty RBAC → deny all):
	// mutations don't run (RBAC happens first), audit stays empty.
	h, rec := newSigilHandler(t, nil, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.allow",
		`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied allow must not write audit")
	}
}

// --- tests: validation ---

func TestPluginTools_Validation(t *testing.T) {
	h := func() *Handler {
		h, _ := newSigilHandler(t, sigilAdminCfg(), &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
		return h
	}()
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"allow-no-namespace", "keeper.plugin.allow", `{"name":"hetzner","ref":"v1.0.0"}`, mcpCodeValidationFailed},
		{"allow-no-name", "keeper.plugin.allow", `{"namespace":"cloud","ref":"v1.0.0"}`, mcpCodeValidationFailed},
		{"allow-no-ref", "keeper.plugin.allow", `{"namespace":"cloud","name":"hetzner"}`, mcpCodeValidationFailed},
		{"allow-bad-ref-slash", "keeper.plugin.allow", `{"namespace":"cloud","name":"hetzner","ref":"feature/x"}`, mcpCodeValidationFailed},
		{"allow-traversal", "keeper.plugin.allow", `{"namespace":"cloud","name":"hetzner","ref":".."}`, mcpCodeValidationFailed},
		{"revoke-no-ref", "keeper.plugin.revoke", `{"namespace":"cloud","name":"hetzner"}`, mcpCodeValidationFailed},
		{"allow-unknown-field", "keeper.plugin.allow", `{"namespace":"cloud","name":"hetzner","ref":"v1.0.0","x":1}`, mcpCodeMalformedRequest},
		{"list-unknown-field", "keeper.plugin.list", `{"x":1}`, mcpCodeMalformedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}

// --- tests: allow ---

func TestPluginAllow_Success(t *testing.T) {
	store := &fakeSigilStore{}
	h, rec := newSigilHandler(t, sigilAdminCfg(), store, fakeSigilSlots{slot: sigilSlotFixture()})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.allow",
		`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodePluginAllowOutput(t, resp)
	if out.Namespace != "cloud" || out.Name != "hetzner" || out.Ref != "v1.0.0" {
		t.Errorf("output triple = %+v", out)
	}
	if out.SHA256 != fixtureSHA256 {
		t.Errorf("sha256 = %q, want %q", out.SHA256, fixtureSHA256)
	}
	if store.inserted == nil {
		t.Fatal("store.Insert was not called")
	}

	ev := requireSingleAudit(t, rec, string(audit.EventPluginAllowed))
	if ev.Payload["namespace"] != "cloud" || ev.Payload["name"] != "hetzner" || ev.Payload["ref"] != "v1.0.0" {
		t.Errorf("audit triple = %+v", ev.Payload)
	}
	if ev.Payload["sha256"] != fixtureSHA256 {
		t.Errorf("audit sha256 = %v", ev.Payload["sha256"])
	}
	if ev.Payload["allowed_by_aid"] != "archon-alice" {
		t.Errorf("audit allowed_by_aid = %v", ev.Payload["allowed_by_aid"])
	}
	// signature/manifest (crypto material / large JSONB) must NOT reach audit.
	if _, ok := ev.Payload["signature"]; ok {
		t.Error("audit payload leaks 'signature'")
	}
	if _, ok := ev.Payload["manifest"]; ok {
		t.Error("audit payload leaks 'manifest'")
	}
}

func TestPluginAllow_NotInCache(t *testing.T) {
	// ReadSlot → ErrSlotNotFound → service ErrPluginNotInCache → plugin-not-in-cache.
	h, rec := newSigilHandler(t, sigilAdminCfg(), &fakeSigilStore{}, fakeSigilSlots{err: pluginhost.ErrSlotNotFound})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.allow",
		`{"namespace":"cloud","name":"ghost","ref":"v1.0.0"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodePluginNotInCache {
		t.Errorf("code = %q, want plugin-not-in-cache", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed allow must not write audit")
	}
}

func TestPluginAllow_AlreadyActive(t *testing.T) {
	// Insert → ErrSigilAlreadyActive → sigil-already-active.
	store := &fakeSigilStore{insertErr: sigil.ErrSigilAlreadyActive}
	h, rec := newSigilHandler(t, sigilAdminCfg(), store, fakeSigilSlots{slot: sigilSlotFixture()})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.allow",
		`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeSigilActive {
		t.Errorf("code = %q, want sigil-already-active", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed allow must not write audit")
	}
}

// --- tests: revoke ---

func TestPluginRevoke_Success(t *testing.T) {
	h, rec := newSigilHandler(t, sigilAdminCfg(), &fakeSigilStore{}, fakeSigilSlots{})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.revoke",
		`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventPluginRevoked))
	if ev.Payload["namespace"] != "cloud" || ev.Payload["name"] != "hetzner" || ev.Payload["ref"] != "v1.0.0" {
		t.Errorf("audit triple = %+v", ev.Payload)
	}
}

func TestPluginRevoke_NotFound(t *testing.T) {
	store := &fakeSigilStore{revokeErr: sigil.ErrSigilNotFound}
	h, rec := newSigilHandler(t, sigilAdminCfg(), store, fakeSigilSlots{})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.revoke",
		`{"namespace":"cloud","name":"hetzner","ref":"v9.9.9"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeSigilNotFound {
		t.Errorf("code = %q, want sigil-not-found", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed revoke must not write audit")
	}
}

// --- tests: list ---

func TestPluginList_Success(t *testing.T) {
	now := time.Now()
	store := &fakeSigilStore{listResult: []*sigil.Sigil{
		{Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", SHA256: "abc",
			AllowedByAID: "archon-alice", AllowedAt: now,
			// signature/manifest are present in the record, but List doesn't return them.
			Signature: []byte("sig"), Manifest: []byte(`{"k":"v"}`)},
	}}
	h, _ := newSigilHandler(t, sigilAdminCfg(), store, fakeSigilSlots{})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out pluginListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Sigils) != 1 {
		t.Fatalf("sigils = %d, want 1", len(out.Sigils))
	}
	s := out.Sigils[0]
	if s.Namespace != "cloud" || s.Name != "hetzner" || s.Ref != "v1.0.0" || s.SHA256 != "abc" {
		t.Errorf("sigil = %+v", s)
	}
	// signature/manifest must NOT appear in the JSON output (checked against raw JSON).
	if got := string(res.StructuredContent); strings.Contains(got, "signature") || strings.Contains(got, "manifest") || strings.Contains(got, "\"sig\"") {
		t.Errorf("list output leaks signature/manifest: %s", got)
	}
}

// TestPluginList_EmptyNonNil — an empty registry serializes as [], not null.
func TestPluginList_EmptyNonNil(t *testing.T) {
	h, _ := newSigilHandler(t, sigilAdminCfg(), &fakeSigilStore{listResult: nil}, fakeSigilSlots{})
	resp := callTool(t, h, "archon-alice", "keeper.plugin.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(string(res.StructuredContent), `"sigils":[]`) {
		t.Errorf("empty list must be [], got: %s", res.StructuredContent)
	}
}

// --- decode helpers ---

func decodePluginAllowOutput(t *testing.T, resp jsonRPCResponse) pluginAllowOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out pluginAllowOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}
