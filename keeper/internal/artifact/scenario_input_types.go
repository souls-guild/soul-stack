package artifact

import (
	"errors"
	"io/fs"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// LoadScenarioManifestResolved — единственная RUNTIME-точка входа keeper-а для
// парсинга `scenario/<name>/main.yml` из снапшота сервиса. Делает то же, что
// [config.LoadScenarioManifestFromBytes], И дополнительно резолвит `$type`-ссылки
// input-схемы по каталогу типов сервиса (`<service>/types.yml`, сиблинг
// service.yml). После резолва `scn.Input` несёт самодостаточную схему БЕЗ `$type`
// — type-форма (object/array/properties/required) подставлена на место ссылки.
//
// Зачем здесь, а не у каждого потребителя: резолв `$type` нужен ВЕЗДЕ, где
// потребляется scn.Input на runtime — value-валидация submitted-input
// ([scenario.ValidateInput] → config.ResolveInputValues), form-prefill,
// secret-schema, render-pipeline. Без резолва узел `{$type: T}` имеет пустой
// `Type` → config.ResolveInputValues пропускает его БЕЗ проверки формы (submitted
// «не-object» в поле типа-object принимался молча). Единый chokepoint на загрузке
// (а не дубль-резолв per-consumer) гарантирует, что энфорсинг есть на каждом пути
// — это контракт «резолв на service-load» (параллель ResolveTypeRefs в soul-lint).
//
// art несёт LocalDir снапшота — types.yml читается через securejoin-ридер. rel —
// относительный путь main.yml (метка File-диагностик / сообщений парсера).
//
// Контракт возврата идентичен config.LoadScenarioManifestFromBytes: error — только
// при невозможности парсинга; diags несут все validation-ошибки, ВКЛЮЧАЯ
// input_type_unknown / input_type_cycle от резолва ссылок (потребитель проверяет
// diag.HasErrors как и прежде). Сервис без types.yml / сценарий без `$type` → схема
// проходит насквозь без изменений (back-compat).
func LoadScenarioManifestResolved(art *ServiceArtifact, rel string, data []byte) (*config.ScenarioManifest, *config.Document, []diag.Diagnostic, error) {
	scn, doc, diags, err := config.LoadScenarioManifestFromBytes(rel, data, config.ValidateOptions{})
	if err != nil {
		return scn, doc, diags, err
	}
	if scn == nil || len(scn.Input) == 0 {
		return scn, doc, diags, nil
	}

	resolved, rdiags := resolveScenarioInputTypeRefs(art, scn.Input, rel)
	if resolved != nil {
		scn.Input = resolved
	}
	diags = append(diags, rdiags...)
	return scn, doc, diags, nil
}

// resolveScenarioInputTypeRefs резолвит `$type`-ссылки input-схемы `in` по
// каталогу типов сервиса (`<art.LocalDir>/types.yml`). Возвращает НОВУЮ схему-map
// с подставленными типами и диагностики резолва (input_type_unknown /
// input_type_cycle / ошибки самого каталога).
//
// Отсутствие types.yml → каталог пустой: схема без `$type` проходит как есть
// (типы опциональны), а ссылка на тип при пустом каталоге даст input_type_unknown
// (указав битую ссылку, симметрично soul-lint). Любая иная I/O-ошибка чтения
// каталога — diag.LevelError (битый снапшот не должен молча пропускать тип-форму).
func resolveScenarioInputTypeRefs(art *ServiceArtifact, in config.InputSchemaMap, scenarioPath string) (config.InputSchemaMap, []diag.Diagnostic) {
	data, err := readSnapshotFile(art.LocalDir, config.TypesCatalogFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseParse,
				File: config.TypesCatalogFile, Code: "io_error",
				Message: err.Error(),
				Hint:    "types.yml присутствует, но не читается — $type-ссылки не резолвятся",
			}}
		}
		// Каталога нет: ссылка на тип всё равно даст input_type_unknown через
		// резолв пустого каталога ниже (укажет конкретную битую ссылку).
		data = nil
	}

	catalog, catDiags := config.ParseTypeCatalog(config.TypesCatalogFile, data)
	resolved, refDiags := config.ResolveTypeRefs(in, catalog)
	for i := range refDiags {
		if refDiags[i].File == "" {
			refDiags[i].File = scenarioPath
		}
	}
	return resolved, append(catDiags, refDiags...)
}
