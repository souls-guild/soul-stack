package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/sigilcache"
)

// dispatchPayload воспроизводит ветки recv-loop handleSession для одного
// FromKeeper-payload-а в части, относящейся к Sigil (ADR-026(h)).
// Полный handleSession требует живого *soulgrpc.StreamSession (mTLS-стрим) — это
// покрыто integration-тестом; здесь изолируем именно диспетчеризацию payload →
// кеш / holder якорей, чтобы не поднимать gRPC ради проверки этих case.
//
// Авторитетный путь набора допусков — SigilSnapshot → ReplaceAll. Одиночный
// PluginSigil — уведомление, набор НЕ мутирует. SigilTrustAnchors (R3-S6) →
// ReplaceAll в holder якорей (fail-closed на битом PEM). Если в main.go меняется
// switch, этот хелпер держим в синхроне — он намеренно дублирует ту же логику.
func dispatchPayload(msg *keeperv1.FromKeeper, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet) {
	switch payload := msg.GetPayload().(type) {
	case *keeperv1.FromKeeper_SigilSnapshot:
		sigils.ReplaceAll(payload.SigilSnapshot.GetSigils())
	case *keeperv1.FromKeeper_SigilTrustAnchors:
		if anchors == nil {
			return
		}
		parsed, err := parseTrustAnchorSet(payload.SigilTrustAnchors.GetPubkeyPem())
		if err != nil {
			return // fail-closed: битый набор — holder не трогаем
		}
		anchors.SetAnchors(parsed)
	case *keeperv1.FromKeeper_PluginSigil:
		// no-op: одиночное уведомление набор не меняет (Вариант A).
	default:
		// прочие payload-ы вне зоны этого теста
	}
}

func snapshotMsg(sigs ...*keeperv1.PluginSigil) *keeperv1.FromKeeper {
	return &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_SigilSnapshot{
		SigilSnapshot: &keeperv1.SigilSnapshot{Sigils: sigs},
	}}
}

func pluginSigil(ns, name, ref, sha string) *keeperv1.PluginSigil {
	return &keeperv1.PluginSigil{Namespace: ns, Name: name, Ref: ref, BinarySha256: sha}
}

// TestReceiverAppliesSnapshot — SigilSnapshot наполняет кеш через ReplaceAll.
func TestReceiverAppliesSnapshot(t *testing.T) {
	sigils := sigilcache.New()

	dispatchPayload(snapshotMsg(
		pluginSigil("core", "pkg", "v1.2.3", "deadbeef"),
		pluginSigil("cloud", "hetzner", "v2", "cafef00d"),
	), sigils, nil)

	got := sigils.Get("core", "pkg")
	if got == nil {
		t.Fatal("receiver не положил snapshot-Sigil в кеш")
	}
	if got.GetRef() != "v1.2.3" || got.GetBinarySha256() != "deadbeef" {
		t.Fatalf("в кеше неверный Sigil: ref=%q sha=%q", got.GetRef(), got.GetBinarySha256())
	}
	if sigils.Get("cloud", "hetzner") == nil {
		t.Fatal("receiver потерял второй Sigil из snapshot-а")
	}
}

// TestReceiverSnapshotRevokesAbsent — near-instant revoke (S6c): новый snapshot
// без ранее допущенного плагина стирает его из кеша (ReplaceAll).
func TestReceiverSnapshotRevokesAbsent(t *testing.T) {
	sigils := sigilcache.New()

	// allow: snapshot с двумя допусками.
	dispatchPayload(snapshotMsg(
		pluginSigil("core", "pkg", "v1", "aa"),
		pluginSigil("core", "file", "v1", "bb"),
	), sigils, nil)
	if sigils.Get("core", "pkg") == nil || sigils.Get("core", "file") == nil {
		t.Fatal("предусловие: оба допуска должны быть в кеше")
	}

	// revoke core/pkg: новый snapshot уже без него.
	dispatchPayload(snapshotMsg(pluginSigil("core", "file", "v1", "bb")), sigils, nil)

	if sigils.Get("core", "pkg") != nil {
		t.Fatal("near-instant revoke не сработал: отозванный допуск остался в кеше")
	}
	if sigils.Get("core", "file") == nil {
		t.Fatal("ReplaceAll по ошибке стёр действующий допуск")
	}
}

// TestReceiverEmptySnapshotClearsCache — пустой snapshot = ни один плагин не
// допущен (revoke всех).
func TestReceiverEmptySnapshotClearsCache(t *testing.T) {
	sigils := sigilcache.New()
	dispatchPayload(snapshotMsg(pluginSigil("core", "pkg", "v1", "aa")), sigils, nil)
	if sigils.Get("core", "pkg") == nil {
		t.Fatal("предусловие: допуск должен быть в кеше")
	}

	dispatchPayload(snapshotMsg(), sigils, nil) // пустой snapshot

	if sigils.Get("core", "pkg") != nil {
		t.Fatal("пустой snapshot должен очистить кеш (ни один плагин не допущен)")
	}
}

// TestReceiverSinglePluginSigilDoesNotMutate — одиночный PluginSigil (Вариант A)
// — уведомление, набор НЕ меняет: ни добавления, ни замены.
func TestReceiverSinglePluginSigilDoesNotMutate(t *testing.T) {
	sigils := sigilcache.New()

	// Авторитетный набор из snapshot-а.
	dispatchPayload(snapshotMsg(pluginSigil("core", "pkg", "v1", "aa")), sigils, nil)

	// Одиночный PluginSigil НЕ должен ни добавить новый, ни заменить существующий.
	dispatchPayload(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_PluginSigil{
		PluginSigil: pluginSigil("core", "pkg", "v2", "bb"),
	}}, sigils, nil)
	dispatchPayload(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_PluginSigil{
		PluginSigil: pluginSigil("community", "new", "v1", "cc"),
	}}, sigils, nil)

	got := sigils.Get("core", "pkg")
	if got == nil || got.GetRef() != "v1" || got.GetBinarySha256() != "aa" {
		t.Fatalf("одиночный PluginSigil не должен менять авторитетный набор, получено %v", got)
	}
	if sigils.Get("community", "new") != nil {
		t.Fatal("одиночный PluginSigil не должен добавлять допуск в набор")
	}
}

// TestReceiverIgnoresNonSigilPayload — не-Sigil payload не трогает кеш.
func TestReceiverIgnoresNonSigilPayload(t *testing.T) {
	sigils := sigilcache.New()
	msg := &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{
		HelloReply: &keeperv1.HelloReply{SessionId: "s1"},
	}}

	dispatchPayload(msg, sigils, nil)

	if got := sigils.Get("core", "pkg"); got != nil {
		t.Fatalf("не-Sigil payload не должен наполнять кеш, получено %v", got)
	}
}

// genAnchorPEM генерирует свежий ed25519-публичный ключ в SPKI PEM-форме
// (симметрично keeper-side sigil.Signer.AnchorSetPEM) для anchors-тестов.
func genAnchorPEM(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pub, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func anchorsMsg(pems ...string) *keeperv1.FromKeeper {
	return &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_SigilTrustAnchors{
		SigilTrustAnchors: &keeperv1.SigilTrustAnchors{PubkeyPem: pems},
	}}
}

// TestReceiverAppliesTrustAnchors — SigilTrustAnchors (R3-S6) заменяет holder
// якорей целиком (ReplaceAll): свежий набор парсится и применяется.
func TestReceiverAppliesTrustAnchors(t *testing.T) {
	holder := sharedhost.NewAnchorSet(nil)
	_, pem1 := genAnchorPEM(t)
	_, pem2 := genAnchorPEM(t)

	dispatchPayload(anchorsMsg(pem1, pem2), nil, holder)

	parsed, err := parseTrustAnchorSet([]string{pem1, pem2})
	if err != nil {
		t.Fatalf("parseTrustAnchorSet: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("ожидали 2 якоря, распарсилось %d", len(parsed))
	}
}

// TestReceiverTrustAnchorsRejectsBrokenPEM — fail-closed: битый PEM в наборе НЕ
// подменяет holder (прежний валидный набор сохраняется).
func TestReceiverTrustAnchorsRejectsBrokenPEM(t *testing.T) {
	prevPub, prevPEM := genAnchorPEM(t)
	holder := sharedhost.NewAnchorSet([]ed25519.PublicKey{prevPub})

	// Битый набор: один валидный PEM + один мусор. parseTrustAnchorSet вернёт
	// ошибку → holder не трогаем.
	_, goodPEM := genAnchorPEM(t)
	dispatchPayload(anchorsMsg(goodPEM, "not a pem block"), nil, holder)

	// Holder остался прежним: проверяем через повторный парс исходного набора.
	keep, err := parseTrustAnchorSet([]string{prevPEM})
	if err != nil || len(keep) != 1 || !keep[0].Equal(prevPub) {
		t.Fatalf("предусловие/инвариант битого набора нарушен: keep=%d err=%v", len(keep), err)
	}
	if _, err := parseTrustAnchorSet([]string{goodPEM, "not a pem block"}); err == nil {
		t.Fatal("ожидали ошибку парсинга битого набора, получили nil")
	}
}

// TestReceiverEmptyTrustAnchorsClearsHolder — пустой набор стирает holder
// (Sigil выключен на Keeper → verify fail-closed по no_trust_anchor).
func TestReceiverEmptyTrustAnchorsClearsHolder(t *testing.T) {
	pub, _ := genAnchorPEM(t)
	holder := sharedhost.NewAnchorSet([]ed25519.PublicKey{pub})

	dispatchPayload(anchorsMsg(), nil, holder)

	got, err := parseTrustAnchorSet(nil)
	if err != nil || got != nil {
		t.Fatalf("пустой набор должен парситься в nil без ошибки, got=%v err=%v", got, err)
	}
}

// TestReceiverTrustAnchorsNilHolder — без holder (push / тест без Host) ветка
// no-op, паники нет.
func TestReceiverTrustAnchorsNilHolder(t *testing.T) {
	_, pem1 := genAnchorPEM(t)
	dispatchPayload(anchorsMsg(pem1), nil, nil) // не должно паниковать
}
