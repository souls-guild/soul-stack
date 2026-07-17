package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// CertPolicyTTL — окно валидности кешированного cert-policy-ответа одного
// Service-а (парный [StateSchemaTTL]-выбор: тот же баланс «дёрганий remote репо»
// vs. свежести секции certificate_rotation, положенной в манифест минуту назад).
const CertPolicyTTL = 60 * time.Second

// CertPolicyLister — поверхность listing-а cert-rotation-политики
// (`certificate_rotation:` + имена scenario/) из локально-материализованного
// снапшота Service-репо. Интерфейс — для подмены fake-ом в тестах; production —
// [artifact.ServiceLoader.LoadCertPolicy].
//
// Контракт: дёргается под per-(name+ref) lock-ом; ref явный, потому что разные
// версии сервиса могут иметь разную политику ротации.
type CertPolicyLister interface {
	ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)
}

// CertPolicyListerFunc — функциональная реализация [CertPolicyLister] (парный
// [StateSchemaListerFunc] для handler-side wire-up).
type CertPolicyListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)

// ListCertPolicy делает функцию реализующей [CertPolicyLister].
func (f CertPolicyListerFunc) ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
	return f(ctx, name, gitURL, ref)
}

// CertPolicyCache — in-process TTL-кеш ответа [CertPolicyLister.ListCertPolicy]
// по ключу `(name, ref)`. Per-Keeper (parity с [StateSchemaCache]): cert-policy —
// read-only представление, отставание между инстансами консистентность не рушит.
//
// Безопасен для конкурентного использования. Per-ключ Mutex сериализует «один
// in-flight loader на ключ».
type CertPolicyCache struct {
	lister CertPolicyLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[certPolicyKey]*certPolicyEntry
}

// certPolicyKey — композитный ключ кеша (name+ref раздельно для инвалидации по
// name; parity со [stateSchemaKey]).
type certPolicyKey struct {
	name string
	ref  string
}

// certPolicyEntry — одна запись кеша: lock сериализует concurrent loader-вызовы
// одного ключа; info/expires — закешированный ответ.
type certPolicyEntry struct {
	lock    sync.Mutex
	info    *artifact.CertPolicyInfo
	expires time.Time
}

// NewCertPolicyCache собирает кеш поверх lister-а. lister обязателен (паника при
// nil; parity с [NewStateSchemaCache]); ttl ≤ 0 нормализуется в [CertPolicyTTL].
func NewCertPolicyCache(lister CertPolicyLister, ttl time.Duration) *CertPolicyCache {
	if lister == nil {
		panic("serviceregistry.NewCertPolicyCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = CertPolicyTTL
	}
	return &CertPolicyCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[certPolicyKey]*certPolicyEntry),
	}
}

// ListCertPolicy возвращает cert-policy info для (name, gitURL, ref). Hit — из
// кеша; miss/истекший TTL — один loader-call под per-ключ lock-ом.
//
// Кешируется ТОЛЬКО success-ответ (parity с [StateSchemaCache.ListStateSchema]).
func (c *CertPolicyCache) ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
	entry := c.entryFor(certPolicyKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.info != nil {
		return cloneCertPolicyInfo(entry.info), nil
	}

	info, err := c.lister.ListCertPolicy(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.info = info
	entry.expires = c.now().Add(c.ttl)
	return cloneCertPolicyInfo(info), nil
}

// Invalidate сбрасывает все записи кеша для данного name (все варианты ref).
// Идемпотентен; parity с [StateSchemaCache.Invalidate].
func (c *CertPolicyCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) certPolicyEntry для key.
func (c *CertPolicyCache) entryFor(key certPolicyKey) *certPolicyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &certPolicyEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneCertPolicyInfo — копия, чтобы caller не мог изменить кешированную запись:
// Scenarios — копия slice, Rotation — deep-copy указателя (иначе общий *Rotation —
// латентная гонка, если появится писатель).
func cloneCertPolicyInfo(in *artifact.CertPolicyInfo) *artifact.CertPolicyInfo {
	if in == nil {
		return nil
	}
	out := *in
	if in.Scenarios != nil {
		out.Scenarios = make([]string, len(in.Scenarios))
		copy(out.Scenarios, in.Scenarios)
	}
	if in.Rotation != nil {
		r := *in.Rotation
		out.Rotation = &r
	}
	return &out
}
