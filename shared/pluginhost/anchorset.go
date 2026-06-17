package pluginhost

import (
	"crypto/ed25519"
	"sync/atomic"
)

// AnchorSet — атомарно-сменяемый набор trust-anchor-ов подписи Sigil
// (ADR-026(h), R3 multi-anchor). Держит снимок публичных ed25519-ключей, против
// которых verify проверяет подпись допуска: подпись валидна, если её подтвердил
// ЛЮБОЙ якорь из набора (OR-семантика — это и даёт безразрывную ротацию ключа
// подписи, см. [verifySigilAndSeal]).
//
// Зачем atomic, а не просто слайс в Host: набор обновляется в рантайме
// сообщением SigilTrustAnchors (ReplaceAll, S6) БЕЗ перезапуска Soul-а, пока
// конкурентные Spawn читают его в verify. atomic.Pointer на immutable-слайс даёт
// lock-free snapshot для verify и атомарную замену для [AnchorSet.SetAnchors];
// сами ed25519.PublicKey immutable, заголовок слайса никогда не мутируется
// по месту — только подменяется целиком.
//
// nil-набор (пустой) = Sigil выключен → verify fail-closed по
// no_trust_anchor (см. [verifySigilAndSeal]). nil-указатель *AnchorSet тоже
// валиден и означает «якорей нет» — методы nil-safe.
type AnchorSet struct {
	// keys — immutable-снимок набора. Пишется только целиком (Store), читается
	// без блокировки (Load). nil = набор пуст.
	keys atomic.Pointer[[]ed25519.PublicKey]
}

// NewAnchorSet конструирует набор из стартовых якорей (bootstrap-набор в S5).
// Копирует заголовок слайса, чтобы мутация caller-ом не затронула снимок.
// Пустой/nil вход → набор без якорей (verify fail-closed no_trust_anchor).
func NewAnchorSet(anchors []ed25519.PublicKey) *AnchorSet {
	a := &AnchorSet{}
	a.SetAnchors(anchors)
	return a
}

// SetAnchors атомарно заменяет весь набор якорей (ReplaceAll-семантика
// ADR-026(h)). Используется S6 при доставке SigilTrustAnchors в рантайме:
// после замены конкурентные verify видят новый набор без рестарта, anchor вне
// нового набора «забывается» (retire старого ключа). Безопасно для
// конкурентного вызова с [AnchorSet.snapshot].
//
// Копирует заголовок входного слайса: caller волен переиспользовать свой буфер,
// снимок останется неизменным до следующего SetAnchors.
func (a *AnchorSet) SetAnchors(anchors []ed25519.PublicKey) {
	if len(anchors) == 0 {
		a.keys.Store(nil)
		return
	}
	cp := make([]ed25519.PublicKey, len(anchors))
	copy(cp, anchors)
	a.keys.Store(&cp)
}

// snapshot возвращает текущий immutable-набор якорей для verify. Без копии:
// слайс никогда не мутируется по месту (SetAnchors подменяет указатель целиком),
// поэтому отдача внутреннего среза read-only-потребителю безопасна. nil =
// набор пуст → verify трактует как no_trust_anchor.
func (a *AnchorSet) snapshot() []ed25519.PublicKey {
	if a == nil {
		return nil
	}
	p := a.keys.Load()
	if p == nil {
		return nil
	}
	return *p
}
