// Package herald provides registries of Herald (delivery channels) and Tiding (subscription
// rules) for notifications about run events in Postgres (ADR-052, slice S1).
//
// Herald is where to send (webhook channel in MVP), Tiding is what to react to and
// with which Herald. Delivery / tap decorator over audit.Writer /
// notification dispatcher are following slices (S2-S4); here only types,
// validation and CRUD layer (pattern keeper/internal/augur omens/rites).
package herald

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/netguard"
)

// NamePattern is canonical form of Herald/Tiding name: kebab-case, length
// 1..63. Same as CHECK heralds_name_format / tidings_name_format in migration
// 071 (like omens.NamePattern).
const NamePattern = `^[a-z0-9-]{1,63}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// HeraldType is closed-enum of channel type (ADR-052 amendment). Canonical set
// of known types is driver registry [channelDrivers] (HTTP-class) + email
// (SMTP-class); single source is [AllHeraldTypes]. Adding HTTP type is one
// entry in channelDrivers + CHECK migration + huma-enum (verified by guard-test
// with AllHeraldTypes).
//
// Classes: webhook is HTTP with HMAC signature (top-level secret_ref); telegram/slack/
// mattermost/discord are HTTP messengers (auth via vault-ref field in config,
// human-readable text); custom is HTTP with fixed webhookPayload body;
// email is SMTP (separate axis, not channelDrivers).
type HeraldType string

const (
	HeraldWebhook    HeraldType = "webhook"
	HeraldTelegram   HeraldType = "telegram"
	HeraldSlack      HeraldType = "slack"
	HeraldMattermost HeraldType = "mattermost"
	HeraldDiscord    HeraldType = "discord"
	HeraldCustom     HeraldType = "custom"
	HeraldEmail      HeraldType = "email"
)

// ValidHeraldType returns true for known channel type: HTTP-driver in
// [channelDrivers] OR email (SMTP-axis). Single source is [AllHeraldTypes].
func ValidHeraldType(t HeraldType) bool {
	if _, ok := driverFor(t); ok {
		return true
	}
	return t == HeraldEmail
}

// Herald is runtime representation of `heralds` registry row.
//
// Config is per-type channel configuration (for webhook — url + opt. headers +
// opt. opt-out flags http_allowed/allow_private). SecretRef is vault-ref of
// channel secret (signing-token), nullable: not every webhook needs signature.
type Herald struct {
	Name      string         `json:"name"`
	Type      HeraldType     `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref,omitempty"`
	// Secret is plaintext webhook signing-secret (dual-mode, ADR-064): operator
	// passes value instead of SecretRef; Service materializes it to Vault
	// ([materializeHeraldSecrets]) and replaces with internal SecretRef. json:"-"
	// NEVER serialized (not in PG/View/audit), request-scoped, erased
	// after write. XOR with SecretRef. Similarly for channel config fields (<base>
	// plaintext XOR <base>_ref) — their plaintext lives in Config until materialization.
	Secret *string `json:"-"`
	// SecretWritten is request-scoped marker: keeper wrote plaintext secret to
	// Vault in this operation (ADR-064 audit-event). json:"-"; read by audit
	// payload (key plaintext_ingested), doesn't go to PG/View.
	SecretWritten bool      `json:"-"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CreatedByAID  *string   `json:"created_by_aid,omitempty"`
}

// Tiding is runtime representation of `tidings` registry row.
//
// EventTypes is non-empty list of audit event-types with area-glob support
// (`scenario_run.*`); validated by [ValidateEventTypes]. Incarnation/Cadence are
// opt. selectors for binding to run source (nil = no filter).
//
// Ephemeral/VoyageID (ADR-052(g)) is ephemeral rule bound to one
// run: Ephemeral=true ⟺ VoyageID != nil (invariant [ErrEphemeralRequiresVoyage],
// duplicated by CHECK tidings_ephemeral_voyage_consistent). Permanent rule is
// Ephemeral=false, VoyageID=nil.
//
// Annotations/Projection (ADR-052(h)) control webhook delivery body.
// Annotations are static operator fields (top-level JSON object),
// merged by key `annotations` to body. Projection is allow-list of payload
// paths (empty = full form). Both applied by delivery worker off-path (N3); domain
// (N1) only stores + validates syntax.
//
// Task (ADR-052 §l) is opt. selector for subscription to SPECIFIC run task by
// address (register ∪ id from changed_tasks of incarnation.run_completed,
// ADR-052 §j). nil = no filter. Non-empty value → rule matches
// incarnation.run_completed only if its changed_tasks has entry with
// register == *Task OR id == *Task ([matchTask]). Self-sufficient: address
// presence in changed_tasks = task changed on at least one host.
//
// CreatedFromCadenceID (ADR-052 §m / ADR-046 §9) is origin marker: rule
// born from Cadence notify[] block (POST /v1/cadences). nil = created otherwise
// (CRUD Tiding manually / ephemeral from Voyage). Non-empty → FK to cadences(id) ON
// DELETE CASCADE: Cadence deletion removes born rules. Orthogonal to Cadence
// selector (subscription filter "send only for runs of this schedule"): cascade
// removes ONLY form-rules, not manually created with same cadence
// selector. Binding by ULID (cadences.id), not name — rename-safe.
type Tiding struct {
	Name                 string         `json:"name"`
	Herald               string         `json:"herald"`
	EventTypes           []string       `json:"event_types"`
	OnlyFailures         bool           `json:"only_failures"`
	OnlyChanges          bool           `json:"only_changes"`
	Incarnation          *string        `json:"incarnation,omitempty"`
	Cadence              *string        `json:"cadence,omitempty"`
	Task                 *string        `json:"task,omitempty"`
	Ephemeral            bool           `json:"ephemeral"`
	VoyageID             *string        `json:"voyage_id,omitempty"`
	CreatedFromCadenceID *string        `json:"created_from_cadence_id,omitempty"`
	Annotations          map[string]any `json:"annotations,omitempty"`
	Projection           []string       `json:"projection,omitempty"`
	Enabled              bool           `json:"enabled"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	CreatedByAID         *string        `json:"created_by_aid,omitempty"`
}

// ValidateConfig validates channel config by type (what DB-CHECK doesn't
// cover — JSONB shape depends on type). Dispatcher by class: HTTP types
// validated by their [channelDriver.validateConfig] (generic descriptor
// field traversal + domain invariants — SSRF guard URL webhook/custom, chat_id telegram);
// email by [validateEmailConfig] (SMTP-axis). Single validator source and
// catalog — same field descriptors.
//
// fail-closed: unknown type / corrupt config rejected at CRUD stage, before
// write.
func ValidateConfig(t HeraldType, config map[string]any) error {
	if d, ok := driverFor(t); ok {
		return d.validateConfig(config)
	}
	if t == HeraldEmail {
		return validateEmailConfig(config)
	}
	return fmt.Errorf("herald: unknown type %q (known: %v)", t, AllHeraldTypes())
}

// validateWebhookURL is domain SSRF guard for URL of HTTP types with operator-specified
// url (webhook/custom). Default guard (both opt-out false): https-only +
// literal-private-IP block ([netguard.ValidateEndpoint]). On opt-out — per-element,
// not to over-restrict (http:// only if http_allowed; private-IP covered by
// dial guard at delivery). Presence/form of url already checked by generic
// descriptor traversal — here only transport guard.
func validateWebhookURL(config map[string]any) error {
	urlStr, _ := config["url"].(string)
	if urlStr == "" {
		return fmt.Errorf("herald: config %q must be a non-empty string", "url")
	}

	httpAllowed := configBool(config, "http_allowed")
	allowPrivate := configBool(config, "allow_private")

	if !httpAllowed && !allowPrivate {
		return netguard.ValidateEndpoint(urlStr)
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("herald: config invalid url %q", urlStr)
	}
	if u.Host == "" {
		return fmt.Errorf("herald: config url %q has no host", urlStr)
	}
	if !httpAllowed && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("herald: config: only https:// allowed (set http_allowed=true to opt out), got scheme %q", u.Scheme)
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("herald: config: unsupported url scheme %q", u.Scheme)
	}
	return nil
}

// configBool reads bool flag from config (missing/non-bool → false). JSON numbers
// and strings are not flags — safety flag set only by explicit `true`.
func configBool(config map[string]any, key string) bool {
	v, ok := config[key].(bool)
	return ok && v
}

// ValidateSecretRef validates channel top-level secret_ref. Semantics (ADR-052
// amendment, secret routing): secret_ref is STRICTLY HMAC signing-token; only
// used by type whose driver declared [channelDriver.secretRequired]
// (webhook). For other types auth-credential is vault-ref field INSIDE config (e.g.
// telegram bot_token_ref), and top-level secret_ref must be EMPTY. Rules:
//   - nil/empty — always ok (signature optional);
//   - type without secret support + non-empty secret_ref → error (field not for type);
//   - type with secret support + non-empty secret_ref → must be valid
//     vault-ref (`vault:<mount>/<path>`), parsed by same parser as omens.auth_ref.
func ValidateSecretRef(t HeraldType, ref *string) error {
	if ref == nil || *ref == "" {
		return nil
	}
	d, ok := driverFor(t)
	if !ok || !d.secretRequired() {
		return fmt.Errorf("herald: secret_ref is only for signing (webhook); %s uses a vault-ref field inside config", t)
	}
	if _, err := vault.ParseRef(*ref); err != nil {
		return fmt.Errorf("herald: invalid secret_ref %q (must be a vault-ref vault:<mount>/<path>): %w", *ref, err)
	}
	return nil
}

// marshalConfig serializes config to JSON bytes for JSONB column. nil → `{}`
// (schema has DEFAULT, but pgx requires non-nil for NOT NULL). Symmetrical to
// pushprovider.marshalParams.
func marshalConfig(config map[string]any) ([]byte, error) {
	if config == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(config)
}

// projectionSegmentRe allows projection path segment: non-empty,
// lowercase/digits/`_`. Full path is segments via `.` (`summary.succeeded`).
var projectionSegmentRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// ValidateProjection validates SYNTAX of projection allow-list paths (ADR-052(h)):
// each path is non-empty `[a-z0-9_]` segments separated by `.`; empty
// segments forbidden (leading/double/trailing dot → `..`/`.x`/`x.`) and `..` itself.
//
// Deep check of path AGAINST event payload form NOT done here —
// allow-list resolved lazily in delivery worker (N3): nonexistent path
// simply won't go to body, payload form catalog evolves and brittle
// static match against it shouldn't exist. nil/empty projection
// allowed (= full payload form).
func ValidateProjection(paths []string) error {
	for _, p := range paths {
		if p == "" {
			return fmt.Errorf("herald: projection path is empty")
		}
		// strings.Split catches empty segment with leading/double/trailing dot
		// (including literal `..` → two empty neighbors) in one pass.
		for _, seg := range strings.Split(p, ".") {
			if seg == "" {
				return fmt.Errorf("herald: invalid projection path %q (empty segment — no leading/trailing/double dot)", p)
			}
			if !projectionSegmentRe.MatchString(seg) {
				return fmt.Errorf("herald: invalid projection path %q (segment %q must match [a-z0-9_])", p, seg)
			}
		}
	}
	return nil
}

// ValidateAnnotationsJSON validates that raw annotations JSON is top-level
// OBJECT (ADR-052(h)/(i)): merged to webhook body by key `annotations`,
// so array/scalar/string at top level not allowed. Called by handler/
// MCP side (N2) decoding user JSON, before [Tiding] construction
// (where annotations already typed as map). Empty/`null` JSON allowed (= no
// static fields). On violation returns plain error (no sentinel):
// handler side (N2) wraps it in [ErrValidation] → 422. Separate sentinel
// not needed here — N2 doesn't distinguish cause of invalid annotations.
func ValidateAnnotationsJSON(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("herald: annotations is not valid JSON")
	}
	if _, ok := probe.(map[string]any); !ok {
		return fmt.Errorf("herald: annotations must be a JSON object (not an array or scalar)")
	}
	return nil
}

// marshalAnnotations serializes annotations to JSON bytes for JSONB column.
// nil → `{}` (NOT NULL DEFAULT, like marshalConfig).
func marshalAnnotations(annotations map[string]any) ([]byte, error) {
	if annotations == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(annotations)
}
