package statemigrate

import "fmt"

// ParseError — ошибка разбора файла миграции (`NNN_to_MMM.yml`): синтаксис
// YAML, отсутствие/конфликт дискриминатора операции, неверная форма поля.
// Code — snake_case-идентификатор класса (стабилен для тестов/диагностики),
// симметрично error-кодам config-слоя ([destiny_tasks.go]).
type ParseError struct {
	Code string // migration_* (см. константы ниже)
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

// Коды ParseError. Все с префиксом migration_ (область DSL миграций).
const (
	CodeYAMLParse        = "migration_yaml_parse_error"   // невалидный YAML
	CodeEmptyDocument    = "migration_empty_document"     // пустой файл
	CodeVersionMissing   = "migration_version_missing"    // нет from_version/to_version
	CodeVersionInvalid   = "migration_version_invalid"    // to_version != from_version+1 и т.п.
	CodeOpDiscriminator  = "migration_op_discriminator"   // не ровно один ключ операции
	CodeOpFieldMissing   = "migration_op_field_missing"   // у операции нет обязательного поля
	CodeForeachMissingAs = "migration_foreach_missing_as" // foreach без as:
)

// EvalError — ошибка применения операции к state: непереносимый путь,
// конфликт rename-to, нерезолвимая CEL-интерполяция, неитерируемый foreach.
// Class — snake_case-класс; Path — адрес операции (для диагностики); Err —
// обёрнутая первопричина (CEL-ошибка и т.п.), может быть nil.
type EvalError struct {
	Class string
	Path  string
	Msg   string
	Err   error
}

func (e *EvalError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s [%s]: %s", e.Class, e.Path, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Msg)
}

func (e *EvalError) Unwrap() error { return e.Err }

// Классы EvalError.
const (
	ClassRenameToExists = "migration_rename_to_exists" // rename/move в уже существующий to
	ClassPathTraverse   = "migration_path_traverse"    // промежуточный сегмент пути — не map
	ClassPathSegment    = "migration_path_segment"     // невалидный/нерезолвимый сегмент пути
	ClassForeachType    = "migration_foreach_type"     // in: дал не список и не map
	ClassCELInterp      = "migration_cel_interp"       // ошибка резолва ${ … }
	ClassChainVersion   = "migration_chain_version"    // разрыв версий в цепочке
)
