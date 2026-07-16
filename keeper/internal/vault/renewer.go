package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	vaultapi "github.com/hashicorp/vault/api"
)

// TokenRenewer is a background auto-renewer for the keeper's token.
//
// Without it, a short-lived token (approle in staging/prod) expires by TTL →
// all vault resolutions (CEL vault(), vault:-ref, core.vault.kv-read,
// JWT-signing-key reads) start failing → Operator API outage. The watcher
// keeps the token alive as long as the process is alive.
//
// Degradation: a root/static dev token (common locally) isn't renewable —
// in that case the watcher doesn't start (warn in the log), and keeper
// keeps running.
//
// Lifecycle: Start launches a goroutine under the passed ctx; on ctx.Done()
// (SIGTERM) the goroutine stops the vault watcher and exits. Stop() gives a
// synchronous wait — the caller waits for the goroutine to exit in the
// shutdown defer, symmetric with the reaper runner in keeper run.
type TokenRenewer struct {
	c      *vaultapi.Client
	logger *slog.Logger

	watcher *vaultapi.LifetimeWatcher
	done    chan struct{}
}

// StartTokenRenewer checks the renewable flag of the current token and, if
// it's renewable, starts a background LifetimeWatcher. Returns a
// *TokenRenewer with a Stop method for graceful shutdown, or nil if no
// watcher is needed (a non-renewable token is normal degradation, not an
// error).
//
// Returns an error only on lookup-self failure or watcher construction
// failure — the caller (keeper run) decides whether that's fatal or a
// warning. The fact that "the token isn't renewable" is NOT considered an
// error.
func (c *Client) StartTokenRenewer(ctx context.Context, logger *slog.Logger) (*TokenRenewer, error) {
	if logger == nil {
		logger = slog.Default()
	}

	self, err := c.c.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: token lookup-self: %w", err)
	}

	renewable, err := self.TokenIsRenewable()
	if err != nil {
		return nil, fmt.Errorf("vault: parse token renewable flag: %w", err)
	}
	if !renewable {
		// A root/static dev token is a normal case. We don't fail, we just
		// live without auto-renew. In prod this signals that approle is
		// configured as not renewable — hence warn, not info.
		logger.Warn("vault: token not renewable, auto-renew disabled")
		return nil, nil
	}

	// LifetimeWatcher renews based on a Secret. For a token we build a
	// Secret with Auth.ClientToken — otherwise the watcher doesn't know
	// which token to renew. RenewBehaviorIgnoreErrors: transient network
	// errors don't kill the watcher instantly — it keeps retrying up to the
	// lifetime threshold, after which it exits normally via DoneCh (the
	// caller gets a warn and keeper stays on the last valid token until it
	// actually expires).
	secret := &vaultapi.Secret{
		Auth: &vaultapi.SecretAuth{
			ClientToken:   c.c.Token(),
			Renewable:     true,
			LeaseDuration: selfTokenTTLSeconds(self),
		},
	}
	watcher, err := c.c.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{
		Secret:        secret,
		RenewBehavior: vaultapi.RenewBehaviorIgnoreErrors,
	})
	if err != nil {
		return nil, fmt.Errorf("vault: new lifetime watcher: %w", err)
	}

	r := &TokenRenewer{
		c:       c.c,
		logger:  logger,
		watcher: watcher,
		done:    make(chan struct{}),
	}

	logger.Info("vault: token auto-renew enabled")
	go r.run(ctx)
	return r, nil
}

// run drives the vault watcher until ctx.Done() or until it exits on its
// own (DoneCh — the token reached the lifetime threshold and can't be
// renewed further).
func (r *TokenRenewer) run(ctx context.Context) {
	defer close(r.done)

	go r.watcher.Start()
	defer r.watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: stop the watcher (defer) and exit.
			r.logger.Info("vault: token auto-renew stopping (shutdown)")
			return

		case err := <-r.watcher.DoneCh():
			// The watcher exited on its own. err != nil — renewal failed;
			// err == nil — the token reached the threshold and renew won't
			// extend it further (nothing more to do). Either way the token
			// is soon/already invalid — this needs to be visible in the
			// logs. The token value is NOT logged.
			if err != nil {
				r.logger.Error("vault: token auto-renew failed, token will expire", slog.Any("error", err))
			} else {
				r.logger.Warn("vault: token auto-renew exhausted (lease at threshold), token will expire")
			}
			return

		case renewal := <-r.watcher.RenewCh():
			// Successful renewal. We only log the TTL, not the token.
			r.logger.Info("vault: token renewed",
				slog.Int("lease_duration_seconds", leaseDurationSeconds(renewal.Secret)))
		}
	}
}

// Stop blocks until the background goroutine exits. The caller invokes it
// from a shutdown defer after ctx is canceled. Safe when r == nil (when the
// watcher never started — a non-renewable token).
func (r *TokenRenewer) Stop() {
	if r == nil {
		return
	}
	<-r.done
}

// leaseDurationSeconds extracts the token's remaining lease in seconds from
// the watcher's renew response (RenewCh): the TTL arrives in
// Secret.Auth.LeaseDuration or the top-level LeaseDuration. 0 if there's no
// data (logging only). For a lookup-self response, the TTL lives elsewhere
// (Data["ttl"]) — see selfTokenTTLSeconds.
func leaseDurationSeconds(s *vaultapi.Secret) int {
	if s == nil {
		return 0
	}
	if s.Auth != nil && s.Auth.LeaseDuration > 0 {
		return s.Auth.LeaseDuration
	}
	return s.LeaseDuration
}

// selfTokenTTLSeconds extracts the token's remaining TTL from a lookup-self
// response. Vault puts it in Data["ttl"] (JSON number), while Auth/
// top-level LeaseDuration are zero in this response — hence a separate
// function for seeding the watcher. 0 if the field is missing/unparseable
// (the watcher will renew on the first cycle and fill in the real TTL
// itself; the seed is just a hint for the initial schedule).
func selfTokenTTLSeconds(s *vaultapi.Secret) int {
	if s == nil || s.Data == nil {
		return 0
	}
	raw, ok := s.Data["ttl"]
	if !ok {
		return 0
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0
	}
	ttl, err := n.Int64()
	if err != nil || ttl < 0 {
		return 0
	}
	return int(ttl)
}
