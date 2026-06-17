package watchman

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
)

// NamedPinger — именованная зависимость для probe. Имя попадает в сообщение об
// ошибке, чтобы оператор видел, ЧТО именно отвалилось (postgres / redis).
type NamedPinger struct {
	Name   string
	Pinger health.Pinger
}

// depsProbe — реализация [HealthProbe] поверх набора `health.Pinger`-ов. Тот же
// контракт зависимостей, что у `/readyz` (PG + Redis обязательны для обслуживания
// запросов): инстанс, потерявший их, изолирован. nil-Pinger пропускается
// (симметрия с health.Readyz: Redis может быть отключён в dev — тогда его
// отсутствие не считается изоляцией).
//
// Probe идёт ПОСЛЕДОВАТЕЛЬНО и short-circuit-ит на первой же ошибке: Watchman-у
// достаточно факта «хотя бы одна зависимость недоступна», полный список
// провалов (как в `/readyz`-JSON) ему не нужен — это экономит ping на втором
// ресурсе, когда первый уже отвалился.
type depsProbe struct {
	pingers []NamedPinger
}

// NewDepsProbe собирает [HealthProbe] из именованных Pinger-ов (обычно PG +
// Redis, те же, что в `/readyz`). nil-Pinger-ы отфильтровываются. Если после
// фильтрации не осталось ни одного — [ErrNoProbeDeps] (Watchman без зависимостей
// бессмыслен).
func NewDepsProbe(pingers ...NamedPinger) (HealthProbe, error) {
	live := make([]NamedPinger, 0, len(pingers))
	for _, p := range pingers {
		if p.Pinger != nil {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return nil, ErrNoProbeDeps
	}
	return &depsProbe{pingers: live}, nil
}

// Probe пингует зависимости последовательно, возвращая первую ошибку (с именем
// зависимости). nil — все здоровы. ctx уже несёт per-tick timeout от Watchman-а.
func (p *depsProbe) Probe(ctx context.Context) error {
	for _, np := range p.pingers {
		if err := np.Pinger.Ping(ctx); err != nil {
			return fmt.Errorf("%s: %w", np.Name, err)
		}
	}
	return nil
}
