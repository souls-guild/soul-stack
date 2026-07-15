package augur

import (
	"context"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Fetcher is the narrow Augur-client surface needed by core.augur.fetch.
// Kept separate so the module doesn't depend on the concrete *Client (can be
// tested with a fake fetcher) and has no access to Deliver/Close (session
// recv-loop infrastructure, not the module's concern). *Client satisfies it.
type Fetcher interface {
	Fetch(ctx context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error)
}

// runContext carries the session's Augur client and the run's apply_id into
// the apply cycle. Stored in ctx before the module call (via stream.Context())
// since the generic SoulModule contract (state+params) can't express it, and
// custom sub-process modules aren't meant to have Augur access — it's a
// keeper-side trusted channel for Soul-side core only.
type runContext struct {
	fetcher Fetcher
	applyID string
}

type ctxKey struct{}

// WithRun stores the Augur client and apply_id in ctx for one run. fetcher may
// be nil (push mode / session without Augur) — FromContext then returns
// ok=false and the module reports a clear "Augur unavailable" error.
func WithRun(ctx context.Context, fetcher Fetcher, applyID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, runContext{fetcher: fetcher, applyID: applyID})
}

// FromContext extracts the Augur client and apply_id. ok=false if ctx carries
// nothing (apply without Augur plumbing) or fetcher is nil — the module
// treats this as "Augur unavailable for this run".
func FromContext(ctx context.Context) (fetcher Fetcher, applyID string, ok bool) {
	rc, present := ctx.Value(ctxKey{}).(runContext)
	if !present || rc.fetcher == nil {
		return nil, "", false
	}
	return rc.fetcher, rc.applyID, true
}
