package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// dirInputLoader — [InputScenarioLoader] over a REAL snapshot directory on
// disk. Unlike fakeInputLoader (a single in-memory YAML), this needs a real
// LocalDir: $type resolution reads a sibling types.yml from the snapshot
// (artifact.LoadScenarioManifestResolved → readSnapshotFile via art.LocalDir),
// not through ReadFile. ReadFile also goes through disk, so main.yml and
// types.yml are read from the same tree.
type dirInputLoader struct{ root string }

func (l *dirInputLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref, LocalDir: l.root}, nil
}

func (l *dirInputLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return artifact.ReadSnapshotFile(l.root, file)
}

// writeServiceWithTypes materializes a minimal service snapshot on disk:
// scenario/create/main.yml + types.yml at the root. Returns the snapshot root.
func writeServiceWithTypes(t *testing.T, mainYAML, typesYAML string) string {
	t.Helper()
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatalf("mkdir scenario: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(mainYAML), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if typesYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "types.yml"), []byte(typesYAML), 0o644); err != nil {
			t.Fatalf("write types.yml: %v", err)
		}
	}
	return root
}

// typesAclUser — a type catalog with AclUser: object with required field `name`
// (a required list on the object node) and optional `read_only`. The shape is
// validated ONLY after $type resolution on the runtime path.
const typesAclUser = `types:
  AclUser:
    type: object
    required: [name]
    properties:
      name:
        type: string
      read_only:
        type: boolean
`

// scenarioUsersOfType — input.users = array of $type:AclUser. The items node
// carries a type reference; before resolution its Type is empty →
// ResolveInputValues would pass a submitted element WITHOUT shape validation
// (this is the MAJOR hole).
const scenarioUsersOfType = `name: create
input:
  users:
    type: array
    items:
      $type: AclUser
tasks: []
`

// TestValidateInput_TypeRef_NonObjectElement_Rejected — the MAIN MAJOR guard: a
// submitted $type:AclUser array element that is NOT an object (a string) is
// REJECTED on the RUNTIME path ValidateInput → ResolveInputValues. Proves that
// $type is resolved at load time (without resolution the node would have an
// empty Type and the value would pass silently).
func TestValidateInput_TypeRef_NonObjectElement_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{"not-an-object"}})
	if err == nil {
		t.Fatal("submitted не-object для $type:AclUser должен быть отклонён, got nil (резолв $type не подключён?)")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (value-валидация против резолвнутой type-формы), got %v", err)
	}
}

// TestValidateInput_TypeRef_MissingRequired_Rejected — an object element
// missing AclUser's required field `name` is rejected. Proves enforcement of
// required fields INSIDE the resolved type (not just a type-match on the top
// node).
func TestValidateInput_TypeRef_MissingRequired_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{map[string]any{"read_only": true}}})
	if err == nil {
		t.Fatal("AclUser без required name должен быть отклонён, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (required name внутри резолвнутого AclUser), got %v", err)
	}
}

// TestValidateInput_TypeRef_ValidElement_OK — a valid AclUser (object with
// name) passes. Closes the guard: resolution does NOT break valid input,
// enforcement works both ways (rejects broken, accepts correct).
func TestValidateInput_TypeRef_ValidElement_OK(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{map[string]any{"name": "alice", "read_only": true}}})
	if err != nil {
		t.Fatalf("валидный AclUser должен проходить после резолва $type: %v", err)
	}
}

// TestValidateInput_TypeRef_StandaloneField_NonObject_Rejected — $type as a
// STANDALONE field (not under items): a submitted non-object is rejected.
// Covers the second reference form (`<param>: { $type: T }`), symmetric to
// array-of-type.
func TestValidateInput_TypeRef_StandaloneField_NonObject_Rejected(t *testing.T) {
	const scn = `name: create
input:
  owner:
    $type: AclUser
tasks: []
`
	root := writeServiceWithTypes(t, scn, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"owner": "not-an-object"})
	if err == nil {
		t.Fatal("самостоятельное $type:AclUser-поле с non-object должно быть отклонено, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid, got %v", err)
	}
}

// typesAclUserPerms — a catalog with AclUser carrying a pattern on perms
// (token-shape filter for Redis ACL, examples/service/redis/types.yml). The
// pattern is checked on the RUNTIME path ValidateInput → ResolveInputValues →
// validateValueAt for each element of the users array. Source of the pattern is
// the redis service's types.yml; duplicated 1:1 here as the guard test's canon.
const typesAclUserPerms = `types:
  AclUser:
    type: object
    required: [name, perms]
    properties:
      name:
        type: string
        pattern: "^[a-z][a-z0-9_-]*$"
      perms:
        type: string
        pattern: "^(?:(?:(?:on|off|nopass|resetpass|reset|clearselectors|sanitize-payload|skip-sanitize-payload|nosanitize-payload|allkeys|allchannels|allcommands|nocommands)|(?:%R~|%W~|%RW~|~)[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|&[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|[+-](?:@[a-z][a-z0-9-]*|[a-z][a-z0-9-]*(?:\\|[a-z][a-z0-9-]*)?)|[><][A-Za-z0-9:_.@%/+=-]+|[#!][0-9a-fA-F]{64}|\\([^()]*\\))(?: (?:(?:on|off|nopass|resetpass|reset|clearselectors|sanitize-payload|skip-sanitize-payload|nosanitize-payload|allkeys|allchannels|allcommands|nocommands)|(?:%R~|%W~|%RW~|~)[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|&[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|[+-](?:@[a-z][a-z0-9-]*|[a-z][a-z0-9-]*(?:\\|[a-z][a-z0-9-]*)?)|[><][A-Za-z0-9:_.@%/+=-]+|[#!][0-9a-fA-F]{64}|\\([^()]*\\)))*)?$"
      state:
        type: string
        default: "on"
        enum: [on, off]
`

// TestValidateInput_AclUserPerms_GarbageRejected — the token-shape pattern on
// AclUser.perms rejects garbage/injections on the RUNTIME path ValidateInput.
// Each case is an operator input.users with one element; perms is a
// non-Redis-ACL string. Proves the pattern is enforced on $type:AclUser array
// elements (the prod path $type resolution → ResolveInputValues →
// validateValueAt), not only in lint.
func TestValidateInput_AclUserPerms_GarbageRejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	cases := map[string]string{
		"произвольный текст":        "произвольный текст",
		"shell-инъекция через ;":    "~* +@all; rm -rf /",
		"перевод строки":            "~app:* +@read\n~evil:* +@all",
		"неизвестный сигил =":       "~* =foo",
		"pipe вне сабкоманды":       "~* +@all | cat",
		"backtick":                  "~* `whoami`",
		"command substitution":      "~* $(id)",
		"ключевое-слово без сигила": "hello",
	}
	for name, perms := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
				map[string]any{"users": []any{map[string]any{"name": "app", "perms": perms, "state": "on"}}})
			if err == nil {
				t.Fatalf("мусорная perms %q должна быть отклонена pattern-ом, got nil", perms)
			}
			if !errors.Is(err, ErrInputInvalid) {
				t.Fatalf("ожидался ErrInputInvalid (perms не соответствует pattern), got %v", err)
			}
		})
	}
}

// TestValidateInput_AclUserPerms_ValidAccepted — valid Redis ACL strings PASS
// the pattern: point permissions, broad ones (allkeys +@all), and REAL system
// strings from essence (replica/monitoring/sentinel/haproxy, with hyphenated
// subcommands like sentinel|is-master-down-by-addr). Closes the guard — the
// pattern doesn't false-reject.
func TestValidateInput_AclUserPerms_ValidAccepted(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	valid := []string{
		"~app:* +@read +@write -@dangerous",
		"~* +@all",
		"allkeys +@all",
		"+psync +replconf +ping", // system replica
		"-@all +@connection +client +ping +info +config|get +cluster|info +slowlog +latency +memory +select +command|count +command|docs",   // system monitoring
		"allchannels +multi +slaveof +ping +exec +subscribe +config|rewrite +role +publish +info +client|setname +client|kill +script|kill", // system sentinel
		"-@all +auth +client|getname +client|id +client|setname +command +hello +ping +role +info +cluster|info",                            // system haproxy
		"allchannels -@all +auth +sentinel|is-master-down-by-addr +sentinel|get-master-addr-by-name +sentinel|myid",                         // hyphenated subcommands
		// NB: pattern allows an empty string (a permissionless off-user), but perms
		// in required:[name,perms] → empty is rejected by required logic BEFORE
		// pattern. A permissionless user is expressed via the `off` token, not an
		// empty string.
		"off",
	}
	for _, perms := range valid {
		err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
			map[string]any{"users": []any{map[string]any{"name": "app", "perms": perms, "state": "on"}}})
		if err != nil {
			t.Fatalf("валидная perms %q должна проходить pattern, got %v", perms, err)
		}
	}
}

// scenarioRequiredTypeField — a standalone field `user: {$type: AclUser}` with
// field-level `required: true`. After $type resolution the node carries the
// type's shape (object + RequiredProps) AND the overlaid Required=true carried
// over by reference (applyRefOverlay, ADR-062). Omitting `user` must fail on
// requireInputValues.
const scenarioRequiredTypeField = `name: create
input:
  user:
    $type: AclUser
    required: true
tasks: []
`

// scenarioOptionalTypeField — the same $type:AclUser node WITHOUT required.
// Omitting `user` passes: required isn't set, no default → absence is legal.
const scenarioOptionalTypeField = `name: create
input:
  user:
    $type: AclUser
tasks: []
`

// TestValidateInput_TypeRef_RequiredField_Omitted_Rejected — regression guard
// for backend enforcement of required on a $type-resolved object node (NIM-72):
// an omitted `user` field with `required: true` gives ErrInputInvalid on the
// RUNTIME path ValidateInput → ResolveInputValues → requireInputValues. A
// rollback of the enforcement (reading s.Required on the post-resolution node,
// input_value.go) would silently allow creating an incarnation without a
// required field — this test catches that regression.
func TestValidateInput_TypeRef_RequiredField_Omitted_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioRequiredTypeField, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{}) // user omitted
	if err == nil {
		t.Fatal("required $type:AclUser-поле без значения должно быть отклонено, got nil (энфорсмент required снят?)")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (required на резолвнутом object-узле), got %v", err)
	}
}

// TestValidateInput_TypeRef_OptionalField_Omitted_OK — the paired case: the
// same $type:AclUser node WITHOUT required, omitted, PASSES. Closes the guard —
// enforcement doesn't false-reject optional type fields (required fires exactly
// from Required=true, not merely from the resolved type having RequiredProps).
func TestValidateInput_TypeRef_OptionalField_Omitted_OK(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioOptionalTypeField, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{}) // user omitted — legal for an optional field
	if err != nil {
		t.Fatalf("опциональное $type:AclUser-поле без значения должно проходить, got %v", err)
	}
}
