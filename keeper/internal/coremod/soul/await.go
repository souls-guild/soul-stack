package soul

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/config"

	"google.golang.org/protobuf/types/known/structpb"
)

// defaultAwaitPollInterval is default presence poll period (parity
// keeper.yml::acolyte_poll_interval). Small but non-zero: presence-check
// is one Redis-pipeline on EXISTS-command per SID (cheap), 2s sufficiently
// frequent for onboarding and creates no load on Redis during long barrier.
const defaultAwaitPollInterval = 2 * time.Second

// awaitConfig is parsed+validated onboarding barrier parameters
// (ADR-061). nil pointer means "barrier not requested".
//
// requireFacts (ADR-061 amendment, 7th wall live-create): on
// refresh_soulprint: true SID counts toward barrier only when online
// (presence-lease) AND typed soulprint written to PG — else next
// Passage render would read soulprint.self.* before async write of first report.
type awaitConfig struct {
	timeout      time.Duration
	minCount     int
	pollInterval time.Duration
	requireFacts bool
}

// awaitResult is barrier outcome. online/pending — by presence-lease; factless —
// online-SID without typed facts (only when requireFacts, else empty); ready —
// counted toward barrier (online, when requireFacts — minus factless).
// lastErr — presence/facts error from last polls for timeout diagnostics.
// startedAt — when the wait began, the line the timeout diagnostics draw between
// an agent that dropped during the wait and one that never came. runEnded — the
// run context's error when the run ended before the barrier's own timeout; the
// shortfall fields are then as of that moment.
type awaitResult struct {
	online    []string
	pending   []string
	factless  []string
	ready     []string
	satisfied bool
	lastErr   error
	startedAt time.Time
	runEnded  error
}

// slowPollThreshold separates a poll the end of the wait merely caught in flight
// — a normal one takes milliseconds, and NIM-886's live one began on the same
// tick as the deadline — from a source that hung through the end of the wait.
const slowPollThreshold = time.Second

// validateAwaitParams is static validation of await fields (for Validate /
// soul-lint runtime safety). sidCount — number of registered SIDs (for
// checking await_min_count ≤ len(sids)). Returns list of text errors.
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

	// await_timeout required when await_online (barrier must not hang forever).
	if awaitOnline && timeoutStr == "" {
		errs = append(errs, fmt.Sprintf("param %q is required when %q is true", "await_timeout", "await_online"))
	}
	return errs
}

// parseAwait parses await parameters into awaitConfig. Returns (nil, nil)
// if barrier not requested (await_online omitted/false). Error — invalid
// parameter / unreachable quorum / ceiling exceeded / no presence-checker.
//
// Static validation part (types, duration-format, min ≤ len, await_timeout
// required) delegated to validateAwaitParams — single source of truth for these
// texts so Apply-path and Validate-path don't diverge in wording.
// Here remains what Validate cannot express: presence-checker,
// timeout positivity and max_await_timeout ceiling (depend on module runtime-state,
// not just params).
//
// Ceiling (max_await_timeout, ADR-061): fail-closed — await_timeout > ceiling
// fails with error BEFORE any poll (explicit error, NOT silent truncation).
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

	// Barrier without presence source impossible: silent success not allowed.
	if m.presence == nil {
		return nil, errors.New("await_online requires presence-checker (Redis SID-lease), not configured")
	}

	// validateAwaitParams guaranteed valid non-empty await_timeout.
	timeoutStr, _ := util.OptStringParam(params, "await_timeout")
	timeout, _ := config.ParseDuration(timeoutStr)
	if timeout <= 0 {
		return nil, fmt.Errorf("param %q: must be > 0", "await_timeout")
	}

	// Ceiling keeper.yml::max_await_timeout — fail-closed DoS-guard.
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

// resolvedMaxAwaitTimeout returns effective await_timeout ceiling from current
// keeper.yml snapshot (hot-reload via maxAwaitTimeout provider). Nil provider /
// empty string / invalid → config.DefaultMaxAwaitTimeout.
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

// awaitOnline polls SID readiness blocking until ready ≥ minCount or timeout expires.
// Readiness: online (Redis SID-lease); with cfg.requireFacts — online AND typed
// soulprint in PG (ADR-061 amendment: if facts already written → zero wait on
// rerun/create_from_souls; waits only for first provision-from-zero report).
//
// res.lastErr is presence/facts error from last polls (for timeout diagnostics:
// "infra unavailable" vs "hosts not onboarded"). Returned even if satisfied=false
// without fatal, so caller can distinguish reason for shortfall.
//
// Source of truth for online is lease (PresenceChecker), NOT PG souls.status
// (ADR-006(a)/ADR-061). Persistent infra error → error (B1-strict cannot confirm
// quorum blindly). A run that ends while the barrier waits is not an error here:
// res.runEnded says so, and the shortfall is reported as of that moment.
func (m *Module) awaitOnline(ctx context.Context, sids []string, cfg *awaitConfig) (awaitResult, error) {
	bctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// First poll — immediately (hosts may already be online before step), then by ticker.
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	res := awaitResult{startedAt: time.Now()}
	// polled: a poll's answer has been committed to res — presence, and facts
	// when required, or facts that failed for a reason of their own, or, with
	// no earlier answer to keep, the presence half of a poll whose facts check
	// the end of the wait cut.
	polled := false
	for {
		pollStart := time.Now()
		alive, perr := m.presence.SoulsStreamAlive(bctx, sids)
		if perr != nil {
			if excused, err := pollError(bctx, perr, pollStart, polled, res.lastErr); !excused {
				res.lastErr = err
			}
		} else {
			online, pending := splitOnline(sids, alive)
			ready, factless := online, []string(nil)
			var ferr error
			excused := false
			if cfg.requireFacts {
				factsStart := time.Now()
				withFacts, err := m.Store.SoulsWithSoulprint(bctx, sids)
				if err != nil {
					// facts unknown → quorum not evaluated this poll.
					ready = nil
					excused, ferr = pollError(bctx, err, factsStart, polled, res.lastErr)
				} else {
					ready, factless = splitFacts(online, withFacts)
				}
			}
			// An excused facts poll answers nothing about the hosts: the last
			// complete answer stays, or the message would name no shortfall at all.
			// With no earlier answer to keep, its presence half still counts —
			// presence did answer, and dropping it would report a presence failure.
			if !excused || !polled {
				polled = true
				res.online, res.pending, res.ready, res.factless = online, pending, ready, factless
				if !excused {
					res.lastErr = ferr
				}
				if ferr == nil && len(ready) >= cfg.minCount {
					res.satisfied = true
					return res, nil
				}
			}
		}

		select {
		case <-bctx.Done():
			if !polled {
				res.pending = sids
			}
			if err := ctx.Err(); err != nil {
				res.runEnded = err
				return res, nil
			}
			// Barrier timeout. If ALL polls failed with infra error, return it
			// fatally (readiness source unavailable); otherwise res.lastErr, if
			// any, enriches the shortfall diagnostics.
			if !polled && res.lastErr != nil {
				return res, fmt.Errorf("await_online: presence check failed: %w", res.lastErr)
			}
			return res, nil
		case <-ticker.C:
		}
	}
}

// pollError decides what a failed poll leaves as the last error, and whether it
// leaves anything. A poll the end of the wait caught in flight says nothing
// about the source it was sent to — keeping it named Redis as the cause of
// NIM-886's timeout, "redis.SoulsStreamAlive: pipeline EXEC: context deadline
// exceeded", over a healthy Redis — so it is excused when an earlier error
// already says more, or when an earlier poll answered and this one was merely
// caught in flight. A source that hung through the end of the wait, or never
// answered at all, is that source's failure and stays reported.
func pollError(bctx context.Context, err error, pollStart time.Time, answered bool, held error) (excused bool, last error) {
	if bctx.Err() == nil {
		return false, err
	}
	if held != nil {
		return true, nil
	}
	if hung := time.Since(pollStart); hung >= slowPollThreshold {
		return false, fmt.Errorf("%w (no answer for %s)", err, hung.Round(time.Millisecond))
	}
	if answered {
		return true, nil
	}
	return false, err
}

// barrierTimeoutMessage is B1-strict barrier failure diagnostics. With requireFacts,
// shortfall classes are split: "not online" (no lease) vs "online but factless"
// (lease exists, typed soulprint not yet written) — so operator can distinguish
// failed onboarding from race on first report.
func barrierTimeoutMessage(sids []string, cfg *awaitConfig, res awaitResult) string {
	window := "within " + cfg.timeout.String()
	if res.runEnded != nil {
		// The run's own deadline can be the shorter one — a plan without
		// refresh_soulprint keeps the 5-minute default — and the hosts still owe
		// an answer then.
		window = fmt.Sprintf("before the run ended (%v), %s into await_timeout=%s",
			res.runEnded, time.Since(res.startedAt).Round(time.Second), cfg.timeout)
	}
	var msg string
	if cfg.requireFacts {
		msg = fmt.Sprintf(
			"onboarding barrier: %d/%d souls ready (online+soulprint) to await_min_count=%d %s",
			len(res.ready), len(sids), cfg.minCount, window)
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
		"onboarding barrier: %d/%d souls online to await_min_count=%d %s (pending: %v)",
		len(res.online), len(sids), cfg.minCount, window, res.pending)
	// Persistent presence failure on last polls: else infra problem (Redis unavailable)
	// masks as "hosts not onboarded".
	if res.lastErr != nil {
		msg += fmt.Sprintf(" (last presence error: %v)", res.lastErr)
	}
	return msg
}

// notOnlineLookupTimeout bounds the registry reads behind the timeout message:
// the barrier has already failed, and its diagnostics must not hold the run.
const notOnlineLookupTimeout = 5 * time.Second

// notOnlineFacts says, for each SID the barrier gave up on, what the registry
// knows about its agent. The poll can only answer "not online"; no stream on
// record, a last stream before the wait began, and one during it point at
// different faults, and only the second can be a record whose machine no longer
// holds its identity. `last_seen_at` is flushed on stream traffic, throttled to
// a third of `stale_after` (30s by default), so the line is at least that
// coarse. A SID the registry cannot be read for is left out rather than allowed
// to mask the failure itself.
func (m *Module) notOnlineFacts(ctx context.Context, sids []string, startedAt time.Time) string {
	if len(sids) == 0 {
		return ""
	}
	lctx, cancel := context.WithTimeout(ctx, notOnlineLookupTimeout)
	defer cancel()
	parts := make([]string, 0, len(sids))
	for _, sid := range sids {
		s, err := m.Store.SelectBySID(lctx, sid)
		if err != nil {
			continue
		}
		parts = append(parts, sid+": "+agentFact(s.LastSeenAt, startedAt))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

func agentFact(lastSeen *time.Time, startedAt time.Time) string {
	switch {
	case lastSeen == nil:
		return "no stream on record"
	case lastSeen.Before(startedAt):
		return "last stream on record " + lastSeen.UTC().Format(time.RFC3339) + ", before the wait began"
	default:
		return "last stream on record " + lastSeen.UTC().Format(time.RFC3339) + ", during the wait"
	}
}

// splitOnline divides SID set into online (in alive set) and pending.
// Deterministic order follows input sids order.
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

// splitFacts divides online set into ready (typed soulprint written) and factless.
// Order follows input online order.
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
