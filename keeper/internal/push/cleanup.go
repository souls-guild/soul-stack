package push

import (
	"context"
	"errors"
	"fmt"
)

// Cleaner удаляет соул-артефакты с push-хоста после прогона (бинарь + плагины).
// Вызывается оператором явно через [SshDispatcher.Cleanup], не автоматически в
// конце SendApply: повторные прогоны выигрывают от SHA-256-кеша доставки, и
// чистка между ними дала бы лишние roundtrip-ы. Cleanup имеет смысл при
// выводе хоста из push-flow / смене soul-агента.
//
// Logs/journal (`/var/log/soul-stack/` и пр.) НЕ трогаются — это аудит-трейл,
// не короткоживущая инстал-зона.
type Cleaner interface {
	Cleanup(ctx context.Context, session Session) error
}

// ShaCleaner — дефолтная реализация поверх ssh exec `rm -rf`. Хост-layout
// фиксирован (`/var/lib/soul-stack/{bin,modules}/`), поэтому путь не приходит
// извне и shell-инжекшен невозможен.
type ShaCleaner struct{}

// NewShaCleaner — конструктор для DI-явности.
func NewShaCleaner() *ShaCleaner { return &ShaCleaner{} }

// Cleanup удаляет hostSoulDir и hostModulesDir вместе с их содержимым.
// /var/log/soul-stack/ намеренно НЕ затрагивается (аудит-данные).
//
// fail-closed: ненулевой exit `rm` (например — permissions) поднимается наверх,
// чтобы caller не считал, что хост вычищен «как-нибудь».
func (c *ShaCleaner) Cleanup(ctx context.Context, session Session) error {
	if session == nil {
		return errors.New("push/cleanup: session is nil")
	}
	cmd := fmt.Sprintf("rm -rf %s %s", hostSoulDir, hostModulesDir)
	if _, err := session.Run(ctx, cmd, nil); err != nil {
		return fmt.Errorf("push/cleanup: rm -rf %s %s: %w", hostSoulDir, hostModulesDir, err)
	}
	return nil
}
