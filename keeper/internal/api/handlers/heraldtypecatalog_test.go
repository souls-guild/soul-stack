package handlers

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

// TestHeraldTypeCatalog_CoversAllTypes — catalog returns EXACTLY herald.AllHeraldTypes
// (single source): every known channel type is present, none extra.
func TestHeraldTypeCatalog_CoversAllTypes(t *testing.T) {
	resp := NewHeraldTypeCatalogHandler(nil).ListTyped()
	if len(resp.Types) == 0 {
		t.Fatal("herald-type catalog is empty")
	}

	got := make([]string, 0, len(resp.Types))
	for _, ty := range resp.Types {
		got = append(got, ty.Type)
	}
	sort.Strings(got)

	want := make([]string, 0)
	for _, ty := range herald.AllHeraldTypes() {
		want = append(want, string(ty))
	}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("catalog types=%v, herald.AllHeraldTypes=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("catalog types[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestHeraldTypeCatalog_FieldsAndSecrets — each type carries a non-empty field set;
// each catalog type is valid per herald.ValidHeraldType (contract: UI sends type
// verbatim); secret fields are marked kind=vault_ref (ADR-052 amendment wiring).
func TestHeraldTypeCatalog_FieldsAndSecrets(t *testing.T) {
	resp := buildHeraldTypeCatalog()
	for _, ty := range resp.Types {
		if !herald.ValidHeraldType(herald.HeraldType(ty.Type)) {
			t.Errorf("catalog type %q rejected by ValidHeraldType", ty.Type)
		}
		if len(ty.Fields) == 0 {
			t.Errorf("catalog type %q has no fields", ty.Type)
		}
		for _, f := range ty.Fields {
			if f.Name == "" {
				t.Errorf("catalog type %q has field with empty name", ty.Type)
			}
			if f.Secret && f.Kind != string(herald.KindVaultRef) {
				t.Errorf("catalog type %q field %q is secret but kind=%q (must be vault_ref)", ty.Type, f.Name, f.Kind)
			}
		}
	}
}

// TestHeraldTypeCatalog_KnownFields — targeted pinning of key fields of the pilot
// types (catalog shape drift check): webhook.url, telegram.bot_token_ref(secret),
// email.smtp_host/to, custom.method. Catches accidental rename/loss of a field.
func TestHeraldTypeCatalog_KnownFields(t *testing.T) {
	resp := buildHeraldTypeCatalog()
	byType := map[string]map[string]HeraldFieldView{}
	for _, ty := range resp.Types {
		fields := map[string]HeraldFieldView{}
		for _, f := range ty.Fields {
			fields[f.Name] = f
		}
		byType[ty.Type] = fields
	}

	assertField := func(typ, name string, wantReq, wantSecret bool) {
		t.Helper()
		f, ok := byType[typ][name]
		if !ok {
			t.Errorf("type %q missing field %q", typ, name)
			return
		}
		if f.Required != wantReq || f.Secret != wantSecret {
			t.Errorf("type %q field %q: required=%v secret=%v, want required=%v secret=%v",
				typ, name, f.Required, f.Secret, wantReq, wantSecret)
		}
	}

	assertField("webhook", "url", true, false)
	assertField("telegram", "bot_token_ref", true, true)
	assertField("slack", "webhook_url_ref", true, true)
	assertField("mattermost", "webhook_url_ref", true, true)
	assertField("discord", "webhook_url_ref", true, true)
	assertField("custom", "url", true, false)
	assertField("custom", "header_secret_ref", false, true)
	assertField("email", "smtp_host", true, false)
	assertField("email", "to", true, false)
	assertField("email", "password_ref", false, true)
}

// TestHeraldTypeCatalog_EnumValues — enum fields carry the set of allowed values
// (kind=enum ⟹ EnumValues non-empty), non-enum are empty. The UI renders enum as a
// select instead of a text input, so the set must reach the catalog from the domain.
func TestHeraldTypeCatalog_EnumValues(t *testing.T) {
	resp := buildHeraldTypeCatalog()
	byType := map[string]map[string]HeraldFieldView{}
	for _, ty := range resp.Types {
		fields := map[string]HeraldFieldView{}
		for _, f := range ty.Fields {
			fields[f.Name] = f
		}
		byType[ty.Type] = fields
	}

	assertEnum := func(typ, name string, want []string) {
		t.Helper()
		f, ok := byType[typ][name]
		if !ok {
			t.Fatalf("type %q missing field %q", typ, name)
		}
		if f.Kind != string(herald.KindEnum) {
			t.Errorf("type %q field %q kind=%q, want enum", typ, name, f.Kind)
		}
		if len(f.EnumValues) != len(want) {
			t.Fatalf("type %q field %q enum_values=%v, want %v", typ, name, f.EnumValues, want)
		}
		for i := range want {
			if f.EnumValues[i] != want[i] {
				t.Errorf("type %q field %q enum_values[%d]=%q, want %q", typ, name, i, f.EnumValues[i], want[i])
			}
		}
	}

	assertEnum("telegram", "parse_mode", []string{"", "MarkdownV2", "HTML"})
	assertEnum("custom", "method", []string{"", "POST", "PUT", "PATCH"})
	assertEnum("email", "tls_mode", []string{"", "starttls", "tls", "none"})

	// Non-enum fields carry no set (otherwise the UI would render a select where an input is needed).
	if got := byType["webhook"]["url"].EnumValues; len(got) != 0 {
		t.Errorf("webhook.url (kind=url) enum_values=%v, want empty", got)
	}
	if got := byType["telegram"]["bot_token_ref"].EnumValues; len(got) != 0 {
		t.Errorf("telegram.bot_token_ref (kind=vault_ref) enum_values=%v, want empty", got)
	}
}

// TestHeraldTypeCatalog_SecretRequired — the top-level secret_ref flag reaches the
// catalog from [herald.channelDriver.secretRequired]: true only for webhook, false
// for messengers/custom/email. The UI shows the secret_ref field by this flag, not
// by a hardcoded type==='webhook' (otherwise a 2nd secret type silently hides the field).
func TestHeraldTypeCatalog_SecretRequired(t *testing.T) {
	resp := buildHeraldTypeCatalog()
	byType := map[string]bool{}
	for _, ty := range resp.Types {
		byType[ty.Type] = ty.SecretRequired
	}

	cases := map[string]bool{
		"webhook":    true,
		"telegram":   false,
		"slack":      false,
		"mattermost": false,
		"discord":    false,
		"custom":     false,
		"email":      false,
	}
	for typ, want := range cases {
		got, ok := byType[typ]
		if !ok {
			t.Fatalf("catalog missing type %q", typ)
		}
		if got != want {
			t.Errorf("type %q secret_required=%v, want %v", typ, got, want)
		}
	}
}
