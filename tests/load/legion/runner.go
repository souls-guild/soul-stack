package legion

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// metricsSettleDelay — пауза перед скрейпом пиковых метрик, чтобы
// keeper_grpc_streams_active успел отразить весь ramp (см. использование).
const metricsSettleDelay = 2 * time.Second

// drainSettleDelay — пауза после teardown стримов перед финальным скрейпом
// keeper_grpc_streams_active: server-side Dec-инкремент gauge-а происходит на
// своём recv-EOF и тоже отстаёт от момента, когда наш Close() вернул управление.
const drainSettleDelay = 3 * time.Second

// Options — параметры одного прогона soul-legion.
type Options struct {
	KeeperEventStream string        // host:port event_stream-листенера (mTLS), напр. 127.0.0.1:9443
	ServerName        string        // SNI/верификация server-cert-а (dev: localhost)
	MetricsURL        string        // http://127.0.0.1:9090 (опц.; "" → метрики не скрейпятся)
	CABundle          []byte        // root CA Keeper-server-cert-а
	Identities        []Identity    // подготовленные setup-фазой N идентичностей
	RampStep          int           // сколько стримов открывать за ступень (0 → все сразу)
	RampInterval      time.Duration // пауза между ступенями
	OpenConcurrency   int           // параллелизм dial-а внутри ступени (0 → = RampStep|len)
	Duration          time.Duration // сколько держать стримы после полного ramp-а
	Soulprint         bool          // слать ли SoulprintReport после Hello
}

// Report — итог прогона.
type Report struct {
	Target         int
	Achieved       int           // реально подтверждённых (HelloReply) стримов
	Failed         int           // ошибок Open
	ConnectP50     time.Duration // латентность Open (Hello→HelloReply включительно)
	ConnectP99     time.Duration
	ConnectMax     time.Duration
	HeldDuration   time.Duration // фактически удержано после ramp-а
	StillConnected int           // живых стримов на конец Duration
	TotalApplies   int           // ApplyRequest, обработанных всеми стабами
	RecvErrors     int           // стабов с НЕштатным обрывом recv-loop (Keeper сбросил стрим)
	FirstRecvErr   string        // текст первого такого обрыва
	MetricsAtPeak  MetricsSnapshot
	FirstErr       string
}

// Run открывает Identities-стримы ступенчато (RampStep / RampInterval), держит
// Duration, снимает метрики на пике и возвращает Report. Caller обязан вызвать
// Close на возвращённых стабах — Run отдаёт их через returned slice для teardown.
func Run(ctx context.Context, opts Options) (*Report, []*Stub, error) {
	total := len(opts.Identities)
	if total == 0 {
		return nil, nil, fmt.Errorf("legion: пустой набор идентичностей")
	}
	rep := &Report{Target: total}

	stubs := make([]*Stub, 0, total)
	var stubsMu sync.Mutex

	latencies := make([]time.Duration, 0, total)
	var latMu sync.Mutex
	var failed int64
	var firstErr atomic.Value // string

	step := opts.RampStep
	if step <= 0 {
		step = total
	}
	conc := opts.OpenConcurrency
	if conc <= 0 {
		conc = step
	}

	openOne := func(id Identity) {
		stub, err := NewStub(id, opts.KeeperEventStream, opts.ServerName, opts.CABundle)
		if err != nil {
			atomic.AddInt64(&failed, 1)
			firstErr.CompareAndSwap(nil, err.Error())
			return
		}
		start := time.Now()
		openCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = stub.Open(openCtx)
		cancel()
		if err != nil {
			atomic.AddInt64(&failed, 1)
			firstErr.CompareAndSwap(nil, err.Error())
			stub.Close()
			return
		}
		lat := time.Since(start)
		latMu.Lock()
		latencies = append(latencies, lat)
		latMu.Unlock()

		if opts.Soulprint {
			_ = stub.SendSoulprint()
		}
		stubsMu.Lock()
		stubs = append(stubs, stub)
		stubsMu.Unlock()
	}

	// Ramp по ступеням; внутри ступени — пул из conc воркеров.
	for offset := 0; offset < total; offset += step {
		end := min(offset+step, total)
		batch := opts.Identities[offset:end]

		var wg sync.WaitGroup
		sem := make(chan struct{}, conc)
		for _, id := range batch {
			wg.Add(1)
			sem <- struct{}{}
			go func(id Identity) {
				defer wg.Done()
				defer func() { <-sem }()
				openOne(id)
			}(id)
		}
		wg.Wait()

		if ctx.Err() != nil {
			break
		}
		if end < total && opts.RampInterval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(opts.RampInterval):
			}
		}
	}

	stubsMu.Lock()
	rep.Achieved = len(stubs)
	stubsMu.Unlock()
	rep.Failed = int(atomic.LoadInt64(&failed))
	if v := firstErr.Load(); v != nil {
		rep.FirstErr = v.(string)
	}

	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		rep.ConnectP50 = percentile(latencies, 50)
		rep.ConnectP99 = percentile(latencies, 99)
		rep.ConnectMax = latencies[len(latencies)-1]
	}

	// Метрики на пике. Короткий settle перед скрейпом: keeper_grpc_streams_active
	// инкрементится server-side при HelloReply, но gauge устаканивается с лагом
	// относительно момента, когда Open вернул управление (последняя порция стримов
	// ещё в полёте Inc-а). Без паузы снимок недосчитывает ~10% на больших N.
	if opts.MetricsURL != "" && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(metricsSettleDelay):
		}
		mctx, mcancel := context.WithTimeout(ctx, 5*time.Second)
		if snap, err := ScrapeMetrics(mctx, opts.MetricsURL); err == nil {
			rep.MetricsAtPeak = snap
		}
		mcancel()
	}

	// Удержание.
	if opts.Duration > 0 && ctx.Err() == nil {
		holdStart := time.Now()
		select {
		case <-ctx.Done():
		case <-time.After(opts.Duration):
		}
		rep.HeldDuration = time.Since(holdStart)
	}

	stubsMu.Lock()
	for _, st := range stubs {
		if st.Connected() {
			rep.StillConnected++
		}
		rep.TotalApplies += st.Applies()
		if re := st.RecvErr(); re != "" {
			rep.RecvErrors++
			if rep.FirstRecvErr == "" {
				rep.FirstRecvErr = re
			}
		}
	}
	out := append([]*Stub(nil), stubs...)
	stubsMu.Unlock()

	return rep, out, nil
}

// DrainScrape снимает keeper_grpc_streams_active ПОСЛЕ teardown стримов (caller
// уже закрыл все стабы) с коротким settle-ом. Доказывает, что gauge вернулся к
// baseline-у, — то есть decrement сработал на каждый Close и стримы не утекли.
// Возвращает снимок с Found=false, если metricsURL пуст или /metrics недоступен.
func DrainScrape(ctx context.Context, metricsURL string) MetricsSnapshot {
	if metricsURL == "" {
		return MetricsSnapshot{}
	}
	select {
	case <-ctx.Done():
	case <-time.After(drainSettleDelay):
	}
	mctx, mcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer mcancel()
	snap, _ := ScrapeMetrics(mctx, metricsURL)
	return snap
}

// percentile возвращает p-й перцентиль отсортированного среза (nearest-rank).
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if idx <= 0 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}
