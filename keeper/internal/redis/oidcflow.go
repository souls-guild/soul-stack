package redis

// OIDCFlowStore is a short-lived server-side store for OIDC code-flow state
// (ADR-058(b), stage 2). Between /auth/oidc/login and /auth/oidc/callback,
// Keeper must remember per-flow secrets that must NOT go into the URL/browser:
//
//   - nonce — anti-replay for id_token (checked against the `nonce` claim);
//   - code_verifier — the PKCE secret (the S256 challenge went to the IdP in
//     the login URL, the verifier stays on the server and is presented at
//     code-exchange time).
//
// The key is an opaque `state` (a CSRF token, the only thing that goes to the
// browser and comes back on callback). The store is cluster-shared (any
// Keeper instance can accept the callback after a login on another one —
// stateless cluster, ADR-002): Redis, not an in-memory map.
//
// Single-use: Consume atomically reads AND deletes the entry (GETDEL). A
// repeat callback with the same state finds nothing → rejected. This closes
// authorization-code replay and double-submit. TTL (~5 min) bounds the window
// of an unfinished flow.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrOIDCFlowNotFound — Consume found no entry for the state: either an
// unknown/forged state (CSRF), an already-consumed one (replay), or an
// expired TTL. All three are indistinguishable from the outside (anti-oracle)
// — the endpoint maps them to a single generic rejection.
var ErrOIDCFlowNotFound = errors.New("redis: oidc flow state not found")

// oidcFlowKeyPrefix is the namespace for flow-state keys. Kept separate from
// lease/heartbeat to avoid clashing with other coordination keys.
const oidcFlowKeyPrefix = "oidc:flow:"

// OIDCFlowState holds the server-side secrets of a single code flow.
// Serialized into Redis as JSON under the state key. No field ever leaves the
// server to the browser.
type OIDCFlowState struct {
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
}

// OIDCFlowStore is the Redis-backed store implementation. nil-safety isn't
// needed: the OIDC endpoint is mounted only when Redis is present
// (ADR-006/ADR-053) — without Redis, the flow is impossible (cluster-shared
// requirement).
type OIDCFlowStore struct {
	client *Client
	ttl    time.Duration
}

// NewOIDCFlowStore builds a store on top of a Redis client. ttl > 0 is
// required (zero/negative → a caller bug, same as Acquire).
func NewOIDCFlowStore(c *Client, ttl time.Duration) (*OIDCFlowStore, error) {
	if c == nil {
		return nil, errors.New("redis.NewOIDCFlowStore: nil client")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redis.NewOIDCFlowStore: ttl must be > 0, got %v", ttl)
	}
	return &OIDCFlowStore{client: c, ttl: ttl}, nil
}

// Save stores the state under state with a TTL. SET NX: if the key already
// exists (a collision on a 256-bit state is practically impossible, but we
// guard against overwriting an active flow) — an error. state must be
// non-empty (the caller generates it via crypto/rand).
func (s *OIDCFlowStore) Save(ctx context.Context, state string, fs OIDCFlowState) error {
	if state == "" {
		return errors.New("redis.OIDCFlowStore.Save: empty state")
	}
	payload, err := json.Marshal(fs)
	if err != nil {
		return fmt.Errorf("redis.OIDCFlowStore.Save: marshal: %w", err)
	}
	ok, err := s.client.underlying().SetNX(ctx, oidcFlowKeyPrefix+state, payload, s.ttl).Result()
	if err != nil {
		return fmt.Errorf("redis.OIDCFlowStore.Save: SETNX: %w", err)
	}
	if !ok {
		return errors.New("redis.OIDCFlowStore.Save: state collision")
	}
	return nil
}

// Consume atomically reads AND deletes the entry for state (GETDEL). If the
// entry isn't found → [ErrOIDCFlowNotFound]. Single-use: a repeat Consume of
// the same state returns ErrOIDCFlowNotFound (anti-replay).
func (s *OIDCFlowStore) Consume(ctx context.Context, state string) (OIDCFlowState, error) {
	if state == "" {
		return OIDCFlowState{}, ErrOIDCFlowNotFound
	}
	raw, err := s.client.underlying().GetDel(ctx, oidcFlowKeyPrefix+state).Bytes()
	if err != nil {
		// redis.Nil — no key (unknown/consumed/expired state); distinguished
		// from a network error (SoulLeaseOwner pattern).
		if errors.Is(err, redis.Nil) {
			return OIDCFlowState{}, ErrOIDCFlowNotFound
		}
		return OIDCFlowState{}, fmt.Errorf("redis.OIDCFlowStore.Consume: GETDEL: %w", err)
	}
	var fs OIDCFlowState
	if err := json.Unmarshal(raw, &fs); err != nil {
		return OIDCFlowState{}, fmt.Errorf("redis.OIDCFlowStore.Consume: unmarshal: %w", err)
	}
	return fs, nil
}
