package clouddriver

import (
	"context"
	"strconv"
)

// ProbeResult — итог одного опроса готовности VM поллером [WaitUntilReady].
type ProbeResult struct {
	// Ready — VM достигла целевого состояния (running + есть IP/DNS): поллер
	// перестаёт её опрашивать.
	Ready bool
	// Err — terminal-ошибка опроса (VM ушла в error/terminated, либо
	// провайдер вернул детерминированный отказ). Поллер прекращает опрос этой
	// VM и помечает её failed. Транзиентные ошибки опроса драйвер НЕ возвращает
	// сюда — он их глотает (вернёт Ready=false, Err=nil) и поллер повторит.
	Err error
}

// ReadyProbe — per-provider предикат готовности одной VM. Единственное, что
// драйвер пишет сам для wait-фазы; цикл поллинга/backoff/ctx-cancel/anti-orphan
// берёт на себя SDK. Драйвер опрашивает VM (DescribeInstances) и возвращает
// [ProbeResult]. ctx уважать не обязан — поллер сам прерывает ожидание между
// раундами по ctx.
type ReadyProbe func(ctx context.Context, vmID string) ProbeResult

// WaitResult — результат [WaitUntilReady] по одной VM.
type WaitResult struct {
	VMID string
	// Ready — VM дождалась готовности.
	Ready bool
	// Err — terminal-ошибка опроса (если была); nil для Ready и для
	// прерванных по ctx (для них Ready=false, Err=nil — отличить можно по
	// возврату самого WaitUntilReady = ctx.Err()).
	Err error
}

// WaitUntilReady опрашивает все vmIDs предикатом probe с backoff-интервалами,
// пока каждая VM не станет Ready либо не вернёт terminal-Err. progress — опц.
// колбэк диагностики (message на каждый раунд), nil допустим.
//
// Anti-orphan (reference-приём для тиража): при ctx-cancel/timeout функция НЕ
// бросает всё — возвращает per-VM [WaitResult] для УЖЕ опрошенных VM (готовые
// помечены Ready=true, ещё-не-готовые — Ready=false) + ctx.Err(). Драйвер по
// этому списку понимает, какие VM создались, но не доехали, и помечает их
// failed с заполненным vm_id — чтобы Keeper мог их Destroy (см. RunInstances-
// flow в soul-cloud-aws). Без этого ctx-cancel в фазе wait оставил бы orphan-VM
// без vm_id у Keeper-а.
func WaitUntilReady(ctx context.Context, cfg BackoffConfig, vmIDs []string, probe ReadyProbe, progress func(string)) ([]WaitResult, error) {
	results := make([]WaitResult, len(vmIDs))
	for i, id := range vmIDs {
		results[i].VMID = id
	}

	pending := make(map[int]struct{}, len(vmIDs))
	for i := range vmIDs {
		pending[i] = struct{}{}
	}

	attempt := 0
	for len(pending) > 0 {
		for i := range pending {
			res := probe(ctx, vmIDs[i])
			switch {
			case res.Err != nil:
				results[i].Err = res.Err
				delete(pending, i)
			case res.Ready:
				results[i].Ready = true
				delete(pending, i)
			}
		}
		if len(pending) == 0 {
			break
		}
		if progress != nil {
			progress(waitProgressMsg(len(pending), len(vmIDs), attempt))
		}
		// ctx-aware ожидание следующего раунда; отмена → anti-orphan возврат.
		if err := sleepCtx(ctx, cfg.next(attempt)); err != nil {
			return results, err
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			return results, ErrWaitDeadline
		}
	}
	return results, nil
}

// ErrWaitDeadline — wait-поллер исчерпал MaxAttempts, не дождавшись готовности
// всех VM. Симметрично ctx.DeadlineExceeded, но без зависимости от наличия
// дедлайна в ctx (лимит задан числом попыток).
var ErrWaitDeadline = waitDeadlineError{}

type waitDeadlineError struct{}

func (waitDeadlineError) Error() string { return "wait-until-ready: max attempts exhausted" }

func waitProgressMsg(pending, total, attempt int) string {
	return "wait-until-ready: " +
		strconv.Itoa(total-pending) + "/" + strconv.Itoa(total) + " ready (attempt " + strconv.Itoa(attempt+1) + ")"
}
