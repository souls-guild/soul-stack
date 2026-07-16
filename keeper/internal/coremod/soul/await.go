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
type awaitResult struct {
	online    []string
	pending   []string
	factless  []string
	ready     []string
	satisfied bool
	lastErr   error
}

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
// quorum blindly). context-cancel (run cancellation) → also error.
func (m *Module) awaitOnline(ctx context.Context, sids []string, cfg *awaitConfig) (awaitResult, error) {
	bctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// First poll — immediately (hosts may already be online before step), then by ticker.
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	var res awaitResult
	polled := false // at least one poll reached presence result
	for {
		alive, perr := m.presence.SoulsStreamAlive(bctx, sids)
		if perr != nil {
			res.lastErr = perr
		} else {
			polled = true
			res.online, res.pending = splitOnline(sids, alive)
			res.ready, res.factless = res.online, nil
			res.lastErr = nil // successful poll clears "hanging" infra-error
			if cfg.requireFacts {
				withFacts, ferr := m.Store.SoulsWithSoulprint(bctx, sids)
				if ferr != nil {
					// facts unknown → quorum not evaluated this poll.
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
			// Barrier timeout: if we reached presence result at least once, diagnostic
			// fields are populated (B1-strict diagnostics). If ALL polls failed with
			// infra error, return it fatally (readiness source unavailable).
			if !polled && res.lastErr != nil {
				res.pending = sids
				return res, fmt.Errorf("await_online: presence check failed: %w", res.lastErr)
			}
			if !polled {
				res.pending = sids
			}
			// res.lastErr nonzero here → persistent failure on last polls with partial
			// shortfall: remains in res for diagnostics enrichment.
			return res, nil
		case <-ticker.C:
		}
	}
}

// barrierTimeoutMessage is B1-strict barrier failure diagnostics. With requireFacts,
// shortfall classes are split: "not online" (no lease) vs "online but factless"
// (lease exists, typed soulprint not yet written) — so operator can distinguish
// failed onboarding from race on first report.
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
	// Persistent presence failure on last polls: else infra problem (Redis unavailable)
	// masks as "hosts not onboarded".
	if res.lastErr != nil {
		msg += fmt.Sprintf(" (last presence error: %v)", res.lastErr)
	}
	return msg
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
