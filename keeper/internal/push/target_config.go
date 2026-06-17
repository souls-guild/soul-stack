package push

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrTargetNotConfigured — SID отсутствует в `keeper.yml::push.targets[]`.
// Sentinel позволяет caller-у (pushorch executeAsync) различать «оператор
// забыл прописать хост» от транспортных ошибок (Authorize/Sign/Dial).
//
// Pilot-форма (S6, 2026-05-26): inline в keeper.yml. S7 заменит резолвер на
// `souls.ssh_target jsonb`-чтение, sentinel останется тот же.
var ErrTargetNotConfigured = errors.New("push: SID не сконфигурирован в push.targets[]")

// Дефолты pilot-резолвера для опущенных полей `push.targets[].*` (см.
// [config.KeeperPushTarget] doc). Каноничные unix-конвенции; оператор перебивает
// per-target явным значением.
const (
	defaultSSHPort  = 22
	defaultSSHUser  = "root"
	defaultSoulPath = "/usr/local/bin/soul"
)

// ConfigTargetResolver — pilot-резолвер SSH-реквизитов из `keeper.yml::push.targets[]`
// (S6 wire-up SshDispatcher, [ADR-032 amendment]). Индексирует записи по SID на
// конструкторе: O(1) lookup на горячем пути SendApply.
//
// Резолв: SID → [SSHTarget] с подставленными дефолтами (port 22 / user root /
// soul-path /usr/local/bin/soul, симметрия с docs/keeper/push.md).
// SID без записи → [ErrTargetNotConfigured] (fail-closed, оператор видит
// чёткое сообщение в push_runs.summary).
//
// S7 заменит этот источник на `souls.ssh_target jsonb` — интерфейс
// [TargetResolver] не изменится, поменяется только конструктор резолвера в
// daemon-wire-up.
type ConfigTargetResolver struct {
	byID map[string]SSHTarget
}

// NewConfigTargetResolver индексирует список `push.targets[]` по SID,
// подставляя дефолты на пустых полях. Дубликаты SID schema-фаза уже отвергла
// (validatePush в shared/config/schema.go); defense-in-depth — последняя
// запись перекрывает предыдущую (но не должно происходить на провалидированном
// конфиге).
func NewConfigTargetResolver(targets []config.KeeperPushTarget) *ConfigTargetResolver {
	byID := make(map[string]SSHTarget, len(targets))
	for _, t := range targets {
		if t.SID == "" {
			continue
		}
		byID[t.SID] = SSHTarget{
			Host:     t.SID,
			Port:     resolveInt(t.SSHPort, defaultSSHPort),
			User:     resolveStr(t.SSHUser, defaultSSHUser),
			SoulPath: resolveStr(t.SoulPath, defaultSoulPath),
		}
	}
	return &ConfigTargetResolver{byID: byID}
}

// Resolve реализует [TargetResolver]. Lookup по SID; не найден → ErrTargetNotConfigured.
func (r *ConfigTargetResolver) Resolve(_ context.Context, sid string) (SSHTarget, error) {
	t, ok := r.byID[sid]
	if !ok {
		return SSHTarget{}, fmt.Errorf("%w: %s", ErrTargetNotConfigured, sid)
	}
	return t, nil
}

func resolveInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func resolveStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
