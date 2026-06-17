//go:build !linux

package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// InotifyName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check). На
// non-Linux платформе сам beacon отдаёт ошибку, но адрес-константа доступна
// для единого keeper-enum / soul-registry source-of-truth.
const InotifyName = beaconaddr.Inotify

// InotifyBeacon — stub-реализация на non-Linux платформах (V5-3, ADR-030
// amendment 2026-05-26). Любой Check возвращает ошибку
// "platform not supported": scheduler логирует и пропускает тик (baseline
// не устанавливается, Portent не эмитится — ошибка проверки ≠ смена состояния
// хоста). Сам реестр registry-у beacon-а доступен на всех платформах
// (рассинхрон Default vs beaconaddr.All — программный баг сборки),
// но Vigil на non-Linux работать не будет.
type InotifyBeacon struct{}

// NewInotify собирает stub-beacon. Никаких ресурсов не аллоцирует.
func NewInotify() *InotifyBeacon { return &InotifyBeacon{} }

func (*InotifyBeacon) Check(_ context.Context, _ *structpb.Struct) (State, *structpb.Struct, error) {
	return "", nil, fmt.Errorf("core.beacon.inotify: platform not supported (Linux-only, V5-3)")
}
