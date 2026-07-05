package harness

// applyRunTerminalFailures — не-success терминалы apply_runs (keeper crud.go):
// задача упала окончательно → WaitApplySuccess немедленно фейлит с дампом матрицы.
var applyRunTerminalFailures = map[string]bool{
	"failed":    true,
	"cancelled": true,
	"orphaned":  true,
	"no_match":  true,
}

// ApplyRunRow — снимок строки apply_runs (PK = apply_id+sid) для решения WaitApplySuccess.
type ApplyRunRow struct {
	SID    string
	Status string
}

// applySettled решает по снимку строк прогона + признаку «apply ещё в полёте»
// (incarnation.applying_apply_id == applyID), достигнут ли УСПЕШНЫЙ терминал.
//
// NIM-46: apply_runs наполняется инкрементально — keeper-строка (sid="keeper",
// on:keeper-задачи) вставляется и success СТРОГО ДО планирования soul-строк
// (run.go: keeper-tasks → host-dispatch). Поэтому «все видимые строки success»
// само по себе НЕ значит «прогон завершён» — soul-строки могли ещё не появиться
// (NIM-45-гонка). Авторитетный сигнал «keeper больше НЕ вставит строк» — снятие
// apply-брекета applying_apply_id (ставится в старте apply lockApplyingWithEpochSQL
// ДО dispatch, снимается в единой терминальной точке UpdateStateFromRun). Пока
// брекет держит ЭТОТ applyID — ждём, даже если все видимые строки success.
//
// Возврат: done — успешный терминал; при терминал-фейле — (false, sid, status)
// для диагностического Fatal у вызывающего.
func applySettled(rows []ApplyRunRow, applyInFlight bool) (done bool, failSID, failStatus string) {
	if len(rows) == 0 {
		return false, "", "" // строк ещё нет — ждём вставки keeper-строки
	}
	allSuccess := true
	for _, r := range rows {
		if r.Status == "success" {
			continue
		}
		if applyRunTerminalFailures[r.Status] {
			return false, r.SID, r.Status
		}
		allSuccess = false // planned/claimed/running/dispatched — ещё в работе
	}
	if !allSuccess {
		return false, "", ""
	}
	// Все строки success — завершено, только если брекет снят (иначе keeper в
	// keeper-window ещё не распланировал soul-строки, см. инвариант выше).
	return !applyInFlight, "", ""
}
