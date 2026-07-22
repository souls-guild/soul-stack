package legion

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// metricsSettleDelay -- pause before scraping peak metrics so that
// keeper_grpc_streams_active has time to reflect the full ramp (see usage).
const metricsSettleDelay = 2 * time.Second

// drainSettleDelay -- pause after stream teardown before the final scrape of
// keeper_grpc_streams_active: the server-side Dec-increment of the gauge
// happens on its own recv-EOF and also lags behind the moment our Close()
// returns control.
const drainSettleDelay = 3 * time.Second

// Options -- parameters for one soul-legion run.
type Options struct {
	KeeperEventStream string        // host:port of the event_stream listener (mTLS), e.g. 127.0.0.1:9443
	ServerName        string        // SNI/server-cert verification (dev: localhost)
	MetricsURL        string        // http://127.0.0.1:9090 (optional; "" -> metrics not scraped)
	CABundle          []byte        // root CA of the Keeper server cert
	Identities        []Identity    // N identities prepared by the setup phase
	RampStep          int           // how many streams to open per step (0 -> all at once)
	RampInterval      time.Duration // pause between steps
	OpenConcurrency   int           // dial concurrency within a step (0 -> = RampStep|len)
	Duration          time.Duration // how long to hold streams after full ramp
	Soulprint         bool          // whether to send SoulprintReport after Hello
}

// Report -- run outcome.
type Report struct {
	Target         int
	Achieved       int           // actually confirmed (HelloReply) streams
	Failed         int           // Open errors
	ConnectP50     time.Duration // Open latency (Hello->HelloReply inclusive)
	ConnectP99     time.Duration
	ConnectMax     time.Duration
	HeldDuration   time.Duration // actually held after ramp
	StillConnected int           // live streams at the end of Duration
	TotalApplies   int           // ApplyRequests handled by all stubs
	RecvErrors     int           // stubs with an abnormal recv-loop break (Keeper dropped the stream)
	FirstRecvErr   string        // text of the first such break
	MetricsAtPeak  MetricsSnapshot
	FirstErr       string
}

// Run opens the Identities streams in steps (RampStep / RampInterval), holds
// for Duration, takes a metrics snapshot at the peak, and returns a Report.
// Caller must call Close on the returned stubs -- Run hands them back via the
// returned slice for teardown.
func Run(ctx context.Context, opts Options) (*Report, []*Stub, error) {
	total := len(opts.Identities)
	if total == 0 {
		return nil, nil, fmt.Errorf("legion: empty identity set")
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

	// Ramp by steps; within a step -- a pool of conc workers.
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

	// Metrics at the peak. A short settle before scraping:
	// keeper_grpc_streams_active is incremented server-side on HelloReply,
	// but the gauge settles with a lag relative to the moment Open returned
	// control (the last batch of streams is still mid-Inc). Without the
	// pause the snapshot undercounts ~10% at large N.
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

	// Hold.
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

// DrainScrape scrapes keeper_grpc_streams_active AFTER stream teardown
// (caller has already closed all stubs) with a short settle. Proves the
// gauge returned to baseline -- i.e. decrement fired on every Close and
// streams did not leak. Returns a snapshot with Found=false if metricsURL is
// empty or /metrics is unreachable.
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

// percentile returns the p-th percentile of a sorted slice (nearest-rank).
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
