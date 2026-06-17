package config

// Статическая модель путей `soulprint.self.<...>` для статпроверки CEL-предикатов
// (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`/`loop.when:`).
//
// Источник истины — [ADR-018](docs/adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)
// и [docs/soul/soulprint.md]: typed-схема `SoulprintFacts` (`os`/`kernel`/`cpu`/
// `memory`/`network`) плюс корневые `sid`/`hostname` и registry-проекции
// (`covens`, `choirs`, `role`). Каноническая форма обязательна: голая `soulprint.<x>` без
// `.self`/`.hosts`/`.where` — ошибка (см. soulprint.md «Каноническая форма»).
//
// Линтер сверяет ТОЛЬКО префикс известных полей. Хвост после массивного поля
// (`soulprint.self.network.interfaces[0].ipv4`) и опционально-вложенные ветки
// `extra` (зарезервированное расширение по ADR-018) принимаются как valid: они
// либо имеют динамическую форму (массивы, ключи интерфейса), либо относятся
// к будущим полям. Цель — ловить опечатки (`os.familly`/`memmory.total_mb`),
// а не быть полной типовой системой.

// soulprintSelfPaths — закрытый whitelist голых сегментов первого уровня под
// `soulprint.self.<segment>`. Дальнейший спуск проверяется через
// soulprintSelfSubPaths (вложенные сообщения).
//
// Регистрируется только то, что есть в SoulprintFacts (ADR-018) + registry-
// проекции (`covens`/`choirs`/`role`, see docs/soul/soulprint.md «Граница
// `Soulprint`↔`souls`-registry»).
var soulprintSelfTopLevel = map[string]bool{
	"sid":      true, // string, registry+collected
	"hostname": true, // string
	"covens":   true, // list<string>, registry-проекция
	"choirs":   true, // list<string>, registry-проекция (ADR-044, зеркало covens)
	"role":     true, // string|null, declared (bootstrap-create only)
	"os":       true, // OsFacts
	"kernel":   true, // KernelFacts
	"cpu":      true, // CpuFacts
	"memory":   true, // MemoryFacts
	"network":  true, // NetworkFacts
}

// soulprintSelfSubPaths — точные двухсегментные пути под `soulprint.self.<msg>.<field>`.
// Используются для проверки опечаток в полях вложенных сообщений. Если top-level
// сегмент есть, но второй неизвестен — флагаем (явно сверяемая опечатка типа
// `os.familly`). Если top-level — массив/строка (`covens`/`sid`/…), вторая
// проверка не запускается (любой суффикс принимается как dyn-доступ или
// runtime-индекс).
var soulprintSelfSubPaths = map[string]map[string]bool{
	"os": {
		"family":      true,
		"distro":      true,
		"version":     true,
		"codename":    true,
		"arch":        true,
		"pkg_mgr":     true,
		"init_system": true,
	},
	"kernel": {
		"version": true,
		"release": true,
	},
	"cpu": {
		"count":  true,
		"model":  true,
		"vendor": true,
	},
	"memory": {
		"total_mb":     true,
		"available_mb": true,
		"swap_mb":      true,
	},
	"network": {
		"primary_ip": true,
		"fqdn":       true,
		"interfaces": true, // список — дальнейший спуск динамический
	},
}

// soulprintScalarTopLevel — top-level сегменты с не-message-типом: спуска
// глубже регистра нет (нечего опечатать в имени поля). Используется, чтобы
// `soulprint.self.sid.startsWith(...)` (вызов метода) проходил без флага на
// «третий сегмент `startsWith`».
var soulprintScalarTopLevel = map[string]bool{
	"sid":      true,
	"hostname": true,
	"covens":   true,
	"choirs":   true,
	"role":     true,
}

// CovenLabelValidator — хук валидации coven-метки в `on: [coven, …]`-литералах
// за пределами формата (regex). Зеркалит интерфейс из keeper/internal/soul
// (которому shared/config принципиально не подчинён); в пилоте — no-op, потому
// что справочника окружений (Q1b ADR-008-amend) ещё нет. Когда справочник
// появится, его клиент подменяет [activeCovenLabelValidator] через
// [SetCovenLabelValidator] на старте бинаря; soul-lint без подмены продолжает
// работать как format-only (regex).
type CovenLabelValidator interface {
	Validate(label string) error
}

// NoopCovenLabelValidator — формат-only проверка (регекс делает [reCovenName]).
// Совместим по форме с keeper/internal/soul.NoopCovenLabelValidator.
type NoopCovenLabelValidator struct{}

// Validate всегда пропускает.
func (NoopCovenLabelValidator) Validate(string) error { return nil }

// activeCovenLabelValidator — package-level хук, который применяется внутри
// [validateOnField] поверх regex-формы (regex остаётся первой линией). nil →
// no-op (см. [covenLabelValidator]). Тесты могут установить кастомный хук через
// [SetCovenLabelValidator]; soul-lint в основном flow не вызывает Set и видит
// no-op. Поток «справочник из БД на keeper-стороне» — отдельный consumer,
// shared/config о нём не знает.
var activeCovenLabelValidator CovenLabelValidator

// SetCovenLabelValidator подменяет глобальный хук. Возвращает предыдущий — для
// детерминированного restore в тестах. nil-аргумент сбрасывает в no-op.
func SetCovenLabelValidator(v CovenLabelValidator) CovenLabelValidator {
	prev := activeCovenLabelValidator
	activeCovenLabelValidator = v
	return prev
}

// covenLabelValidator — текущий активный хук (или no-op, если не настроен).
// Возвращается [validateOnField] для проверки каждой не-CEL-обёрнутой метки.
func covenLabelValidator() CovenLabelValidator {
	if activeCovenLabelValidator == nil {
		return NoopCovenLabelValidator{}
	}
	return activeCovenLabelValidator
}
