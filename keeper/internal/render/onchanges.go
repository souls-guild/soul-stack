package render

import (
	"errors"
	"fmt"
)

// ErrOnChangesUnknownRegister — `onchanges:` ссылается на register-имя, которого
// нет среди register'ов задач прогона. Строгий вариант (ошибка, не warn): пустой
// onchanges-источник по опечатке молча превратил бы gating в «никогда не
// changed» → задача всегда пропускалась бы, маскируя баг автора scenario.
var ErrOnChangesUnknownRegister = errors.New("render: onchanges ссылается на несуществующий register")

// ErrOnFailUnknownRegister — `onfail:` ссылается на register-имя, которого нет
// среди register'ов задач прогона. Зеркало ErrOnChangesUnknownRegister: пустой
// onfail-источник по опечатке молча превратил бы rescue-gating в «никогда не
// failed» → onfail-задача всегда пропускалась бы, маскируя баг автора scenario.
var ErrOnFailUnknownRegister = errors.New("render: onfail ссылается на несуществующий register")

// resolveOnChanges превращает register-имена `onchanges:` (RenderedTask.
// onChangesNames) в task-индексы (RenderedTask.OnChangesIdx) по всему плану
// прогона (Variant A: резолв на Keeper-е, Soul оперирует индексами). Вызывается
// финальным проходом [Pipeline.Render], когда план собран целиком и все
// Index/Register известны (apply:destiny/loop дают сквозные индексы).
//
// Карта register-имя → Index строится по всем задачам плана; самоссылку
// (`onchanges: [self]` на задаче с `register: self`) НЕ исключаем особым кодом —
// она резолвится в собственный Index, а Soul gating на собственный (ещё не
// выполненный) register даст changed==false → скип. Это согласовано с порядком
// «requisites проверяются до запуска задачи».
//
// Неизвестное имя → [ErrOnChangesUnknownRegister] (строгий вариант, ловит
// опечатку register-id). Пустой onChangesNames → OnChangesIdx остаётся nil
// (безусловный запуск).
func resolveOnChanges(tasks []*RenderedTask) error {
	byRegister := registerIndex(tasks)
	for _, t := range tasks {
		if len(t.onChangesNames) == 0 {
			continue
		}
		idxs, err := resolveRegisterNames(byRegister, t.onChangesNames, t.Name, "onchanges", ErrOnChangesUnknownRegister)
		if err != nil {
			return err
		}
		t.OnChangesIdx = idxs
	}
	return nil
}

// resolveOnFail превращает register-имена `onfail:` (RenderedTask.onFailNames) в
// task-индексы (RenderedTask.OnFailIdx) по всему плану прогона. Полное зеркало
// resolveOnChanges (Variant A): разница только в семантике gating на Soul-е —
// onfail срабатывает по register.failed источника (rescue), а не register.changed.
//
// Неизвестное имя → [ErrOnFailUnknownRegister]. Пустой onFailNames → OnFailIdx
// остаётся nil (не-onfail-задача, gating не применяется).
func resolveOnFail(tasks []*RenderedTask) error {
	byRegister := registerIndex(tasks)
	for _, t := range tasks {
		if len(t.onFailNames) == 0 {
			continue
		}
		idxs, err := resolveRegisterNames(byRegister, t.onFailNames, t.Name, "onfail", ErrOnFailUnknownRegister)
		if err != nil {
			return err
		}
		t.OnFailIdx = idxs
	}
	return nil
}

// registerIndex строит карту register-имя → Index по всем задачам плана.
// Задачи без register: в карту не попадают (адресуются только своим idx).
func registerIndex(tasks []*RenderedTask) map[string]int {
	byRegister := make(map[string]int, len(tasks))
	for _, t := range tasks {
		if t.Register != "" {
			byRegister[t.Register] = t.Index
		}
	}
	return byRegister
}

// resolveRegisterNames резолвит список register-имён requisite-а в task-индексы по
// карте byRegister. Неизвестное имя → обёрнутая sentinel-ошибка unknownErr с
// координатами (имя задачи, kind requisite-а, само имя). kind — "onchanges"/"onfail"
// для текста ошибки.
func resolveRegisterNames(byRegister map[string]int, names []string, taskName, kind string, unknownErr error) ([]int, error) {
	idxs := make([]int, 0, len(names))
	for _, name := range names {
		srcIdx, ok := byRegister[name]
		if !ok {
			return nil, fmt.Errorf("%w: задача %q → %s: [%s] (нет задачи с register: %s)",
				unknownErr, taskName, kind, name, name)
		}
		idxs = append(idxs, srcIdx)
	}
	return idxs, nil
}
