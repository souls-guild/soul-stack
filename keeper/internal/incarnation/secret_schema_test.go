package incarnation

import (
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// stateSchemaFixture parses a `state_schema:` body written in the input dialect
// ([NIM-740]) through the real decoder, so the fixture is a shape the parser actually
// produces rather than one assembled by hand.
func stateSchemaFixture(t *testing.T, src string) config.InputSchemaMap {
	t.Helper()
	var m config.InputSchemaMap
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("state_schema fixture does not parse: %v\n%s", err, src)
	}
	return m
}

// CollectStateSchemaSecrets walks a state_schema for secret:true (nesting via
// properties/items/additional_properties).
func TestCollectStateSchemaSecrets(t *testing.T) {
	schema := stateSchemaFixture(t, `
admin_token: { type: string, secret: true }
replicas:    { type: integer }
tls:
  type: object
  properties:
    key:  { type: string, secret: true }
    port: { type: integer }
acl:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	for _, want := range []string{"admin_token", "tls.key", "acl[].password"} {
		if !set[want] {
			t.Errorf("secret path %q not collected: %v", want, set)
		}
	}
	if set["replicas"] || set["tls.port"] || set["acl[].name"] {
		t.Errorf("non-secret path marked — over-collect: %v", set)
	}
}

// secret ON THE additionalProperties node itself (the value of an arbitrary map key is
// secret) does NOT enter SecretPathSet: neither `map_field` (would mark the whole map →
// over-mask on read-path) nor `map_field.*` (IsSecret never asks for such a path →
// dead entry). Degradation to the vault+regex masking layer is intentional (★ limitation
// of the schema layer). Regression guard for the ap-secret branch of CollectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesSecretLeaf(t *testing.T) {
	schema := stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	if set["map_field"] {
		t.Errorf("ap-secret-leaf marked `map_field` — over-mask of the whole map: %v", set)
	}
	if set["map_field.*"] {
		t.Errorf("ap-secret-leaf marked `map_field.*` — dead entry (IsSecret never queries this path): %v", set)
	}
	if len(set) != 0 {
		t.Errorf("ap-secret-leaf should not produce any entry (degradation to vault+regex): %v", set)
	}
}

// ap node WITHOUT secret but with nested concrete `properties` that are secret: the schema
// layer MUST cover the exact names (recursion into ap runs), but not the ap node itself.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret(t *testing.T) {
	schema := stateSchemaFixture(t, `
users:
  type: object
  additional_properties:
    type: object
    properties:
      name:     { type: string }
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	// ap-path = map name (`users`), nested concrete `password` → `users.password`.
	if !set["users.password"] {
		t.Errorf("nested concrete secret under ap not collected: %v", set)
	}
	if set["users"] {
		t.Errorf("the ap-node `users` itself marked secret — over-mask: %v", set)
	}
}

// ★ Documents a GAP (seal-review nit, NOT a fix): the collected schema path
// `users.password` does NOT match the real cell path `users.<dynamic-key>.password`.
// Recursion into ap does not insert a segment for an arbitrary key → `users.password`
// is collected, while maskMapLayered walks the path by the CONCRETE map key
// (`users.alice.password`). [audit.SecretPathSet.IsSecret] compares the exact shape AND
// normalizeIdx — but normalizeIdx only generalizes slice indices (`[N]`→`[]`), not
// map keys → no match. The schema layer does NOT mask such a secret (degradation to
// vault+regex by the name `password`). The test pins CURRENT behavior: once dynamic-key
// matching lands in IsSecret (a separate slice), the `!IsSecret(...)` assert will fail —
// a signal to update the limitation in CollectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret_DynamicKeyGap(t *testing.T) {
	schema := stateSchemaFixture(t, `
users:
  type: object
  additional_properties:
    type: object
    properties:
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	// The ap-path is collected without a dynamic-key segment.
	if !set["users.password"] {
		t.Fatalf("expected collected path `users.password`: %v", set)
	}
	// ★ Current behavior: the real cell path with a CONCRETE map key does NOT match the
	// schema layer (the dynamic-key segment `alice` is not covered; normalizeIdx leaves it alone).
	if set.IsSecret("users.alice.password") {
		t.Errorf("IsSecret(users.alice.password) = true — gap unexpectedly closed; update the limitation in CollectStateSchemaSecrets")
	}
	// Control: idx generalization does not help — a map key is not a slice index.
	if set.IsSecret("users.bob.password") {
		t.Errorf("IsSecret(users.bob.password) = true — gap unexpectedly closed; update the limitation in CollectStateSchemaSecrets")
	}
}

// `type: secret` sitting ON an additional_properties node is still entered into the
// path set; only the older `secret: true` FLAG is ignored there.
//
// The distinction is not cosmetic. The flag's path is the MAP's, so honouring it would
// mark the whole map (the ★ limitation above). `type: secret` gets the same path, and
// the older raw-map walk stripped the `secret` key but left the type, so the entry WAS
// made. This layer is belt and braces — a declared secret should never be in state at
// all, and config.CollectSecretFields refuses one in this position — which is exactly
// why it must not quietly narrow when nobody is looking.
func TestCollectStateSchemaSecrets_AdditionalPropertiesTypeSecretStillMarked(t *testing.T) {
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: secret }
`), "", set)
	if !set["map_field"] {
		t.Errorf("`type: secret` on an ap node was not marked: %v", set)
	}

	// The flag, by contrast, is ignored there — pinned separately above, restated here
	// so the two cases sit next to each other and cannot be conflated by a later edit.
	flagSet := audit.SecretPathSet{}
	CollectStateSchemaSecrets(stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: string, secret: true }
`), "", flagSet)
	if len(flagSet) != 0 {
		t.Errorf("`secret: true` on an ap node was marked — over-mask of the whole map: %v", flagSet)
	}
}

// ★ NIM-826 — the seal's producer. `${ incarnation.state.<field> }` can only be
// addressed at the TOP segment, so a nested or array secret contributes its
// field name and seals that subtree; a service declaring nothing secret
// contributes nil, which is what leaves an ordinary run's diagnostics unmasked.
func TestStateSchemaSecretFields(t *testing.T) {
	art := &artifact.ServiceArtifact{Manifest: &config.ServiceManifest{
		StateSchema: stateSchemaFixture(t, `
admin_token: { type: string, secret: true }
replicas:    { type: integer }
port:        { type: string }
tls:
  type: object
  properties:
    key:  { type: string, secret: true }
    cert: { type: string }
acl:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      password: { type: string, secret: true }
vault_ref: { type: secret }
`),
	}}

	got := StateSchemaSecretFields(art)

	// `vault_ref` is a top-level scalar `type: secret`: its address is exact, so
	// taking it widens nothing — see TestStateSchemaSecretFields_DeclaredSecret
	// DoesNotSealItsCollection for the shape where it would.
	want := map[string]bool{"admin_token": true, "tls": true, "acl": true, "vault_ref": true}
	for name := range want {
		if !got[name] {
			t.Errorf("state field %q is not a seal address: %v — a cell reading it renders plaintext", name, got)
		}
	}
	for _, name := range []string{"replicas", "port", "key", "password", "cert"} {
		if got[name] {
			t.Errorf("%q became a seal address: %v — either a non-secret field or a LEAF that CEL cannot address", name, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("StateSchemaSecretFields = %v, want exactly %v", got, want)
	}
}

// ★ THE OVER-SEAL GUARD, and the reason the seal decides per ADDRESS rather than
// per marker. ADR-0083 §1 permits `type: secret` in two positions, and they land on
// opposite sides of the rule: a top-level scalar folds to itself, while a property
// of a top-level array's items folds to the whole ENCLOSING COLLECTION —
// an ACL inventory keyed by user name, which is public and read by every day-2
// scenario.
//
// The first version of this fix inherited every path from the collector and folded
// them all, so on the two flagship example services it masked the inventory out of
// every run plan and sealed nothing that leaks — the whole cost and none of the
// benefit. It was defended by a comment saying "nothing writes there", true of the
// LEAF and false of the segment the code addresses. The correction after that
// dropped the marker entirely, which threw away the scalar case for no gain.
func TestStateSchemaSecretFields_DeclaredSecretDoesNotSealItsCollection(t *testing.T) {
	art := &artifact.ServiceArtifact{Manifest: &config.ServiceManifest{
		// The shape an ACL inventory has: a typed array whose element carries one
		// `type: secret` property keyed by a sibling. Trimmed — a real declaration
		// carries more properties.
		StateSchema: stateSchemaFixture(t, `
redis_users:
  type: array
  items:
    type: object
    properties:
      name:  { type: string, required: true }
      perms: { type: string, required: true }
      password:
        type: secret
        key: name
vault_ref: { type: secret }
db_password: { type: string, secret: true }
`),
	}}

	got := StateSchemaSecretFields(art)

	if got["redis_users"] {
		t.Errorf("`redis_users` is a seal address: %v\n"+
			"Its ONLY secret is `type: secret`, whose value never enters state — so this masks the ACL\n"+
			"inventory out of apply_run_plan.params, status_details and error_summary for every day-2\n"+
			"run of the service, and masks no secret at all.", got)
	}
	if !got["vault_ref"] {
		t.Errorf("a scalar `type: secret` is NOT a seal address: %v\n"+
			"Its address is exact, so taking it masks nothing else — and the value is not guaranteed\n"+
			"absent from the record: the state merge strips it, the UPGRADE write does not, so a\n"+
			"migration can leave a plaintext on that path readable for a whole remediation run.", got)
	}
	if !got["db_password"] {
		t.Errorf("the `secret: true` field is NOT a seal address: %v — narrowing the marker set must not lose the leak this ticket is about", got)
	}

	// The MASKING walk still sees both markers: the two questions diverge here, and
	// a fix that narrowed the mask as well would undo NIM-531.
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	for _, path := range []string{"redis_users[].password", "vault_ref", "db_password"} {
		if !set[path] {
			t.Errorf("mask path %q was lost: %v — the seal's narrower marker set leaked into the mask", path, set)
		}
	}
}

// nil rather than an empty map, so the caller hands the seal an address set that
// is absent rather than present-and-empty — and so the common service, which
// declares no state secret, costs the render nothing.
func TestStateSchemaSecretFields_NilWhenNothingDeclared(t *testing.T) {
	for name, art := range map[string]*artifact.ServiceArtifact{
		"no artifact": nil,
		"no manifest": {},
		"no secret": {Manifest: &config.ServiceManifest{
			StateSchema: config.InputSchemaMap{"port": {Type: "string"}},
		}},
	} {
		if got := StateSchemaSecretFields(art); got != nil {
			t.Errorf("%s: StateSchemaSecretFields = %v, want nil", name, got)
		}
	}
}

// ★ The seal DOES take `secret: true` on an `additional_properties` node, where the
// mask deliberately does not — the two questions diverge in the other direction from
// TestStateSchemaSecretFields_DeclaredSecretDoesNotSealItsCollection, and getting
// this one backwards is a leak rather than an over-seal.
//
// The mask declines because its path vocabulary cannot say "any key"
// (TestCollectStateSchemaSecrets_AdditionalPropertiesSecretLeaf pins that, and its
// degradation to vault+regex is intentional). The seal's unit IS the top segment, so
// "every value of this map is secret" is exactly what it can express. Dropping it
// there would leave a map of state-resident plaintext with no address at all, which
// is precisely the leak this ticket exists for.
func TestStateSchemaSecretFields_TakesTheAdditionalPropertiesFlagTheMaskDeclines(t *testing.T) {
	art := &artifact.ServiceArtifact{Manifest: &config.ServiceManifest{
		StateSchema: stateSchemaFixture(t, `
tokens:
  type: object
  additional_properties: { type: string, secret: true }
port: { type: string }
`),
	}}

	if got := StateSchemaSecretFields(art); !got["tokens"] {
		t.Errorf("`tokens` is not a seal address: %v\n"+
			"Every value of that map is declared `secret: true` and LIVES in state, so\n"+
			"`${ incarnation.state.tokens.<k> }` renders plaintext into apply_run_plan with nothing\n"+
			"marking it — the mask declines this shape too, by its own documented limitation.", got)
	} else if got["port"] {
		t.Errorf("the ap flag spread to a sibling field: %v", got)
	}

	// The mask's behaviour on the same schema is UNCHANGED — the two questions
	// diverge here, and narrowing the mask to match would undo NIM-531's sibling.
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	if len(set) != 0 {
		t.Errorf("the mask now marks something for an ap-secret leaf: %v — that is the pinned "+
			"degradation to vault+regex, not this ticket's to change", set)
	}
}

// ★★ THE DECISION TABLE for this walk, and it exists because three consecutive
// reviews found three defects in one predicate — every one of them the same
// mistake, not three: [isSecretNode] serves TWO questions, and each time the seal
// silently inherited an answer that had only ever been decided for the mask.
//
//	round 1: took every declared secret        -> sealed a public ACL collection off one leaf
//	round 2: dropped `type: secret` entirely    -> lost the scalar, which costs nothing to keep
//	round 3: inherited apNode's ignoreFlag      -> lost a map of state-resident plaintext
//
// So the shapes are enumerated and BOTH answers are written down per row. A new
// shape has no row and fails here; a changed answer has to be typed, next to the
// reason it is being changed from. That is the same instrument as
// shared/cel.activationRoots, pointed at the producer instead of the detector —
// and the reason it works is that a wrong answer becomes a sentence somebody has
// to write, rather than a case nobody looked at.
//
// The two questions and why they legitimately differ:
//
//	MASK — what must never be PRINTED if it is in the record. Addresses the exact
//	path, so saying yes costs nothing and it says yes to nearly everything.
//	SEAL — which cell must be marked because a CEL read takes home plaintext.
//	Addresses only the TOP SEGMENT, so saying yes can cost every sibling under it.
func TestStateSchemaMarkers_EveryShapeDecidedForBothQuestions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schema   string
		wantMask []string
		wantSeal []string
		reason   string
	}{{
		name:     "flag, top-level scalar",
		schema:   `admin_token: { type: string, secret: true }`,
		wantMask: []string{"admin_token"},
		wantSeal: []string{"admin_token"},
		reason:   "the value lives in state and the address is exact -- the plain case this ticket is about",
	}, {
		name:     "flag, nested under properties",
		schema:   "tls:\n  type: object\n  properties:\n    key:  { type: string, secret: true }\n    cert: { type: string }",
		wantMask: []string{"tls.key"},
		wantSeal: []string{"tls"},
		reason:   "the fold takes the public `cert` with it; accepted, because the alternative is a credential in a durable column",
	}, {
		name:     "flag, under items",
		schema:   "acl:\n  type: array\n  items:\n    type: object\n    properties:\n      name:     { type: string }\n      password: { type: string, secret: true }",
		wantMask: []string{"acl[].password"},
		wantSeal: []string{"acl"},
		reason:   "same fold, same acceptance: a `secret: true` element property really is plaintext in the record",
	}, {
		name:     "flag, ON an additional_properties node",
		schema:   "tokens:\n  type: object\n  additional_properties: { type: string, secret: true }",
		wantMask: nil,
		wantSeal: []string{"tokens"},
		reason: "THE ROUND-3 CASE. The mask declines because `tokens.*` is a path IsSecret never asks for; " +
			"the seal's unit IS the segment, so `every value of this map is secret` is exactly what it can say",
	}, {
		name:     "flag, nested UNDER an additional_properties node",
		schema:   "users:\n  type: object\n  additional_properties:\n    type: object\n    properties:\n      password: { type: string, secret: true }",
		wantMask: []string{"users.password"},
		wantSeal: []string{"users"},
		reason:   "the flag is on a concrete property, not on the ap node; the mask's own dynamic-key gap is pinned separately",
	}, {
		name:     "type, top-level scalar",
		schema:   `vault_ref: { type: secret }`,
		wantMask: []string{"vault_ref"},
		wantSeal: []string{"vault_ref"},
		reason: "THE ROUND-2 CASE. The value is normally in Vault, not state -- but the address is exact, so " +
			"covering the abnormal route (the upgrade write does not strip) costs nothing",
	}, {
		name:     "type, under items, keyed by a sibling",
		schema:   "redis_users:\n  type: array\n  items:\n    type: object\n    properties:\n      name: { type: string, required: true }\n      password:\n        type: secret\n        key: name",
		wantMask: []string{"redis_users[].password"},
		wantSeal: nil,
		reason: "THE ROUND-1 CASE. The fold would seal the whole ACL inventory -- public, read by every day-2 " +
			"scenario -- for a value the merge keeps out of the record",
	}, {
		name:     "type, ON an additional_properties node",
		schema:   "map_field:\n  type: object\n  additional_properties: { type: secret }",
		wantMask: []string{"map_field"},
		wantSeal: []string{"map_field"},
		reason:   "unreachable through a loaded manifest either way; both answers are belt and braces and the address is exact",
	}, {
		name:     "nothing declared",
		schema:   "port:     { type: string }\nreplicas: { type: integer }",
		wantMask: nil,
		wantSeal: nil,
		reason:   "the common service: no address, so an ordinary run's diagnostics stay legible",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			schema := stateSchemaFixture(t, tc.schema)

			mask := audit.SecretPathSet{}
			CollectStateSchemaSecrets(schema, "", mask)
			assertPathSet(t, "MASK", mask, tc.wantMask, tc.reason)

			seal := StateSchemaSecretFields(&artifact.ServiceArtifact{
				Manifest: &config.ServiceManifest{StateSchema: schema},
			})
			assertPathSet(t, "SEAL", seal, tc.wantSeal, tc.reason)
		})
	}
}

// assertPathSet compares one answer against its row, and says which of the two
// questions is being answered — a diff on the wrong one reads as the other's bug.
func assertPathSet[M ~map[string]bool](t *testing.T, question string, got M, want []string, reason string) {
	t.Helper()
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s: %q missing from %v\nrow says: %s", question, w, got, reason)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: got %v, the row allows exactly %v\nrow says: %s\n"+
			"If this is a deliberate change, edit the row and its reason together -- an answer\n"+
			"changed without one is how the last three defects here happened.", question, got, want, reason)
	}
}
