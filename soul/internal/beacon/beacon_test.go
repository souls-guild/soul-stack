package beacon

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
)

// TestDefaultRegistry — soul-side половина инварианта «keeper-enum ==
// soul-registry == shared»: реестр [Default] покрывает РОВНО канонический набор
// [beaconaddr.All] (и не больше). Keeper-side половина (enum == beaconaddr.All)
// проверяется в keeper/internal/oracle; общий источник shared/beaconaddr даёт
// транзитивно keeper-enum == soul-registry — корень устранённого S3-бага.
func TestDefaultRegistry(t *testing.T) {
	r := Default()
	canonical := beaconaddr.All()
	for _, name := range canonical {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("Default-реестр не содержит канонический адрес %q", name)
		}
	}
	if len(r.Names()) != len(canonical) {
		t.Fatalf("soul-registry (%d) рассинхронен с beaconaddr.All (%d): %v", len(r.Names()), len(canonical), r.Names())
	}
	if _, ok := r.Lookup("core.beacon.nope"); ok {
		t.Error("Lookup неизвестного beacon должен вернуть false")
	}
}
