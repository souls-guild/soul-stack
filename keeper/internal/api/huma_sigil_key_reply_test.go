// GOLDEN byte-exact wire-guard для NATIVE wire-DTO SIGIL-KEY-домена (handler-native T5d).
// sigil-key больше НЕ зависит от legacy-генерата — golden сверяет json native-значения с ЗАФИКСИРОВАННОЙ
// строкой-эталоном (pinned). Покрыты date-time + status-enum как строка. Мутация формы
// native-struct краснит case.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenSigilKeyWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_SigilKeyReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 0, time.UTC) // handler даёт Truncate(Second)
	pem := "-----BEGIN PUBLIC KEY-----\nMCowBQ...\n-----END PUBLIC KEY-----\n"
	keyID := "a1b2c3d4e5f6"

	// --- SigilKeyIntroduceReply: status-enum как строка ---
	goldenSigilKeyWire(t, "IntroduceReply/primary",
		SigilKeyIntroduceReply{IntroducedAt: ts, IsPrimary: true, KeyID: keyID, PubkeyPEM: pem, Status: SigilKeyIntroduceReplyStatusActive},
		`{"introduced_at":"2026-06-14T12:34:56Z","is_primary":true,"key_id":"a1b2c3d4e5f6","pubkey_pem":"-----BEGIN PUBLIC KEY-----\nMCowBQ...\n-----END PUBLIC KEY-----\n","status":"active"}`)
	goldenSigilKeyWire(t, "IntroduceReply/retired",
		SigilKeyIntroduceReply{IntroducedAt: ts, IsPrimary: false, KeyID: keyID, PubkeyPEM: pem, Status: SigilKeyIntroduceReplyStatusRetired},
		`{"introduced_at":"2026-06-14T12:34:56Z","is_primary":false,"key_id":"a1b2c3d4e5f6","pubkey_pem":"-----BEGIN PUBLIC KEY-----\nMCowBQ...\n-----END PUBLIC KEY-----\n","status":"retired"}`)

	// --- SigilKeyView (nested): status-enum как строка ---
	goldenSigilKeyWire(t, "SigilKeyView/active",
		SigilKeyView{IntroducedAt: ts, IsPrimary: true, KeyID: keyID, Status: SigilKeyViewStatusActive},
		`{"introduced_at":"2026-06-14T12:34:56Z","is_primary":true,"key_id":"a1b2c3d4e5f6","status":"active"}`)
}

// TestGoldenWire_SigilKeyProjection проверяет, что проекция доменных handlers.SigilKey*-
// result-ов → native сохраняет byte-exact wire против зафиксированного эталона. Ловит регресс
// в маппинге полей (вкл. list items[]).
func TestGoldenWire_SigilKeyProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pem := "PEM"
	keyID := "key0"

	introV := handlers.SigilKeyIntroduceView{IntroducedAt: ts, IsPrimary: true, KeyID: keyID, PubkeyPEM: pem, Status: "active"}
	goldenSigilKeyWire(t, "proj/IntroduceReply", newSigilKeyIntroduceReply(introV),
		`{"introduced_at":"2026-06-14T12:00:00Z","is_primary":true,"key_id":"key0","pubkey_pem":"PEM","status":"active"}`)

	viewV := handlers.SigilKeyView{IntroducedAt: ts, IsPrimary: false, KeyID: keyID, Status: "retired"}
	goldenSigilKeyWire(t, "proj/SigilKeyView", newSigilKeyView(viewV),
		`{"introduced_at":"2026-06-14T12:00:00Z","is_primary":false,"key_id":"key0","status":"retired"}`)

	pageV := handlers.SigilKeyListPage{Items: []handlers.SigilKeyView{viewV}}
	goldenSigilKeyWire(t, "proj/SigilKeyListReply", newSigilKeyListReply(pageV),
		`{"items":[{"introduced_at":"2026-06-14T12:00:00Z","is_primary":false,"key_id":"key0","status":"retired"}]}`)
	// handler даёт make([]., 0): items=`[]` (non-nil), НЕ null
	pageEmpty := handlers.SigilKeyListPage{Items: []handlers.SigilKeyView{}}
	goldenSigilKeyWire(t, "proj/SigilKeyListReply/empty", newSigilKeyListReply(pageEmpty),
		`{"items":[]}`)
}
