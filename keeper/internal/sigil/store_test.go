package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// validRecord — заготовка валидной записи для проверки Insert-guard-ов. db не
// нужна: guard-ы отрабатывают ДО обращения к ExecQueryRower.
func validRecord() *Sigil {
	digest := sha256.Sum256([]byte("binary"))
	return &Sigil{
		Namespace:    "cloud",
		Name:         "hetzner",
		Ref:          "v1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    make([]byte, ed25519.SignatureSize),
		ManifestRaw:  []byte("kind: cloud_driver\n"),
		Manifest:     []byte(`{"kind":"cloud_driver"}`),
		AllowedByAID: "archon-a",
	}
}

// TestInsert_GuardEmptyManifestRaw — пустой ManifestRaw отклоняется ДО запроса в
// БД: подпись ставится ровно над этими байтами, fallback неприменим
// (Normalize("{}") != Normalize("")). nil-db гарантирует, что guard сработал до
// QueryRow (иначе был бы nil-panic).
func TestInsert_GuardEmptyManifestRaw(t *testing.T) {
	rec := validRecord()
	rec.ManifestRaw = nil
	err := Insert(context.Background(), nil, rec)
	if err == nil {
		t.Fatal("Insert с пустым ManifestRaw должен вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "manifest_raw") {
		t.Errorf("ошибка = %q, ожидалось упоминание manifest_raw", err)
	}

	rec.ManifestRaw = []byte{}
	if err := Insert(context.Background(), nil, rec); err == nil {
		t.Fatal("Insert с ManifestRaw len 0 должен вернуть ошибку")
	}
}
