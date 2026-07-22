package mcp

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

func TestMapIncarnationErrorToMCP(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"nil", nil, ""},
		{"not-found", incarnation.ErrIncarnationNotFound, mcpCodeNotFound},
		{"already-exists", incarnation.ErrIncarnationAlreadyExists, mcpCodeIncarnationExists},
		{"not-locked", incarnation.ErrIncarnationNotLocked, mcpCodeIncarnationLocked},
		{"busy", incarnation.ErrIncarnationBusy, mcpCodeIncarnationLocked},
		{"locked", incarnation.ErrIncarnationLocked, mcpCodeIncarnationLocked},
		{"downgrade-tx", incarnation.ErrDowngradeUnsupported, mcpCodeIncarnationLocked},
		{"downgrade-ref", incarnation.ErrDowngradeViaRef, mcpCodeIncarnationLocked},
		{"schema-mismatch", incarnation.ErrSchemaVersionMismatch, mcpCodeIncarnationLocked},
		{"upgrade-noop", incarnation.ErrUpgradeNoop, mcpCodeValidationFailed},
		{"chain-broken", artifact.ErrMigrationChainBroken, mcpCodeValidationFailed},
		{"service-not-registered", incarnation.ErrServiceNotRegistered, mcpCodeInternalError},
		{"load-snapshot", incarnation.ErrLoadTargetSnapshot, mcpCodeInternalError},
		{"snapshot-invalid", incarnation.ErrTargetSnapshotInvalid, mcpCodeInternalError},
		{"chain-load", incarnation.ErrLoadMigrationChain, mcpCodeInternalError},
		{"build-evaluator", incarnation.ErrBuildEvaluator, mcpCodeInternalError},
		{"rbac-denied", rbac.ErrPermissionDenied, mcpCodeForbidden},
		{"unknown", errors.New("boom"), mcpCodeInternalError},
		// Wrapped %w errors (PrepareUpgrade wraps sentinels) must also be
		// recognized via errors.Is.
		{"wrapped-noop", fmt.Errorf("%w: v2", incarnation.ErrUpgradeNoop), mcpCodeValidationFailed},
		{"wrapped-downgrade", fmt.Errorf("%w: v1", incarnation.ErrDowngradeViaRef), mcpCodeIncarnationLocked},
		{"wrapped-load", fmt.Errorf("%w: git boom", incarnation.ErrLoadTargetSnapshot), mcpCodeInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, detail := mapIncarnationErrorToMCP(tc.err)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.err != nil && detail == "" {
				t.Errorf("detail is empty for %v", tc.err)
			}
			// Public detail must not leak the internal package prefix.
			if len(detail) >= 13 && detail[:13] == "incarnation: " {
				t.Errorf("detail leaks internal prefix: %q", detail)
			}
		})
	}
}

// mcpToolsDocPath — path to the normative § Errors table relative to the
// package directory (keeper/internal/mcp). docs/ is the repo root, outside
// the keeper module, but this is a filesystem path, not a go import.
const mcpToolsDocPath = "../../../docs/keeper/mcp-tools.md"

// TestReservedMCPCodes_PresentInDocs — code↔doc invariant: every reserved
// URN suffix in [reservedMCPCodes] must appear in the docs/keeper/mcp-tools.md
// § Errors table. Catches drift ("code declared in Go but missing from
// docs", and the reverse is visible on review). Also keeps reservedMCPCodes
// actually consumed.
func TestReservedMCPCodes_PresentInDocs(t *testing.T) {
	raw, err := os.ReadFile(mcpToolsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpToolsDocPath, err)
	}
	doc := string(raw)
	for _, code := range reservedMCPCodes {
		// Codes in the docs are backtick-wrapped (`migration-failed`) in the
		// § Errors table — look for exactly that form to avoid matching a
		// stray substring in prose.
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("reserved MCP code %q not documented in %s § Errors", code, mcpToolsDocPath)
		}
	}
}

func TestMapRoleErrorToMCP(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"nil", nil, ""},
		{"role-not-found", rbac.ErrRoleNotFound, mcpCodeNotFound},
		{"membership-not-found", rbac.ErrRoleOperatorNotFound, mcpCodeNotFound},
		{"operator-not-found", rbac.ErrOperatorNotFound, mcpCodeNotFound},
		{"role-exists", rbac.ErrRoleAlreadyExists, mcpCodeRoleExists},
		{"role-builtin", rbac.ErrRoleBuiltin, mcpCodeRoleBuiltin},
		{"lockout", rbac.ErrWouldLockOutCluster, mcpCodeWouldLockOutCluster},
		{"invalid-name", rbac.ErrInvalidRoleName, mcpCodeValidationFailed},
		{"rbac-denied", rbac.ErrPermissionDenied, mcpCodeForbidden},
		{"unknown", errors.New("boom"), mcpCodeInternalError},
		// Wrapped %w errors (CreateRole/DeleteRole wrap sentinels) are
		// recognized via errors.Is.
		{"wrapped-exists", fmt.Errorf("%w (constraint x): pg", rbac.ErrRoleAlreadyExists), mcpCodeRoleExists},
		{"wrapped-name", fmt.Errorf("%w: %q must match", rbac.ErrInvalidRoleName, "Bad"), mcpCodeValidationFailed},
		{"wrapped-operator", fmt.Errorf("%w (constraint y): pg", rbac.ErrOperatorNotFound), mcpCodeNotFound},
		// A malformed permission is a wrapped ParsePermission error with no
		// sentinel — caught by the "rbac: invalid permission " prefix.
		{"bad-permission", fmt.Errorf("rbac: invalid permission %q: %w", "boom.x.y", errors.New("three segments")), mcpCodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, detail := mapRoleErrorToMCP(tc.err)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.err != nil && detail == "" {
				t.Errorf("detail is empty for %v", tc.err)
			}
			// Public detail must not leak the internal package prefix "rbac: ".
			if strings.HasPrefix(detail, "rbac: ") {
				t.Errorf("detail leaks internal prefix: %q", detail)
			}
		})
	}
}

func TestMapSigilErrorToMCP(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"nil", nil, ""},
		{"not-in-cache", sigil.ErrPluginNotInCache, mcpCodePluginNotInCache},
		{"already-active", sigil.ErrSigilAlreadyActive, mcpCodeSigilActive},
		{"not-found", sigil.ErrSigilNotFound, mcpCodeSigilNotFound},
		{"unknown", errors.New("boom"), mcpCodeInternalError},
		// Wrapped %w errors (Allow wraps ErrPluginNotInCache) are recognized
		// via errors.Is.
		{"wrapped-not-in-cache", fmt.Errorf("%w: cloud-ghost", sigil.ErrPluginNotInCache), mcpCodePluginNotInCache},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, detail := mapSigilErrorToMCP(tc.err)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.err != nil && detail == "" {
				t.Errorf("detail is empty for %v", tc.err)
			}
			if strings.HasPrefix(detail, "sigil: ") {
				t.Errorf("detail leaks internal prefix: %q", detail)
			}
		})
	}
}

func TestIncarnationRBACContext(t *testing.T) {
	// name-bound tools → {"incarnation": name}.
	got := incarnationRBACContext("redis-prod")
	if got == nil || got["incarnation"] != "redis-prod" {
		t.Fatalf("name-bound context = %v, want {incarnation: redis-prod}", got)
	}
	// create/list — no selector.
	if incarnationRBACContext("") != nil {
		t.Errorf("empty name must yield nil selector (NoSelector parity with REST)")
	}
}
