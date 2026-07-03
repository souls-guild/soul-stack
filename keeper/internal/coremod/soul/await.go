package soul

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/config"

	"google.golang.org/protobuf/types/known/structpb"
)

// defaultAwaitPollInterval — период опроса presence по умолчанию (parity
// keeper.yml::acolyte_poll_interval). Мелкий, но не нулевой: presence-проверка
// — один Redis-pipeline на EXISTS-команду per SID (дёшево), 2s достаточно
// частый для онбординга и не создаёт нагрузки на Redis при долгом барьере.
const defaultAwaitPollInterval = 2 * time.Second

// awaitConfig — разобранные+валидированные параметры барьера онбординга
// (ADR-061). nil-указатель означает «барьер не запрошен».
//
// requireFacts (ADR-061 amendment, 7-я стена live-create): при
// refresh_soulprint: true SID засчитывается барьером только когда online
// (presence-lease) И typed soulprint записан в PG — иначе render следующего
// Passage читал бы soulprint.self.* до асинхронной записи первого репорта.
type awaitConfig struct {
	timeout      time.Duration
	minCount     int
	pollInterval time.Duration
	requireFacts bool
}

// awaitResult — итог барьера. online/pending — по presence-lease; factless —
// online-SID без typed facts (только при requireFacts, иначе пусто); ready —
// засчитанные барьером (online, при requireFacts — минус factless).
// lastErr — presence/facts-ошибка последних опросов для диагностики таймаута.
type awaitResult struct {
	online    []string
	pending   []string
	factless  []string
	ready     []string
	satisfied bool
	lastErr   error
}

// validateAwaitParams — статическая проверка await-полей (для Validate /
// soul-lint runtime-страховки). sidCount — число регистрируемых SID (для
// проверки await_min_count ≤ len(sids)). Возвращает список текстовых ошибок.
func validateAwaitParams(params *structpb.Struct, sidCount int) []string {
	awaitOnline, _, err := util.OptBoolParam(params, "await_online")
	if err != nil {
		return []string{err.Error()}
	}

	var errs []string
	timeoutStr, terr := util.OptStringParam(params, "await_timeout")
	if terr != nil {
		errs = append(errs, terr.Error())
	} else if timeoutStr != "" {
		if _, perr := config.ParseDuration(timeoutStr); perr != nil {
			errs = append(errs, fmt.Sprintf("param %q: invalid duration %q", "await_timeout", timeoutStr))
		}
	}

	pollStr, perr := util.OptStringParam(params, "await_poll_interval")
	if perr != nil {
		errs = append(errs, perr.Error())
	} else if pollStr != "" {
		if _, dErr := config.ParseDuration(pollStr); dErr != nil {
			errs = append(errs, fmt.Sprintf("param %q: invalid duration %q", "await_poll_interval", pollStr))
		}
	}

	minCount, minSet, merr := util.OptIntParam(params, "await_min_count")
	if merr != nil {
		errs = append(errs, merr.Error())
	} else if minSet {
		if minCount <= 0 {
			errs = append(errs, fmt.Sprintf("param %q: must be > 0", "await_min_count"))
		} else if sidCount > 0 && minCount > int64(sidCount) {
			errs = append(errs, fmt.Sprintf("param %q: %d exceeds number of registered SIDs (%d)", "await_min_count", minCount, sidCount))
		}
	}

	// await_timeout обязателен при await_online (барьер не должен висеть вечно).
	if awaitOnline && timeoutStr == "" {
		errs = append(errs, fmt.Sprintf("param %q is required when %q is true", "await_timeout", "await_online"))
	}
	return errs
}

// parseAwait разбирает await-параметры в awaitConfig. Возвращает (nil, nil),
// если барьер не запрошен (await_online опущен/false). Ошибка — невалидный
// параметр / недостижимый кворум / превышение потолка / нет presence-checker-а.
//
// Статическая часть валидации (типы, duration-формат, min ≤ len, обязательность
// await_timeout) делегируется validateAwaitParams — единственный источник этих
// текстов, чтобы Apply-путь и Validate-путь не расходились формулировками.
// Здесь остаётся то, что validate выразить не может: presence-checker,
// положительность timeout и потолок max_await_timeout (зависят от runtime-state
// модуля, а не только от params).
//
// Потолок (max_await_timeout, ADR-061): fail-closed — await_timeout > потолка
// завершается ошибкой ДО любого poll-а (явная ошибка, НЕ тихое обрезание).
func (m *Module) parseAwait(params *structpb.Struct, sidCount int) (*awaitConfig, error) {
	awaitOnline, _, err := util.OptBoolParam(params, "await_online")
	if err != nil {
		return nil, err
	}
	if !awaitOnline {
		return nil, nil
	}

	if errs := validateAwaitParams(params, sidCount); len(errs) > 0 {
		return nil, errors.New(errs[0])
	}

	// Барьер без presence-источника невозможен: молчаливый success недопустим.
	if m.presence == nil {
		return nil, errors.New("await_online requires presence-checker (Redis SID-lease), not configured")
	}

	// validateAwaitParams гарантировал валидный непустой await_timeout.
	timeoutStr, _ := util.OptStringParam(params, "await_timeout")
	timeout, _ := config.ParseDuration(timeoutStr)
	if timeout <= 0 {
		return nil, fmt.Errorf("param %q: must be > 0", "await_timeout")
	}

	// Потолок keeper.yml::max_await_timeout — fail-closed DoS-guard.
	ceiling := m.resolvedMaxAwaitTimeout()
	if timeout > ceiling {
		return nil, fmt.Errorf("param %q (%s) exceeds keeper.yml max_await_timeout ceiling (%s)", "await_timeout", timeout, ceiling)
	}

	cfg := &awaitConfig{timeout: timeout, minCount: sidCount, pollInterval: defaultAwaitPollInterval}

	if minCount, minSet, _ := util.OptIntParam(params, "await_min_count"); minSet {
		cfg.minCount = int(minCount)
	}

	if pollStr, _ := util.OptStringParam(params, "await_poll_interval"); pollStr != "" {
		if poll, _ := config.ParseDuration(pollStr); poll > 0 {
			cfg.pollInterval = poll
		}
	}
	return cfg, nil
}

// resolvedMaxAwaitTimeout возвращает эффективный потолок await_timeout из
// текущего snapshot keeper.yml (hot-reload через maxAwaitTimeout-провайдер).
// nil-провайдер / пустая строка / невалид → config.DefaultMaxAwaitTimeout.
func (m *Module) resolvedMaxAwaitTimeout() time.Duration {
	if m.maxAwaitTimeout == nil {
		return config.DefaultMaxAwaitTimeout
	}
	raw := m.maxAwaitTimeout()
	if raw == "" {
		return config.DefaultMaxAwaitTimeout
	}
	d, err := config.ParseDuration(raw)
	if err != nil || d <= 0 {
		return config.DefaultMaxAwaitTimeout
	}
	return d
}

// awaitOnline блокирующе поллит готовность SID до ready ≥ minCount или
// истечения timeout. Готовность: online (Redis SID-lease); при
// cfg.requireFacts — online И typed soulprint в PG (ADR-061 amendment:
// facts уже записаны → нулевое ожидание rerun/create_from_souls; ждёт только
// первый репорт provision-from-zero).
//
// res.lastErr — presence/facts-ошибка с последних опросов (для диагностики
// таймаута: «инфра недоступна» vs «хосты не онбордились»). Возвращается и при
// satisfied=false без фатала, чтобы вызывающий различил причину недобора.
//
// Источник истины online — lease (PresenceChecker), НЕ PG souls.status
// (ADR-006(a)/ADR-061). Persistent infra-ошибка → error (B1-strict не может
// подтвердить кворум вслепую). context-cancel (отмена прогона) — тоже error.
func (m *Module) awaitOnline(ctx context.Context, sids []string, cfg *awaitConfig) (awaitResult, error) {
	bctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// Первый опрос — сразу (хосты могли уже быть online до шага), затем по тикеру.
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	var res awaitResult
	polled := false // хоть один опрос дошёл до presence-результата
	for {
		alive, perr := m.presence.SoulsStreamAlive(bctx, sids)
		if perr != nil {
			res.lastErr = perr
		} else {
			polled = true
			res.online, res.pending = splitOnline(sids, alive)
			res.ready, res.factless = res.online, nil
			res.lastErr = nil // успешный опрос сбрасывает «висящую» infra-ошибку
			if cfg.requireFacts {
				withFacts, ferr := m.Store.SoulsWithSoulprint(bctx, sids)
				if ferr != nil {
					// facts неизвестны → кворум на этом опросе не оцениваем.
					res.lastErr = ferr
					res.ready = nil
				} else {
					res.ready, res.factless = splitFacts(res.online, withFacts)
				}
			}
			if res.lastErr == nil && len(res.ready) >= cfg.minCount {
				res.satisfied = true
				return res, nil
			}
		}

		select {
		case <-bctx.Done():
			// Таймаут барьера: если хоть раз дошли до presence-результата, поля
			// диагностики посчитаны (B1-strict diagnostics). Если ВСЕ опросы падали
			// infra-ошибкой — отдаём её фаталом (источник готовности недоступен).
			if !polled && res.lastErr != nil {
				res.pending = sids
				return res, fmt.Errorf("await_online: presence check failed: %w", res.lastErr)
			}
			if !polled {
				res.pending = sids
			}
			// res.lastErr ненулевой здесь → persistent сбой на последних опросах
			// при частичном недоборе: остаётся в res для обогащения диагностики.
			return res, nil
		case <-ticker.C:
		}
	}
}

// barrierTimeoutMessage — диагностика B1-strict-провала барьера. При
// requireFacts классы недобора разделены: «not online» (нет lease) vs «online
// but factless» (lease есть, typed soulprint ещё не записан) — иначе оператор
// не отличит несостоявшийся онбординг от гонки первого репорта.
func barrierTimeoutMessage(sids []string, cfg *awaitConfig, res awaitResult) string {
	var msg string
	if cfg.requireFacts {
		msg = fmt.Sprintf(
			"onboarding barrier: %d/%d souls ready (online+soulprint) to await_min_count=%d within %s",
			len(res.ready), len(sids), cfg.minCount, cfg.timeout)
		if len(res.pending) > 0 {
			msg += fmt.Sprintf(" (not online: %v)", res.pending)
		}
		if len(res.factless) > 0 {
			msg += fmt.Sprintf(" (online but factless: %v)", res.factless)
		}
		if res.lastErr != nil {
			msg += fmt.Sprintf(" (last error: %v)", res.lastErr)
		}
		return msg
	}
	msg = fmt.Sprintf(
		"onboarding barrier: %d/%d souls online to await_min_count=%d within %s (pending: %v)",
		len(res.online), len(sids), cfg.minCount, cfg.timeout, res.pending)
	// Persistent presence-сбой на последних опросах: иначе infra-проблема
	// (redis недоступен) маскируется под «хосты не онбордились».
	if res.lastErr != nil {
		msg += fmt.Sprintf(" (last presence error: %v)", res.lastErr)
	}
	return msg
}

// splitOnline делит набор SID на online (есть в alive-множестве) и pending.
// Детерминированный порядок — по входному порядку sids.
func splitOnline(sids []string, alive map[string]struct{}) (online, pending []string) {
	online = make([]string, 0, len(sids))
	pending = make([]string, 0)
	for _, sid := range sids {
		if _, ok := alive[sid]; ok {
			online = append(online, sid)
		} else {
			pending = append(pending, sid)
		}
	}
	return online, pending
}

// splitFacts делит online-набор на ready (typed soulprint записан) и factless.
// Порядок — по входному порядку online.
func splitFacts(online []string, withFacts map[string]struct{}) (ready, factless []string) {
	ready = make([]string, 0, len(online))
	factless = make([]string, 0)
	for _, sid := range online {
		if _, ok := withFacts[sid]; ok {
			ready = append(ready, sid)
		} else {
			factless = append(factless, sid)
		}
	}
	return ready, factless
}
