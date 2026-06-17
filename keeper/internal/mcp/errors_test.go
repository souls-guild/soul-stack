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
		// Обёрнутые %w-ошибки (PrepareUpgrade оборачивает sentinel-ы) тоже
		// должны распознаваться через errors.Is.
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
			// Public-detail не должен протекать internal-pkg-префиксом.
			if len(detail) >= 13 && detail[:13] == "incarnation: " {
				t.Errorf("detail leaks internal prefix: %q", detail)
			}
		})
	}
}

// mcpToolsDocPath — путь к нормативной таблице § Errors относительно
// директории пакета (keeper/internal/mcp). docs/ — корень репо, вне
// keeper-модуля, но это файловая система, не go-import.
const mcpToolsDocPath = "../../../docs/keeper/mcp-tools.md"

// TestReservedMCPCodes_PresentInDocs — инвариант code↔doc: каждый
// зарезервированный URN-suffix из [reservedMCPCodes] обязан присутствовать в
// таблице § Errors docs/keeper/mcp-tools.md. Ловит drift «код объявлен в Go,
// но в доках забыт» (и обратное — при ревью видно расхождение). Заодно делает
// reservedMCPCodes реально потребляемой переменной.
func TestReservedMCPCodes_PresentInDocs(t *testing.T) {
	raw, err := os.ReadFile(mcpToolsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpToolsDocPath, err)
	}
	doc := string(raw)
	for _, code := range reservedMCPCodes {
		// Коды в доке записаны в backtick-обёртке (`migration-failed`) в
		// таблице § Errors — ищем именно эту форму, чтобы не словить
		// случайное вхождение подстроки в прозе.
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
		// Обёрнутые %w-ошибки (CreateRole/DeleteRole оборачивают sentinel-ы)
		// распознаются через errors.Is.
		{"wrapped-exists", fmt.Errorf("%w (constraint x): pg", rbac.ErrRoleAlreadyExists), mcpCodeRoleExists},
		{"wrapped-name", fmt.Errorf("%w: %q must match", rbac.ErrInvalidRoleName, "Bad"), mcpCodeValidationFailed},
		{"wrapped-operator", fmt.Errorf("%w (constraint y): pg", rbac.ErrOperatorNotFound), mcpCodeNotFound},
		// Битый permission — wrapped ParsePermission без sentinel-а, ловится по
		// префиксу "rbac: invalid permission ".
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
			// Public-detail не должен протекать internal-pkg-префиксом "rbac: ".
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
		// Обёрнутые %w-ошибки (Allow оборачивает ErrPluginNotInCache) распознаются
		// через errors.Is.
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
	// create/list — без селектора.
	if incarnationRBACContext("") != nil {
		t.Errorf("empty name must yield nil selector (NoSelector parity with REST)")
	}
}
