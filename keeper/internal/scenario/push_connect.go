package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// errPushUnreachable is a push host whose SSH session could not be opened within
// soul_timeout: the push host's unavailability (user decision, NIM-949).
var errPushUnreachable = errors.New(soulUnavailable)

// sendPush runs a push host's step. Under linear dispatch a failed SSH connect —
// nothing started on the host — is retried until the step's soul_timeout, the push
// host's equivalent of waiting for a stream, and stops when the step ends. A
// session that broke after the command started is never retried: that would
// apply the step twice.
func (r *Runner) sendPush(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, sid string, route push.Route, req *keeperv1.ApplyRequest, onEvent push.EventHandler) (*keeperv1.RunResult, error) {
	rr, err := r.deps.PushApply.SendApply(ctx, sid, route, req, onEvent)
	if spec.executionModel != ExecutionLinear || !errors.Is(err, push.ErrConnect) {
		return rr, err
	}
	soulTimeout := stepSoulTimeout(spec, passage)
	deadline := time.Now().Add(soulTimeout)
	log.Info("scenario: push host unreachable - retrying the SSH connect until soul_timeout",
		slog.String("sid", sid), slog.Duration("soul_timeout", soulTimeout), slog.Any("error", err))
	// Five barrier polls between attempts: a second in production.
	tick := time.NewTicker(5 * r.pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, err
		case <-stepDone(ctx):
			return nil, err
		case <-tick.C:
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: no SSH session within soul_timeout (%s): %w", errPushUnreachable, soulTimeout, err)
		}
		rr, err = r.deps.PushApply.SendApply(ctx, sid, route, req, onEvent)
		if !errors.Is(err, push.ErrConnect) {
			return rr, err
		}
	}
}
