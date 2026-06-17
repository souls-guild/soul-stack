// Command soul-legion — нагрузочный stub-генератор (Ф0, docs/testing/load-testing.md).
//
// Test-only артефакт (НЕ поставочный бинарь, ADR-004): поднимает N одновременных
// fake-Soul-стримов (gRPC bidi поверх mTLS EventStream) к живому Keeper-у, мерит
// achieved-N / connect-латентность / ресурсы Keeper-а и сверяет с расчётной
// таблицей scaling.md.
//
// Пример (dev-стенд):
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
		serverName = flag.String("server-name", "localhost", "SNI/верификация server-cert-а (dev: localhost)")
		metricsURL = flag.String("metrics", "http://127.0.0.1:9090", "Keeper /metrics base URL (пусто → не скрейпить)")
		pgDSN      = flag.String("pg", "postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable", "PG DSN Keeper-кластера для setup-фазы")
		vaultAddr  = flag.String("vault", "http://127.0.0.1:8200", "Vault addr (dev-PKI)")
		vaultToken = flag.String("vault-token", "root", "Vault token")
		pkiMount   = flag.String("pki-mount", "pki", "Vault PKI mount")
		pkiRole    = flag.String("pki-role", "soul-seed", "Vault PKI role")
		caPath     = flag.String("ca", "/tmp/keeper-dev/tls/vault-ca.crt", "root CA Keeper-server-cert-а (PEM)")
		count      = flag.Int("count", 100, "число fake-Soul-стримов N")
		domain     = flag.String("domain", "example.com", "домен SID-ов: legion-<NNNNN>.<domain>")
		sidPrefix  = flag.String("sid-prefix", "legion-", "prefix SID-ов (изоляция от реального флота + cleanup)")
		coven      = flag.String("coven", "legion", "coven-метка легиона (souls.coven; таргет Voyage/API-preview); пусто → без coven")
		ramp       = flag.Int("ramp", 0, "стримов за ступень (0 → все сразу)")
		rampIvl    = flag.Duration("ramp-interval", 500*time.Millisecond, "пауза между ступенями")
		openConc   = flag.Int("open-concurrency", 100, "параллелизм dial-а внутри ступени")
		duration   = flag.Duration("duration", 30*time.Second, "сколько держать стримы после полного ramp-а")
		soulprint  = flag.Bool("soulprint", true, "слать SoulprintReport после Hello")
		register   = flag.Bool("register", true, "предрегистрировать SID в souls/soul_seeds (нужно: Keeper авторизует по seed-fingerprint)")
		cleanup    = flag.Bool("cleanup", true, "удалить легион из реестра по sid-prefix на выходе")
		issueConc  = flag.Int("issue-concurrency", 32, "параллелизм Vault-issue в setup-фазе")

		// ── ось B (API-нагрузка) + ось C (один Voyage), поверх живого легиона ──
		openAPI    = flag.String("openapi", "http://127.0.0.1:8080", "OpenAPI-listener base URL (/v1-ручки)")
		jwt        = flag.String("jwt", "", "admin-Archon-JWT для /v1 (Bearer); пусто → ось B/C пропущены (оператор: TOKEN=$(make dev-jwt))")
		apiLoad    = flag.Bool("api", false, "ось B: round-robin-гон всех безопасных /v1-ручек поверх легиона")
		apiConc    = flag.Int("api-concurrency", 16, "число параллельных API-воркеров (ось B)")
		apiDur     = flag.Duration("api-duration", 15*time.Second, "длительность API-гона (ось B)")
		voyageRun  = flag.Bool("voyage", false, "ось C: запустить ОДИН command-Voyage по coven легиона и замерить end-to-end")
		voyageMod  = flag.String("voyage-module", "core.cmd.shell", "command-модуль Voyage (ось C)")
		voyageConc = flag.Int("voyage-concurrency", 0, "top-level voyage.concurrency: >0 → кладётся в тело create (параллелизм диспетча); 0 → НЕ слать (keeper-дефолт=1, последовательно)")
		voyagePoll = flag.Duration("voyage-poll-timeout", 120*time.Second, "бюджет ожидания терминального статуса Voyage (ось C)")

		// ── ось write (write+audit-путь): create→delete циклы безопасных сущностей ──
		writeLoad = flag.Bool("write", false, "ось write: create→delete циклы безопасных сущностей (synod/role/push-provider/herald), профиль write+audit-пути")
		writeConc = flag.Int("write-concurrency", 8, "число параллельных write-воркеров (write тяжелее read → меньше воркеров)")
		writeDur  = flag.Duration("write-duration", 0, "длительность write-гона (0 → берётся --api-duration)")
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
		// --write-duration по умолчанию (0) наследует --api-duration: write- и
		// API-оси задают одну длительность гона, если оператор не разделил их явно.
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

// pickDuration возвращает primary, если он задан (>0), иначе fallback. Нужен для
// --write-duration: дефолт 0 наследует --api-duration (общая длительность гона).
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

	// ── setup-фаза: mint N сертов из dev-PKI + предрегистрация в БД ──────────
	fmt.Printf("[setup] минтим %d leaf-cert(ов) из %s/%s/issue/%s ...\n", p.count, p.vaultAddr, p.pkiMount, p.pkiRole)
	pki := legion.NewVaultPKI(p.vaultAddr, p.vaultToken, p.pkiMount, p.pkiRole)
	setupStart := time.Now()
	ids, err := mintIdentities(ctx, pki, p)
	if err != nil {
		return err
	}
	fmt.Printf("[setup] выпущено %d сертов за %s\n", len(ids), time.Since(setupStart).Round(time.Millisecond))

	// reg нужен и для setup-регистрации, и для cleanup созданного Voyage (ось C):
	// держим его на уровне run, а не внутри register-блока.
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
		fmt.Printf("[setup] предрегистрировано %d SID в souls/soul_seeds (coven=%q) за %s\n",
			len(ids), p.coven, time.Since(regStart).Round(time.Millisecond))

		if p.cleanup {
			defer func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer ccancel()
				n, cerr := reg.Cleanup(cctx, p.sidPrefix)
				if cerr != nil {
					fmt.Fprintf(os.Stderr, "[cleanup] ошибка: %v\n", cerr)
					return
				}
				fmt.Printf("[cleanup] удалено %d souls-строк по prefix %q\n", n, p.sidPrefix)
			}()
		}
	}

	// Оси B/C гонятся поверх ЖИВОГО легиона: hold стримов переносится в main
	// (Run с Duration=0 возвращает живые стабы сразу после ramp). API-нагрузка и
	// Voyage идут поверх них, затем стримы закрываются. Без B/C — прежнее
	// поведение (hold внутри Run по --duration).
	runDuration := p.duration
	loadPhase := p.apiLoad || p.voyageRun || p.writeLoad
	if loadPhase {
		runDuration = 0
		if p.jwt == "" {
			return fmt.Errorf("оси B/C/write требуют --jwt (admin-Archon-токен): TOKEN=$(make dev-jwt)")
		}
	}

	// Базовый срез метрик ДО подъёма стримов.
	var before legion.MetricsSnapshot
	if p.metricsURL != "" {
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
		before, _ = legion.ScrapeMetrics(bctx, p.metricsURL)
		bcancel()
	}

	// ── connect-фаза ────────────────────────────────────────────────────────
	fmt.Printf("[connect] открываем %d стримов к %s (ramp=%d/%s, concurrency=%d) ...\n",
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

	// ── ось B (API-нагрузка) + ось C (один Voyage), поверх живого легиона ──────
	if loadPhase {
		runLoadPhases(ctx, p, reg, stubs)
	}

	// Drain-замер: закрываем все стримы и снимаем keeper_grpc_streams_active
	// ещё раз. Если gauge вернулся к baseline-у — decrement сработал на каждый
	// Close, стримы не утекли (honesty-замер: дрейн доказан, не заявлен).
	closeStubs(stubs)
	drained := legion.DrainScrape(ctx, p.metricsURL)
	printDrain(drained, before)
	return nil
}

// runLoadPhases гонит ось B (API-нагрузка) и ось C (один Voyage) поверх живого
// легиона. coven легиона — таргет обеих осей (preview/Voyage по coven). Voyage
// чистится из PG (DeleteVoyage) при p.cleanup, чтобы нагрузочный прогон не оседал
// в реестре. Контекст-отмена (Ctrl-C) прерывает гон, но не cleanup (он на
// background-context, как souls-cleanup).
func runLoadPhases(ctx context.Context, p runParams, reg *legion.Registrar, stubs []*legion.Stub) {
	if p.apiLoad {
		fmt.Printf("\n[api] ось B: round-robin-гон всех безопасных ручек (coven=%q, conc=%d, %s) ...\n",
			p.coven, p.apiConc, p.apiDur)
		arep, err := legion.RunAPILoad(ctx, legion.APILoadOptions{
			BaseURL:     p.openAPI,
			JWT:         p.jwt,
			Coven:       p.coven,
			Concurrency: p.apiConc,
			Duration:    p.apiDur,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[api] ошибка: %v\n", err)
		} else {
			printAPIReport(arep)
		}
	}

	if p.writeLoad {
		fmt.Printf("\n[write] ось write: create→delete циклы безопасных сущностей (conc=%d, %s) ...\n",
			p.writeConc, p.writeDur)
		wrep, err := legion.RunWriteLoad(ctx, legion.WriteLoadOptions{
			BaseURL:     p.openAPI,
			JWT:         p.jwt,
			Concurrency: p.writeConc,
			Duration:    p.writeDur,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[write] ошибка: %v\n", err)
		} else {
			printWriteReport(wrep)
		}
	}

	if p.voyageRun {
		if p.coven == "" {
			fmt.Fprintln(os.Stderr, "[voyage] пропущен: пустой --coven (нечего таргетить)")
			return
		}
		fmt.Printf("\n[voyage] ось C: запуск ОДНОГО command-Voyage по coven=%q (module=%s) ...\n", p.coven, p.voyageMod)
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
			fmt.Fprintf(os.Stderr, "[voyage] ошибка: %v\n", err)
		}
		// Cleanup созданного Voyage (точный voyage_id, каскад targets). errands —
		// короткоживущие, оседают до purge_old_errands; чужие прогоны не задеты.
		if vrep != nil && vrep.VoyageID != "" && p.cleanup && reg != nil {
			cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
			if derr := reg.DeleteVoyage(cctx, vrep.VoyageID); derr != nil {
				fmt.Fprintf(os.Stderr, "[voyage] cleanup voyage %s: %v\n", vrep.VoyageID, derr)
			} else {
				fmt.Printf("[voyage] cleanup: удалён Voyage %s (+targets)\n", vrep.VoyageID)
			}
			ccancel()
		}
	}
}

func printAPIReport(r *legion.APILoadReport) {
	fmt.Println("════════════════════ ось B — API-нагрузка ════════════════════")
	fmt.Printf("  длительность       : %s (%d смонтированных ручек (+%d пропущено probe-ом))\n",
		r.Wall.Round(time.Millisecond), len(r.Endpoints), len(r.Skipped))
	if len(r.Skipped) > 0 {
		// Пропущенные probe-ом (404, не смонтированы) — одной краткой строкой, в
		// per-endpoint таблицу не идут (нечего мерить).
		fmt.Printf("  skipped (404)      : %s\n", strings.Join(r.Skipped, ", "))
	}
	// Сортируем по убыванию throughput — самые горячие ручки сверху.
	eps := make([]legion.EndpointStat, len(r.Endpoints))
	copy(eps, r.Endpoints)
	sort.Slice(eps, func(i, j int) bool { return eps[i].Throughput > eps[j].Throughput })
	for _, s := range eps {
		printEndpointStat(s)
	}
	if r.FirstErr != "" {
		fmt.Printf("  ВНИМАНИЕ первая ошибка: %s\n", r.FirstErr)
	}
	fmt.Println("═══════════════════════════════════════════════════════════════")
}

// printWriteReport печатает per-kind отчёт write-оси: на каждую сущность две
// строки (POST <kind> и DELETE <kind>) с req/err/p50/p99/max/thr, как в оси B.
// Финальная строка — sweep (сколько остаточных legionload-* прибрала страховка;
// в норме 0 — per-iteration delete всё прибрал).
func printWriteReport(r *legion.WriteLoadReport) {
	fmt.Println("════════════════════ ось write — create→delete ════════════════════")
	fmt.Printf("  длительность       : %s (%d сущностей × 2 операции)\n",
		r.Wall.Round(time.Millisecond), len(r.Entities))
	for _, e := range r.Entities {
		printEndpointStat(e.Create)
		printEndpointStat(e.Delete)
	}
	fmt.Printf("  sweep остаточных   : %d (страховка от лика; в норме 0 — per-iteration delete всё прибрал)\n", r.Swept)
	if r.FirstErr != "" {
		fmt.Printf("  ВНИМАНИЕ первая ошибка: %s\n", r.FirstErr)
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
	fmt.Println("════════════════════ ось C — один Voyage ════════════════════")
	fmt.Printf("  voyage_id          : %s\n", r.VoyageID)
	fmt.Printf("  kind               : command (единица батча = хост)\n")
	fmt.Printf("  scope_size         : %d (резолвнуто хостов по coven)\n", r.ScopeSize)
	fmt.Printf("  create-латентность : %s (POST приём+резолв+persist)\n", r.CreateLat.Round(time.Millisecond))
	fmt.Printf("  end-to-end         : %s (POST → терминал; dispatch→ErrandResult→commit→audit)\n", r.EndToEnd.Round(time.Millisecond))
	fmt.Printf("  финальный статус   : %s (succeeded=%d failed=%d)\n", r.FinalStatus, r.Succeeded, r.Failed)
	fmt.Printf("  poll-итераций      : %d\n", r.Polls)
	fmt.Printf("  ErrandRequest-ов отвечено стабами: %d\n", totalErrands)
	fmt.Println("  ── audit/claim (мерить вручную, docs/testing/load-testing.md §4.2) ──")
	fmt.Println("    audit-INSERT-rate : docker exec soul-stack-postgres psql -U keeper -d keeper -c \\")
	fmt.Println("                        \"SELECT count(*) FROM audit_log WHERE created_at > NOW() - INTERVAL '2 min'\"")
	fmt.Println("    apply_runs/errands: psql -c \"SELECT count(*) FROM errands WHERE started_at > NOW() - INTERVAL '2 min'\"")
	fmt.Println("══════════════════════════════════════════════════════════════")
}

// mintIdentities параллельно выпускает count сертов. Vault dev-PKI выдерживает
// десятки параллельных issue; ограничиваем issueConc, чтобы не насыщать Vault на
// setup-фазе (она не предмет замера — мерим Keeper).
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
		return nil, fmt.Errorf("setup: ни один cert не выпущен: %w", firstErr)
	}
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "[setup] предупреждение: %d issue-ошибок (первая: %v)\n", p.count-len(out), firstErr)
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
		fmt.Printf("  recv-loop обрывы   : %d (НЕштатные, Keeper сбросил стрим; первый: %s)\n",
			rep.RecvErrors, rep.FirstRecvErr)
	}

	m := rep.MetricsAtPeak
	if m.Found {
		fmt.Println("  ── Keeper /metrics на пике ──")
		fmt.Printf("    keeper_grpc_streams_active : %.0f (было %.0f, дельта %+.0f)\n",
			m.StreamsActive, before.StreamsActive, m.StreamsActive-before.StreamsActive)
		fmt.Printf("    go_goroutines              : %.0f (было %.0f, дельта %+.0f)\n",
			m.GoGoroutines, before.GoGoroutines, m.GoGoroutines-before.GoGoroutines)
		fmt.Printf("    process_resident_memory    : %.1f MiB (было %.1f MiB)\n",
			m.ResidentBytes/1024/1024, before.ResidentBytes/1024/1024)
		fmt.Printf("    go_heap_inuse              : %.1f MiB (было %.1f MiB)\n",
			m.HeapInUseBytes/1024/1024, before.HeapInUseBytes/1024/1024)
		printScalingCompare(m, rep.Achieved)
	} else {
		fmt.Println("  (метрики не скрейпились — --metrics пуст или /metrics недоступен)")
	}

	fmt.Println("  ── внешние пробелы (мерить вручную, docs/testing/load-testing.md §4.2) ──")
	fmt.Println("    Redis SID-lease     : redis-cli -p 6381 INFO commandstats | grep -E 'cmdstat_(set|exists|get)'")
	fmt.Println("    PG pool/claim       : docker exec soul-stack-postgres psql -U keeper -d keeper -c \\")
	fmt.Println("                          \"SELECT count(*) FROM pg_stat_activity WHERE datname='keeper'\"")
	fmt.Println("    Keeper RAM/threads  : ps -o rss,nlwp -p <keeper-pid>  (RSS в KiB / число потоков)")
	fmt.Println("    Conclave live-count : redis-cli -p 6381 KEYS 'keeper:instance:*'")
	fmt.Println("═════════════════════════════════════════════════════════════")
}

// printDrain печатает результат финального скрейпа после teardown стримов:
// keeper_grpc_streams_active должен вернуться к baseline-у (decrement сработал на
// каждый Close, стримы не утекли). Δ относительно baseline ≠ 0 — сигнал утечки.
func printDrain(drained, before legion.MetricsSnapshot) {
	if !drained.Found {
		fmt.Println("  drained streams_active: (метрики недоступны — дрейн не проверен)")
		return
	}
	delta := drained.StreamsActive - before.StreamsActive
	verdict := "OK (вернулся к baseline)"
	if delta > 0.5 {
		verdict = "ВНИМАНИЕ: streams_active НЕ вернулся к baseline — возможна утечка стримов (Dec не сработал на Close)"
	}
	fmt.Printf("  drained streams_active : %.0f (baseline %.0f, дельта %+.0f) — %s\n",
		drained.StreamsActive, before.StreamsActive, delta, verdict)
}

// printScalingCompare сверяет наблюдаемый расход RAM/горутин с расчётной строкой
// scaling.md (Keeper 8 GB / 8 vCPU под 100k VM). Линейная экстраполяция «на одну
// душу» — грубая, для sanity-валидации порядка, не точного sizing-а (Ф0-цель).
func printScalingCompare(m legion.MetricsSnapshot, achieved int) {
	if achieved <= 0 {
		return
	}
	const scalingKeeperRAMBytes = 8.0 * 1024 * 1024 * 1024 // 8 GB на инстанс
	const scalingTargetVM = 100_000.0                      // целевой масштаб

	ramPerSoul := m.ResidentBytes / float64(achieved)
	goroutPerSoul := m.GoGoroutines / float64(achieved)
	projRAM100k := ramPerSoul * scalingTargetVM
	fmt.Println("  ── сверка с scaling.md (Keeper 8 GB/8 vCPU под 100k VM) ──")
	fmt.Printf("    RSS на душу        : ~%.2f MiB\n", ramPerSoul/1024/1024)
	fmt.Printf("    горутин на душу    : ~%.2f\n", goroutPerSoul)
	fmt.Printf("    экстрап. RSS@100k  : ~%.1f GiB (бюджет scaling.md: %.0f GiB/инстанс × 3+)\n",
		projRAM100k/1024/1024/1024, scalingKeeperRAMBytes/1024/1024/1024)
	if projRAM100k > scalingKeeperRAMBytes*3 {
		fmt.Println("    ВНИМАНИЕ: линейная экстраполяция RSS@100k превышает 3×8 GB бюджет — повод для Ф1/Ф2-замера")
	}
}
