// Command soul-legion -- load stub generator (Phase 0, docs/testing/load-testing.md).
//
// Test-only artifact (NOT a shipped binary, ADR-004): brings up N concurrent
// fake-Soul streams (gRPC bidi over mTLS EventStream) to a live Keeper,
// measures achieved-N / connect latency / Keeper resource usage, and
// compares against the scaling.md projection table.
//
// Example (dev stand):
//
//	soul-legion \
//	  --keeper-endpoint=127.0.0.1:9443 --metrics=http://127.0.0.1:9090 \
//	  --pg=postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable \
//	  --vault=http://127.0.0.1:8200 --vault-token=root \
//	  --ca=/tmp/keeper-dev/tls/vault-ca.crt \
//	  --count=1000 --ramp=200 --ramp-interval=500ms --duration=30s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/souls-guild/soul-stack/tests/load/legion"
)

func main() {
	var (
		keeperEP   = flag.String("keeper-endpoint", "127.0.0.1:9443", "Keeper event_stream host:port (mTLS)")
		serverName = flag.String("server-name", "localhost", "SNI/server-cert verification (dev: localhost)")
		metricsURL = flag.String("metrics", "http://127.0.0.1:9090", "Keeper /metrics base URL (empty -> no scraping)")
		pgDSN      = flag.String("pg", "postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable", "PG DSN of the Keeper cluster for the setup phase")
		vaultAddr  = flag.String("vault", "http://127.0.0.1:8200", "Vault addr (dev-PKI)")
		vaultToken = flag.String("vault-token", "root", "Vault token")
		pkiMount   = flag.String("pki-mount", "pki", "Vault PKI mount")
		pkiRole    = flag.String("pki-role", "soul-seed", "Vault PKI role")
		caPath     = flag.String("ca", "/tmp/keeper-dev/tls/vault-ca.crt", "root CA of the Keeper server cert (PEM)")
		count      = flag.Int("count", 100, "number of fake-Soul streams N")
		domain     = flag.String("domain", "example.com", "SID domain: legion-<NNNNN>.<domain>")
		sidPrefix  = flag.String("sid-prefix", "legion-", "SID prefix (isolation from the real fleet + cleanup)")
		coven      = flag.String("coven", "legion", "legion coven label (souls.coven; Voyage/API-preview target); empty -> no coven")
		ramp       = flag.Int("ramp", 0, "streams per step (0 -> all at once)")
		rampIvl    = flag.Duration("ramp-interval", 500*time.Millisecond, "pause between steps")
		openConc   = flag.Int("open-concurrency", 100, "dial concurrency within a step")
		duration   = flag.Duration("duration", 30*time.Second, "how long to hold streams after full ramp")
		soulprint  = flag.Bool("soulprint", true, "send SoulprintReport after Hello")
		register   = flag.Bool("register", true, "pre-register SID in souls/soul_seeds (needed: Keeper authorizes by seed-fingerprint)")
		cleanup    = flag.Bool("cleanup", true, "delete the legion from the registry by sid-prefix on exit")
		issueConc  = flag.Int("issue-concurrency", 32, "Vault-issue concurrency in the setup phase")

		// -- axis B (API load) + axis C (a single Voyage), on top of a live legion --
		openAPI    = flag.String("openapi", "http://127.0.0.1:8080", "OpenAPI listener base URL (/v1 handlers)")
		jwt        = flag.String("jwt", "", "admin-Archon-JWT for /v1 (Bearer); empty -> axis B/C skipped (operator: TOKEN=$(make dev-jwt))")
		apiLoad    = flag.Bool("api", false, "axis B: round-robin run over all safe /v1 handlers on top of the legion")
		apiConc    = flag.Int("api-concurrency", 16, "number of parallel API workers (axis B)")
		apiDur     = flag.Duration("api-duration", 15*time.Second, "duration of the API run (axis B)")
		voyageRun  = flag.Bool("voyage", false, "axis C: run ONE command Voyage against the legion's coven and measure end-to-end")
		voyageMod  = flag.String("voyage-module", "core.cmd.shell", "command module for the Voyage (axis C)")
		voyageConc = flag.Int("voyage-concurrency", 0, "top-level voyage.concurrency: >0 -> included in the create body (dispatch parallelism); 0 -> do NOT send (keeper default=1, sequential)")
		voyagePoll = flag.Duration("voyage-poll-timeout", 120*time.Second, "budget for waiting for the Voyage terminal status (axis C)")

		// -- axis write (write+audit path): create->delete cycles of safe entities --
		writeLoad = flag.Bool("write", false, "axis write: create->delete cycles of safe entities (synod/role/push-provider/herald), write+audit path profile")
		writeConc = flag.Int("write-concurrency", 8, "number of parallel write workers (write is heavier than read -> fewer workers)")
		writeDur  = flag.Duration("write-duration", 0, "duration of the write run (0 -> takes --api-duration)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, runParams{
		keeperEP:   *keeperEP,
		serverName: *serverName,
		metricsURL: *metricsURL,
		pgDSN:      *pgDSN,
		vaultAddr:  *vaultAddr,
		vaultToken: *vaultToken,
		pkiMount:   *pkiMount,
		pkiRole:    *pkiRole,
		caPath:     *caPath,
		count:      *count,
		domain:     *domain,
		sidPrefix:  *sidPrefix,
		coven:      *coven,
		ramp:       *ramp,
		rampIvl:    *rampIvl,
		openConc:   *openConc,
		duration:   *duration,
		soulprint:  *soulprint,
		register:   *register,
		cleanup:    *cleanup,
		issueConc:  *issueConc,
		openAPI:    *openAPI,
		jwt:        *jwt,
		apiLoad:    *apiLoad,
		apiConc:    *apiConc,
		apiDur:     *apiDur,
		voyageRun:  *voyageRun,
		voyageMod:  *voyageMod,
		voyageConc: *voyageConc,
		voyagePoll: *voyagePoll,
		writeLoad:  *writeLoad,
		writeConc:  *writeConc,
		// --write-duration default (0) inherits --api-duration: write and
		// API axes share a single run duration unless the operator splits
		// them explicitly.
		writeDur: pickDuration(*writeDur, *apiDur),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "soul-legion: %v\n", err)
		os.Exit(1)
	}
}

type runParams struct {
	keeperEP, serverName, metricsURL string
	pgDSN, vaultAddr, vaultToken     string
	pkiMount, pkiRole, caPath        string
	count                            int
	domain, sidPrefix, coven         string
	ramp, openConc, issueConc        int
	rampIvl, duration                time.Duration
	soulprint, register, cleanup     bool

	openAPI, jwt, voyageMod string
	apiLoad, voyageRun      bool
	apiConc                 int
	apiDur                  time.Duration
	voyageConc              int
	voyagePoll              time.Duration

	writeLoad bool
	writeConc int
	writeDur  time.Duration
}

// pickDuration returns primary if it's set (>0), otherwise fallback. Used for
// --write-duration: the 0 default inherits --api-duration (shared run
// duration).
func pickDuration(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func run(ctx context.Context, p runParams) error {
	caBundle, err := os.ReadFile(p.caPath)
	if err != nil {
		return fmt.Errorf("read CA %s: %w", p.caPath, err)
	}

	// -- setup phase: mint N certs from dev-PKI + pre-register in the DB --------
	fmt.Printf("[setup] minting %d leaf cert(s) from %s/%s/issue/%s ...\n", p.count, p.vaultAddr, p.pkiMount, p.pkiRole)
	pki := legion.NewVaultPKI(p.vaultAddr, p.vaultToken, p.pkiMount, p.pkiRole)
	setupStart := time.Now()
	ids, err := mintIdentities(ctx, pki, p)
	if err != nil {
		return err
	}
	fmt.Printf("[setup] issued %d certs in %s\n", len(ids), time.Since(setupStart).Round(time.Millisecond))

	// reg is needed both for setup registration and for cleanup of the
	// created Voyage (axis C): keep it at the run level, not inside the
	// register block.
	var reg *legion.Registrar
	if p.register {
		reg, err = legion.NewRegistrar(ctx, p.pgDSN)
		if err != nil {
			return fmt.Errorf("setup: registrar: %w", err)
		}
		defer reg.Close()

		regStart := time.Now()
		var covens []string
		if p.coven != "" {
			covens = []string{p.coven}
		}
		if err := reg.Register(ctx, p.sidPrefix, covens, ids); err != nil {
			return fmt.Errorf("setup: register: %w", err)
		}
		fmt.Printf("[setup] pre-registered %d SID(s) in souls/soul_seeds (coven=%q) in %s\n",
			len(ids), p.coven, time.Since(regStart).Round(time.Millisecond))

		if p.cleanup {
			defer func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer ccancel()
				n, cerr := reg.Cleanup(cctx, p.sidPrefix)
				if cerr != nil {
					fmt.Fprintf(os.Stderr, "[cleanup] error: %v\n", cerr)
					return
				}
				fmt.Printf("[cleanup] deleted %d souls row(s) by prefix %q\n", n, p.sidPrefix)
			}()
		}
	}

	// Axes B/C run on top of a LIVE legion: stream hold moves into main (Run
	// with Duration=0 returns live stubs right after ramp). API load and
	// Voyage run on top of them, then streams close. Without B/C -- the
	// previous behavior (hold inside Run per --duration).
	runDuration := p.duration
	loadPhase := p.apiLoad || p.voyageRun || p.writeLoad
	if loadPhase {
		runDuration = 0
		if p.jwt == "" {
			return fmt.Errorf("axes B/C/write require --jwt (admin-Archon token): TOKEN=$(make dev-jwt)")
		}
	}

	// Baseline metrics snapshot BEFORE bringing up streams.
	var before legion.MetricsSnapshot
	if p.metricsURL != "" {
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
		before, _ = legion.ScrapeMetrics(bctx, p.metricsURL)
		bcancel()
	}

	// -- connect phase -----------------------------------------------------
	fmt.Printf("[connect] opening %d streams to %s (ramp=%d/%s, concurrency=%d) ...\n",
		len(ids), p.keeperEP, p.ramp, p.rampIvl, p.openConc)
	rep, stubs, err := legion.Run(ctx, legion.Options{
		KeeperEventStream: p.keeperEP,
		ServerName:        p.serverName,
		MetricsURL:        p.metricsURL,
		CABundle:          caBundle,
		Identities:        ids,
		RampStep:          p.ramp,
		RampInterval:      p.rampIvl,
		OpenConcurrency:   p.openConc,
		Duration:          runDuration,
		Soulprint:         p.soulprint,
	})
	if err != nil {
		closeStubs(stubs)
		return fmt.Errorf("connect: %w", err)
	}

	printReport(rep, before)

	// -- axis B (API load) + axis C (a single Voyage), on top of a live legion --
	if loadPhase {
		runLoadPhases(ctx, p, reg, stubs)
	}

	// Drain measurement: close all streams and scrape
	// keeper_grpc_streams_active once more. If the gauge returned to
	// baseline -- decrement fired on every Close, streams did not leak
	// (honesty measurement: drain proven, not just claimed).
	closeStubs(stubs)
	drained := legion.DrainScrape(ctx, p.metricsURL)
	printDrain(drained, before)
	return nil
}

// runLoadPhases runs axis B (API load) and axis C (a single Voyage) on top of
// a live legion. The legion's coven is the target of both axes
// (preview/Voyage by coven). The Voyage is cleaned up from PG (DeleteVoyage)
// when p.cleanup is set, so the load run doesn't linger in the registry.
// Context cancellation (Ctrl-C) interrupts the run but not cleanup (it runs
// on a background context, like souls-cleanup).
func runLoadPhases(ctx context.Context, p runParams, reg *legion.Registrar, stubs []*legion.Stub) {
	if p.apiLoad {
		fmt.Printf("\n[api] axis B: round-robin run over all safe handlers (coven=%q, conc=%d, %s) ...\n",
			p.coven, p.apiConc, p.apiDur)
		arep, err := legion.RunAPILoad(ctx, legion.APILoadOptions{
			BaseURL:     p.openAPI,
			JWT:         p.jwt,
			Coven:       p.coven,
			Concurrency: p.apiConc,
			Duration:    p.apiDur,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[api] error: %v\n", err)
		} else {
			printAPIReport(arep)
		}
	}

	if p.writeLoad {
		fmt.Printf("\n[write] axis write: create->delete cycles of safe entities (conc=%d, %s) ...\n",
			p.writeConc, p.writeDur)
		wrep, err := legion.RunWriteLoad(ctx, legion.WriteLoadOptions{
			BaseURL:     p.openAPI,
			JWT:         p.jwt,
			Concurrency: p.writeConc,
			Duration:    p.writeDur,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[write] error: %v\n", err)
		} else {
			printWriteReport(wrep)
		}
	}

	if p.voyageRun {
		if p.coven == "" {
			fmt.Fprintln(os.Stderr, "[voyage] skipped: empty --coven (nothing to target)")
			return
		}
		fmt.Printf("\n[voyage] axis C: running ONE command Voyage against coven=%q (module=%s) ...\n", p.coven, p.voyageMod)
		vrep, err := legion.RunCommandVoyage(ctx, legion.VoyageRunOptions{
			BaseURL:     p.openAPI,
			JWT:         p.jwt,
			Coven:       p.coven,
			Module:      p.voyageMod,
			Input:       map[string]any{"cmd": "echo ok"},
			Concurrency: p.voyageConc,
			Timeout:     p.voyagePoll,
		})
		if vrep != nil {
			printVoyageReport(vrep, stubs)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[voyage] error: %v\n", err)
		}
		// Cleanup of the created Voyage (exact voyage_id, cascades targets).
		// errands are short-lived and settle until purge_old_errands; other
		// runs are untouched.
		if vrep != nil && vrep.VoyageID != "" && p.cleanup && reg != nil {
			cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
			if derr := reg.DeleteVoyage(cctx, vrep.VoyageID); derr != nil {
				fmt.Fprintf(os.Stderr, "[voyage] cleanup voyage %s: %v\n", vrep.VoyageID, derr)
			} else {
				fmt.Printf("[voyage] cleanup: deleted Voyage %s (+targets)\n", vrep.VoyageID)
			}
			ccancel()
		}
	}
}

func printAPIReport(r *legion.APILoadReport) {
	fmt.Println("════════════════════ axis B - API load ════════════════════")
	fmt.Printf("  duration           : %s (%d mounted handlers (+%d skipped by probe))\n",
		r.Wall.Round(time.Millisecond), len(r.Endpoints), len(r.Skipped))
	if len(r.Skipped) > 0 {
		// Skipped by the probe (404, not mounted) -- one short line, does
		// not go into the per-endpoint table (nothing to measure).
		fmt.Printf("  skipped (404)      : %s\n", strings.Join(r.Skipped, ", "))
	}
	// Sort by descending throughput -- hottest handlers on top.
	eps := make([]legion.EndpointStat, len(r.Endpoints))
	copy(eps, r.Endpoints)
	sort.Slice(eps, func(i, j int) bool { return eps[i].Throughput > eps[j].Throughput })
	for _, s := range eps {
		printEndpointStat(s)
	}
	if r.FirstErr != "" {
		fmt.Printf("  WARNING first error: %s\n", r.FirstErr)
	}
	fmt.Println("═══════════════════════════════════════════════════════════════")
}

// printWriteReport prints the per-kind report of the write axis: two lines
// per entity (POST <kind> and DELETE <kind>) with req/err/p50/p99/max/thr,
// same as axis B. The final line -- sweep (how many residual legionload-*
// were picked up by the safety net; normally 0 -- per-iteration delete
// cleaned everything).
func printWriteReport(r *legion.WriteLoadReport) {
	fmt.Println("════════════════════ axis write - create->delete ════════════════════")
	fmt.Printf("  duration           : %s (%d entities x 2 operations)\n",
		r.Wall.Round(time.Millisecond), len(r.Entities))
	for _, e := range r.Entities {
		printEndpointStat(e.Create)
		printEndpointStat(e.Delete)
	}
	fmt.Printf("  residual sweep     : %d (leak safety net; normally 0 -- per-iteration delete cleaned everything)\n", r.Swept)
	if r.FirstErr != "" {
		fmt.Printf("  WARNING first error: %s\n", r.FirstErr)
	}
	fmt.Println("════════════════════════════════════════════════════════════════════")
}

func printEndpointStat(s legion.EndpointStat) {
	fmt.Printf("  %-34s req=%d err=%d  p50/p99/max=%s/%s/%s  thr=%.1f req/s\n",
		s.Name, s.Requests, s.Errors,
		s.P50.Round(time.Microsecond), s.P99.Round(time.Microsecond), s.Max.Round(time.Microsecond),
		s.Throughput)
}

func printVoyageReport(r *legion.VoyageRunReport, stubs []*legion.Stub) {
	var totalErrands int
	for _, st := range stubs {
		totalErrands += st.Errands()
	}
	fmt.Println("════════════════════ axis C - one Voyage ════════════════════")
	fmt.Printf("  voyage_id          : %s\n", r.VoyageID)
	fmt.Printf("  kind               : command (batch unit = host)\n")
	fmt.Printf("  scope_size         : %d (hosts resolved by coven)\n", r.ScopeSize)
	fmt.Printf("  create latency     : %s (POST accept+resolve+persist)\n", r.CreateLat.Round(time.Millisecond))
	fmt.Printf("  end-to-end         : %s (POST -> terminal; dispatch->ErrandResult->commit->audit)\n", r.EndToEnd.Round(time.Millisecond))
	fmt.Printf("  final status       : %s (succeeded=%d failed=%d)\n", r.FinalStatus, r.Succeeded, r.Failed)
	fmt.Printf("  poll iterations    : %d\n", r.Polls)
	fmt.Printf("  ErrandRequest(s) answered by stubs: %d\n", totalErrands)
	fmt.Println("  -- audit/claim (measure manually, docs/testing/load-testing.md §4.2) --")
	fmt.Println("    audit-INSERT-rate : docker exec soul-stack-postgres psql -U keeper -d keeper -c \\")
	fmt.Println("                        \"SELECT count(*) FROM audit_log WHERE created_at > NOW() - INTERVAL '2 min'\"")
	fmt.Println("    apply_runs/errands: psql -c \"SELECT count(*) FROM errands WHERE started_at > NOW() - INTERVAL '2 min'\"")
	fmt.Println("══════════════════════════════════════════════════════════════")
}

// mintIdentities issues count certs in parallel. Vault dev-PKI can handle
// dozens of parallel issues; we cap issueConc to avoid saturating Vault
// during the setup phase (it's not the thing under measurement -- we measure
// Keeper).
func mintIdentities(ctx context.Context, pki *legion.VaultPKI, p runParams) ([]legion.Identity, error) {
	ids := make([]legion.Identity, p.count)
	errs := make([]error, p.count)
	sem := make(chan struct{}, p.issueConc)
	var wg sync.WaitGroup
	for i := range p.count {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			sid := fmt.Sprintf("%s%05d.%s", p.sidPrefix, i, p.domain)
			id, err := pki.Issue(ctx, sid, "24h")
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	out := make([]legion.Identity, 0, p.count)
	var firstErr error
	for i := range p.count {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		out = append(out, ids[i])
	}
	if len(out) == 0 && firstErr != nil {
		return nil, fmt.Errorf("setup: no cert was issued: %w", firstErr)
	}
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: %d issue error(s) (first: %v)\n", p.count-len(out), firstErr)
	}
	return out, nil
}

func closeStubs(stubs []*legion.Stub) {
	var wg sync.WaitGroup
	for _, st := range stubs {
		wg.Add(1)
		go func(st *legion.Stub) {
			defer wg.Done()
			st.Close()
		}(st)
	}
	wg.Wait()
}

func printReport(rep *legion.Report, before legion.MetricsSnapshot) {
	fmt.Println()
	fmt.Println("════════════════════ soul-legion report ════════════════════")
	fmt.Printf("  target N           : %d\n", rep.Target)
	fmt.Printf("  achieved (HelloAck): %d\n", rep.Achieved)
	fmt.Printf("  failed Open        : %d\n", rep.Failed)
	if rep.FirstErr != "" {
		fmt.Printf("  first error        : %s\n", rep.FirstErr)
	}
	fmt.Printf("  connect p50/p99/max: %s / %s / %s\n",
		rep.ConnectP50.Round(time.Microsecond),
		rep.ConnectP99.Round(time.Microsecond),
		rep.ConnectMax.Round(time.Microsecond))
	fmt.Printf("  held duration      : %s\n", rep.HeldDuration.Round(time.Millisecond))
	fmt.Printf("  still connected    : %d / %d\n", rep.StillConnected, rep.Achieved)
	fmt.Printf("  apply requests     : %d\n", rep.TotalApplies)
	if rep.RecvErrors > 0 {
		fmt.Printf("  recv-loop drops    : %d (abnormal, Keeper dropped the stream; first: %s)\n",
			rep.RecvErrors, rep.FirstRecvErr)
	}

	m := rep.MetricsAtPeak
	if m.Found {
		fmt.Println("  -- Keeper /metrics at peak --")
		fmt.Printf("    keeper_grpc_streams_active : %.0f (was %.0f, delta %+.0f)\n",
			m.StreamsActive, before.StreamsActive, m.StreamsActive-before.StreamsActive)
		fmt.Printf("    go_goroutines              : %.0f (was %.0f, delta %+.0f)\n",
			m.GoGoroutines, before.GoGoroutines, m.GoGoroutines-before.GoGoroutines)
		fmt.Printf("    process_resident_memory    : %.1f MiB (was %.1f MiB)\n",
			m.ResidentBytes/1024/1024, before.ResidentBytes/1024/1024)
		fmt.Printf("    go_heap_inuse              : %.1f MiB (was %.1f MiB)\n",
			m.HeapInUseBytes/1024/1024, before.HeapInUseBytes/1024/1024)
		printScalingCompare(m, rep.Achieved)
	} else {
		fmt.Println("  (metrics not scraped -- --metrics empty or /metrics unreachable)")
	}

	fmt.Println("  -- external gaps (measure manually, docs/testing/load-testing.md §4.2) --")
	fmt.Println("    Redis SID-lease     : redis-cli -p 6381 INFO commandstats | grep -E 'cmdstat_(set|exists|get)'")
	fmt.Println("    PG pool/claim       : docker exec soul-stack-postgres psql -U keeper -d keeper -c \\")
	fmt.Println("                          \"SELECT count(*) FROM pg_stat_activity WHERE datname='keeper'\"")
	fmt.Println("    Keeper RAM/threads  : ps -o rss,nlwp -p <keeper-pid>  (RSS in KiB / thread count)")
	fmt.Println("    Conclave live-count : redis-cli -p 6381 KEYS 'keeper:instance:*'")
	fmt.Println("═════════════════════════════════════════════════════════════")
}

// printDrain prints the result of the final scrape after stream teardown:
// keeper_grpc_streams_active should return to baseline (decrement fired on
// every Close, streams did not leak). A delta != 0 relative to baseline is a
// leak signal.
func printDrain(drained, before legion.MetricsSnapshot) {
	if !drained.Found {
		fmt.Println("  drained streams_active: (metrics unreachable -- drain not verified)")
		return
	}
	delta := drained.StreamsActive - before.StreamsActive
	verdict := "OK (returned to baseline)"
	if delta > 0.5 {
		verdict = "WARNING: streams_active did NOT return to baseline -- possible stream leak (Dec did not fire on Close)"
	}
	fmt.Printf("  drained streams_active : %.0f (baseline %.0f, delta %+.0f) -- %s\n",
		drained.StreamsActive, before.StreamsActive, delta, verdict)
}

// printScalingCompare compares observed RAM/goroutine usage against the
// scaling.md projection row (Keeper 8 GB / 8 vCPU per 100k VM). The linear
// "per soul" extrapolation is coarse, for order-of-magnitude sanity
// validation, not precise sizing (Phase 0 goal).
func printScalingCompare(m legion.MetricsSnapshot, achieved int) {
	if achieved <= 0 {
		return
	}
	const scalingKeeperRAMBytes = 8.0 * 1024 * 1024 * 1024 // 8 GB per instance
	const scalingTargetVM = 100_000.0                      // target scale

	ramPerSoul := m.ResidentBytes / float64(achieved)
	goroutPerSoul := m.GoGoroutines / float64(achieved)
	projRAM100k := ramPerSoul * scalingTargetVM
	fmt.Println("  -- comparison with scaling.md (Keeper 8 GB/8 vCPU per 100k VM) --")
	fmt.Printf("    RSS per soul       : ~%.2f MiB\n", ramPerSoul/1024/1024)
	fmt.Printf("    goroutines per soul: ~%.2f\n", goroutPerSoul)
	fmt.Printf("    extrap. RSS@100k   : ~%.1f GiB (scaling.md budget: %.0f GiB/instance x 3+)\n",
		projRAM100k/1024/1024/1024, scalingKeeperRAMBytes/1024/1024/1024)
	if projRAM100k > scalingKeeperRAMBytes*3 {
		fmt.Println("    WARNING: linear RSS@100k extrapolation exceeds the 3x8 GB budget -- worth a Phase 1/2 measurement")
	}
}
