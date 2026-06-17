package oracle

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Sentinel-ошибки service-валидации Vigil / Decree (S3). Management-Service
// маппит их в 422 через errors.Is (не по строковому префиксу текста —
// переименование диагностики не должно молча сломать маппинг 422→500).
// Конкретный диагностический текст несёт обёрнутая ошибка (уже public —
// формируется здесь, без internal SQL/stack).
var ErrValidation = errors.New("oracle: validation failed")

// Паттерны форм, дублирующие CHECK-и миграции 041 (отбить bad value до
// round-trip-а — лучшая диагностика, нет лишнего обращения на битом вводе).
//
//   - NamePattern        — vigils_name_format / decrees_name_format (kebab 1..63).
//   - CovenPattern       — формат элемента coven-метки субъекта (kebab; per-element
//     CHECK для text[] декларативно без триггера не выразить — миграция 041).
//   - IncarnationPattern — decrees_incarnation_name_format (= incarnation.name,
//     корневая Coven-метка, ADR-008).
//   - ScenarioPattern    — decrees_scenario_format (snake_case named scenario).
const (
	NamePattern        = `^[a-z0-9-]{1,63}$`
	CovenPattern       = `^[a-z0-9][a-z0-9-]*$`
	IncarnationPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	ScenarioPattern    = `^[a-z][a-z0-9_]*$`
)

var (
	nameRe        = regexp.MustCompile(NamePattern)
	covenRe       = regexp.MustCompile(CovenPattern)
	incarnationRe = regexp.MustCompile(IncarnationPattern)
	scenarioRe    = regexp.MustCompile(ScenarioPattern)
)

// ValidName проверяет имя Vigil / Decree на каноническую форму (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven проверяет одну Coven-метку субъекта.
func ValidCoven(coven string) bool { return covenRe.MatchString(coven) }

// ValidIncarnationName проверяет таргет-incarnation Decree-а (формат
// incarnation.name).
func ValidIncarnationName(name string) bool { return incarnationRe.MatchString(name) }

// ValidScenario проверяет имя named scenario (action_scenario Decree-а).
func ValidScenario(name string) bool { return scenarioRe.MatchString(name) }

// knownBeaconAddrs — closed enum адресов встроенных core-beacon (ADR-030,
// VigilDef.check). Строится из канонического списка [beaconaddr.All] — единого
// источника истины, общего с soul-side реестром `beacon.Default()`
// (`soul/internal/beacon`). keeper НЕ импортирует soul (compiler-изоляция,
// ADR-011), поэтому общий список живёт в нейтральном `shared/beaconaddr`: это
// убирает прежний дубль keeper↔soul (S3-баг: рассинхрон давал ложный 422 на
// валидном Vigil). Plugin-kind `soul_beacon` (community-проверки, S5) ещё не
// введён — до того check_addr ограничен этим набором (неизвестный check —
// ошибка валидации, а не молча неисполнимый Vigil).
var knownBeaconAddrs = buildKnownBeaconAddrs()

func buildKnownBeaconAddrs() map[string]struct{} {
	addrs := beaconaddr.All()
	m := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		m[a] = struct{}{}
	}
	return m
}

// ValidCheckAddr — членство check_addr в [knownBeaconAddrs].
func ValidCheckAddr(addr string) bool {
	_, ok := knownBeaconAddrs[addr]
	return ok
}

// validateCovenList проверяет per-element формат coven-меток субъекта. Пустой
// список допустим у вызывающего (XOR-проверка решает, задан ли субъект);
// здесь — только формат непустых элементов.
func validateCovenList(coven []string) error {
	for _, c := range coven {
		if !ValidCoven(c) {
			return fmt.Errorf("%w: invalid coven %q (must match %s)", ErrValidation, c, CovenPattern)
		}
	}
	return nil
}

// validateSubjectXOR проверяет XOR-инвариант субъекта (coven-список XOR sid):
// ровно одно из непустого coven-списка / непустого sid задано. Симметрично
// CHECK vigils_subject_xor / decrees_subject_xor (defence in depth) и
// [augur.ValidateSubjectXOR]. SID-формат на этом слое не нормируется
// (FQDN-семантика — registry-сторона).
func validateSubjectXOR(coven []string, sid *string) error {
	hasCoven := len(coven) > 0
	hasSID := sid != nil && *sid != ""
	if hasCoven == hasSID {
		return fmt.Errorf("%w: subject must be exactly one of coven / sid (XOR)", ErrValidation)
	}
	if hasCoven {
		return validateCovenList(coven)
	}
	return nil
}

// validateInterval проверяет формат duration-строки interval Vigil-а через
// тот же парсер, что и прочие duration-поля keeper.yml ([config.ParseDuration]).
func validateInterval(spec string) error {
	if spec == "" {
		return fmt.Errorf("%w: interval is empty", ErrValidation)
	}
	if _, err := config.ParseDuration(spec); err != nil {
		return fmt.Errorf("%w: invalid interval %q: %s", ErrValidation, spec, err)
	}
	return nil
}

// validateCooldown проверяет формат cooldown Decree-а. Пустая строка допустима
// (репозиторий подставит DEFAULT '0s' — cooldown выключен).
func validateCooldown(spec string) error {
	if spec == "" {
		return nil
	}
	if _, err := config.ParseDuration(spec); err != nil {
		return fmt.Errorf("%w: invalid cooldown %q: %s", ErrValidation, spec, err)
	}
	return nil
}
