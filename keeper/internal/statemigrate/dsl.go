package statemigrate

// Migration — один шаг миграции state_schema (`migrations/<NNN>_to_<MMM>.yml`),
// преобразующий incarnation.state с версии FromVersion на ToVersion. Чистая
// функция state_v<N> → state_v<M> ([ADR-019], [docs/migrations.md]).
type Migration struct {
	FromVersion int
	ToVersion   int
	Description string
	Transform   []Op
}

// Chain — упорядоченная цепочка шагов миграции (001→002→003…). [Apply]
// прогоняет её последовательно; ToVersion одного шага должен совпадать с
// FromVersion следующего (валидируется в [Apply]).
type Chain []*Migration

// Op — одна операция блока transform:. Дискриминируемое объединение: ровно
// одно из полей non-nil ([docs/migrations.md §«Операции transform:»]). move —
// исторический алиас rename (общий code-path applyRename, см. apply.go).
type Op struct {
	Rename  *RenameOp  // rename / move: переместить значение from → to
	Set     *SetOp     // set: записать value в path
	Delete  *DeleteOp  // delete: удалить значение по path (нет — no-op)
	Foreach *ForeachOp // foreach: структурный цикл по списку/значениям map
}

// RenameOp — `rename`/`move`: { from: <path>, to: <path> }. Перенос значения;
// существующий to → ошибка (явный delete перед rename).
type RenameOp struct {
	From string
	To   string
}

// SetOp — `set`: записать Value в Path. Value — произвольное YAML-значение
// (map/list/scalar) с возможными `${ … }`-CEL-интерполяциями в строковых
// листьях (рекурсивный обход, apply.go). Существующий Path перезаписывается.
type SetOp struct {
	Path  string
	Value any
}

// DeleteOp — `delete`: { path: <path> }. Несуществующий path → no-op.
type DeleteOp struct {
	Path string
}

// ForeachOp — `foreach`: структурный цикл. In — CEL-выражение, дающее список
// либо map; над списком As биндится к ЭЛЕМЕНТУ, над map — к ЗНАЧЕНИЮ. Do —
// вложенный transform-список, исполняемый на каждой итерации с добавленным в
// scope As. Вложенность foreach допустима.
type ForeachOp struct {
	In string
	As string
	Do []Op
}
