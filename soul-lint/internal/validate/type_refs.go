package validate

// Офлайн-проверка переиспользуемых именованных типов (`service/<name>/types.yml`
// + `$type`-ссылки в input: сценария). Цель — поймать ДО keeper:
//   - битый каталог типов (input_type_duplicate / input_type_cycle между типами);
//   - ссылку из input: сценария на несуществующий тип (input_type_unknown);
//   - циклическую ссылку, видимую со стороны потребителя (input_type_cycle).
//
// Структурные инварианты ОДНОГО узла ($type-ref-conflict, невалидное имя)
// config-валидатор уже ловит на парсе сценария — здесь добавляется только то,
// что требует КАТАЛОГА типов (его config-парсер сценария в одиночку не видит).
//
// Каталог берётся из сиблинга `../../types.yml` относительно main.yml: раскладка
// сервиса — `<service>/scenario/<name>/main.yml`, types.yml лежит в корне
// `<service>/types.yml`. Отсутствие types.yml → проверка пропускается (типы
// опциональны; ссылка на тип при отсутствующем каталоге всё равно даст
// input_type_unknown при резолве пустого каталога). Любая I/O-/parse-ошибка
// каталога не валит линт сценария катастрофически — она прокидывается как
// собственная диагностика каталога.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// typeRefDiagnostics проверяет `$type`-ссылки input: сценария против каталога
// типов сервиса. scenarioPath — путь к main.yml; m — распарсенный сценарий
// (m==nil → nil: без input нечего резолвить). Возвращает диагностики каталога
// (duplicate/cycle между типами) + диагностики резолва input: (unknown/cycle).
func typeRefDiagnostics(scenarioPath string, m *config.ScenarioManifest) []diag.Diagnostic {
	if m == nil || len(m.Input) == 0 {
		return nil
	}
	if !inputHasTypeRef(m.Input) {
		// Сценарий не использует $type — каталог читать незачем (types.yml может
		// отсутствовать у сервиса без типов, это валидно).
		return nil
	}

	// Раскладка: <service>/scenario/<name>/main.yml → <service>/types.yml.
	serviceRoot := filepath.Dir(filepath.Dir(filepath.Dir(scenarioPath)))
	typesPath := filepath.Join(serviceRoot, config.TypesCatalogFile)

	data, err := os.ReadFile(typesPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Каталога нет, но сценарий ссылается на тип → резолв пустого каталога
			// даст input_type_unknown (ниже), указав конкретную битую ссылку.
			data = nil
		} else {
			return []diag.Diagnostic{{
				Level:   diag.LevelWarning,
				Phase:   diag.PhaseParse,
				File:    typesPath,
				Code:    "io_error",
				Message: err.Error(),
				Hint:    "types.yml присутствует, но не читается — $type-ссылки не проверены офлайн",
			}}
		}
	}

	catalog, catDiags := config.ParseTypeCatalog(typesPath, data)
	out := catDiags

	// Резолв input: сценария по каталогу: ловит input_type_unknown (ссылка на
	// отсутствующий тип) и input_type_cycle, видимые со стороны потребителя.
	// File-поле проставляем на путь сценария (диагностика про его input:).
	_, refDiags := config.ResolveTypeRefs(m.Input, catalog)
	for i := range refDiags {
		if refDiags[i].File == "" {
			refDiags[i].File = scenarioPath
		}
	}
	out = append(out, refDiags...)
	return out
}

// inputHasTypeRef — true, если хотя бы один узел input: (рекурсивно по items/
// properties/additional_properties) несёт `$type`-ссылку. Дешёвый short-circuit:
// без ссылок читать каталог не нужно.
func inputHasTypeRef(m config.InputSchemaMap) bool {
	for _, s := range m {
		if schemaHasTypeRef(s) {
			return true
		}
	}
	return false
}

func schemaHasTypeRef(s *config.InputSchema) bool {
	if s == nil {
		return false
	}
	if s.TypeRef != "" {
		return true
	}
	if schemaHasTypeRef(s.Items) {
		return true
	}
	if inputHasTypeRef(s.Properties) {
		return true
	}
	if ap, ok := s.AdditionalProperties.(*config.InputSchema); ok && schemaHasTypeRef(ap) {
		return true
	}
	return false
}
