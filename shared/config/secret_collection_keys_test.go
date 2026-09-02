package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// usersSchema is the wb-service-redis shape: a collection whose elements each carry
// a `password` declared `type: secret` and addressed by the `name` sibling.
func usersSchema(t *testing.T) InputSchemaMap {
	t.Helper()
	return stateSchema(t, `
redis_users:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      perms:    { type: string }
      password: { type: secret, key: name }
# Same shape, no declared secret anywhere in it: a repeated name here derives no
# path and must stay quiet.
plain_users:
  type: array
  items:
    type: object
    properties:
      name: { type: string }
admin_password: { type: secret }
`)
}

func captureTask(field string, value any) Task {
	return Task{Name: "capture", On: "keeper", Module: &ModuleTask{
		Module: "core.state.set",
		Params: map[string]any{"field": field, "value": value},
	}}
}

// The mistake this catches is a copy-pasted users block, so the assertion is on the
// whole diagnostic an author reads: the code the gate matches on, and a message and
// path that name the SECOND element and the one it collides with.
func TestScanDuplicateSecretKeys(t *testing.T) {
	got := ScanDuplicateSecretKeys("scenario/update_users/main.yml", usersSchema(t), []Task{captureTask("redis_users", []any{
		map[string]any{"name": "alice", "perms": "+@read"},
		map[string]any{"name": "bob", "perms": "+@read"},
		map[string]any{"name": "alice", "perms": "+@write"},
	})})
	if len(got) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one %s", got, SecretKeyDuplicateCode)
	}
	d := got[0]
	if d.Code != SecretKeyDuplicateCode || d.Level != diag.LevelError {
		t.Errorf("code/level = %s/%s, want %s/%s", d.Code, d.Level, SecretKeyDuplicateCode, diag.LevelError)
	}
	if want := "$.tasks[0].params.value[2].name"; d.YAMLPath != want {
		t.Errorf("YAMLPath = %q, want %q", d.YAMLPath, want)
	}
	for _, want := range []string{"[2]", "[0]", "alice", "redis_users.password"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message does not name %q: %s", want, d.Message)
		}
	}
}

// What must NOT be reported. Each of these is either legitimate or undecidable
// offline, and a linter that refuses them is a linter the corpus turns off.
func TestScanDuplicateSecretKeysQuiet(t *testing.T) {
	cases := map[string][]Task{
		"distinct keys": {captureTask("redis_users", []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		})},
		// The normal shape of a computed collection: every key is an expression, and
		// what it renders to is known only at apply.
		"interpolated key": {captureTask("redis_users", []any{
			map[string]any{"name": "${ input.user }"},
			map[string]any{"name": "${ input.user }"},
		})},
		// Not a path segment, so it derives nothing to collide on -- apply refuses
		// it as an unsafe segment, which is a different diagnostic and not this
		// rule's to make.
		"unsafe key": {captureTask("redis_users", []any{
			map[string]any{"name": "../../keeper"},
			map[string]any{"name": "../../keeper"},
		})},
		"non-string key": {captureTask("redis_users", []any{
			map[string]any{"name": 7},
			map[string]any{"name": 7},
		})},
		"key absent": {captureTask("redis_users", []any{
			map[string]any{"perms": "+@read"},
			map[string]any{"perms": "+@read"},
		})},
		// A repeated value in a field with no declared secret derives no path.
		"another field": {captureTask("plain_users", []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "alice"},
		})},
		// A scalar secret has no key at all.
		"scalar secret": {captureTask("admin_password", "${ generate_secret({}) }")},
		// `add`/`append` hand over ONE element; a collision against the STORED
		// collection is not visible here.
		"single element": {{Name: "capture", On: "keeper", Module: &ModuleTask{
			Module: "core.state.add",
			Params: map[string]any{"field": "redis_users", "key": "alice", "value": map[string]any{"name": "alice"}},
		}}},
		// `remove` declares no `value:` at all, so a list there is a param error
		// and the elements it holds are a collection no run ever forms.
		"verb that takes no value": {{Name: "capture", On: "keeper", Module: &ModuleTask{
			Module: "core.state.remove",
			Params: map[string]any{"field": "redis_users", "value": []any{
				map[string]any{"name": "alice"},
				map[string]any{"name": "alice"},
			}},
		}}},
		"not a capture": {{Name: "run", Module: &ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"field": "redis_users", "value": []any{
				map[string]any{"name": "alice"},
				map[string]any{"name": "alice"},
			}},
		}}},
	}
	for name, tasks := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ScanDuplicateSecretKeys("main.yml", usersSchema(t), tasks); len(got) != 0 {
				t.Fatalf("diagnostics = %+v, want none", got)
			}
		})
	}
}

// A capture inside a `block:` writes the same field through the same module.
func TestScanDuplicateSecretKeysBlock(t *testing.T) {
	got := ScanDuplicateSecretKeys("main.yml", usersSchema(t), []Task{{
		Name: "guarded",
		Block: &BlockTask{Block: []Task{captureTask("redis_users", []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "alice"},
		})}},
	}})
	if len(got) != 1 {
		t.Fatalf("diagnostics = %+v, want one", got)
	}
	if want := "$.tasks[0].block[0].params.value[1].name"; got[0].YAMLPath != want {
		t.Errorf("YAMLPath = %q, want %q", got[0].YAMLPath, want)
	}
}

// What must be unique is the (key, property) pair the path is derived from, not the
// element's identity: two secrets of one element may be addressed by DIFFERENT
// siblings, and then the same text is legitimately a key twice.
func TestScanDuplicateSecretKeysPerDeclaredSecret(t *testing.T) {
	schema := stateSchema(t, `
accounts:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      alias:    { type: string }
      password: { type: secret, key: name }
      token:    { type: secret, key: alias }
`)
	got := ScanDuplicateSecretKeys("main.yml", schema, []Task{captureTask("accounts", []any{
		map[string]any{"name": "alice", "alias": "ops"},
		map[string]any{"name": "ops", "alias": "alice"},
	})})
	if len(got) != 0 {
		t.Fatalf("diagnostics = %+v, want none: the paths are all distinct", got)
	}
}

// No schema, no check — a scenario linted without its service.yml must not be
// refused, the same asymmetry [ScanOwnNamespaceVault] carries for the service name.
func TestScanDuplicateSecretKeysNoSchema(t *testing.T) {
	tasks := []Task{captureTask("redis_users", []any{
		map[string]any{"name": "alice"},
		map[string]any{"name": "alice"},
	})}
	if got := ScanDuplicateSecretKeys("main.yml", nil, tasks); len(got) != 0 {
		t.Fatalf("diagnostics = %+v, want none", got)
	}
}
