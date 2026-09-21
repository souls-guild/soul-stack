// Package oidc implements OAuth2/OIDC authentication for operators (Archons)
// via the authorization-code flow with PKCE. ADR-058(b) (stage 2, accepted +
// end-to-end).
//
// Model (ADR-058): the external IdP authenticates the human Archon, Keeper
// VALIDATES the id_token, extracts claims (sub/groups) and returns an
// auth.ExternalIdentity — mapping onto operators(aid)+roles and issuing the
// internal JWT is done by auth.Mapper + jwt.Issuer further up the stack.
//
// Flow (ADR-058(b)):
//  1. BeginLogin (GET /auth/oidc/login) → crypto/rand state+nonce+PKCE
//     verifier; S256 code_challenge; persist {state→(nonce,verifier)} in
//     FlowStore (Redis, TTL ~5m, cluster-shared); redirect to the IdP's
//     authorization_endpoint.
//  2. the human authenticates with the IdP.
//  3. CompleteLogin (GET /auth/oidc/callback?code&state) → Consume state from
//     the store (CSRF + single-use), code-exchange with the code_verifier
//     (PKCE), id_token VALIDATION via the go-oidc verifier (JWKS signature /
//     iss / aud==client_id / exp / iat), nonce check (anti-replay), claims
//     extraction.
//
// Security (ADR-058(g), "security first"):
//   - PKCE is mandatory (S256): code_verifier stays server-side, without it
//     the code-exchange is impossible (anti code-interception);
//   - state is a CSRF token, single-use (FlowStore.Consume = GETDEL);
//   - nonce protects against id_token replay;
//   - issuer is HTTPS-only (validated by the config layer + New);
//   - any failure reason is sanitized into auth.ErrAuthFailed (anti-oracle);
//     details go only to the debug log, without tokens/secrets.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// defaultAIDClaim is the claim the AID is derived from when aid_claim is not
// set. `sub` was chosen as the default: a stable opaque subject identifier at
// the IdP, always present (a mandatory OIDC claim), unchanged when the email
// changes.
const defaultAIDClaim = "sub"

// defaultGroupsClaim is the default claim carrying groups (Keycloak/most IdPs).
const defaultGroupsClaim = "groups"

// defaultScopes is the minimal scope set used when none is configured: openid
// is mandatory (without it the IdP won't return an id_token), profile/email/
// groups are for the claims.
var defaultScopes = []string{gooidc.ScopeOpenID, "profile", "email", "groups"}

// Config is the resolved configuration of the OIDC authenticator. ClientSecret
// has already been resolved from Vault at load time (ADR-058(e)) — plaintext
// arrives here, NOT a *_ref. TLSCA is the resolved IdP CA bundle (optional).
//
// Fields are 1:1 with config.KeeperAuthOIDC (shared/config/keeper.go), minus
// the *_ref suffixes.
type Config struct {
	Issuer       string   // https://idp/realms/... (discovery base, HTTPS-only)
	ClientID     string   // OAuth2 client_id (== expected id_token aud)
	ClientSecret string   // resolved from client_secret_ref (may be empty for a public client)
	RedirectURL  string   // https://keeper/auth/oidc/callback
	Scopes       []string // openid (required), email, profile, groups
	TLSCA        []byte   // resolved IdP CA bundle (optional)
	AIDClaim     string   // claim → AID (default sub)
	GroupsClaim  string   // claim → []groups for the role map (default groups)
}

// FlowStore is the server-side store of code-flow secrets (nonce + PKCE
// verifier), keyed by state. Declared as an interface so the oidc package does
// not import keeper/internal/redis (layering) and can be tested with an
// in-memory fake. *redis.OIDCFlowStore satisfies it automatically (Save/Consume).
type FlowStore interface {
	Save(ctx context.Context, state string, fs FlowState) error
	Consume(ctx context.Context, state string) (FlowState, error)
}

// FlowState holds the server-side secrets of a single flow (mirrors
// redis.OIDCFlowState). Duplicated here so the oidc package does not depend on
// the redis package's types; the daemon adapter converts between them at
// wiring time.
type FlowState struct {
	Nonce        string
	CodeVerifier string
}

// ErrFlowNotFound means Consume found no entry for the given state (CSRF /
// replay / expired TTL). The FlowStore implementation returns it; CompleteLogin
// maps it to ErrAuthFailed.
var ErrFlowNotFound = errors.New("auth/oidc: flow state not found")

// Authorization is returned when the login flow starts: where to redirect the
// browser and which opaque state to keep (for endpoint logs). CodeVerifier/
// Nonce are already stored in FlowStore and are NOT present in Authorization
// (server-side only).
type Authorization struct {
	RedirectTo string // IdP authorization_endpoint with query (client_id/redirect/scope/state/code_challenge)
	State      string // CSRF token (for log tracing)
}

// Authenticator performs the OIDC code flow with PKCE (ADR-058(b)).
type Authenticator struct {
	cfg         Config
	verifier    *gooidc.IDTokenVerifier
	oauth2      *oauth2.Config
	store       FlowStore
	httpClient  *http.Client // custom IdP CA (if TLSCA is set); otherwise nil → default
	aidClaim    string
	groupsClaim string
	logger      *slog.Logger
}

// New constructs the OIDC authenticator: discovery against the issuer (a
// network call to /.well-known/openid-configuration), building the verifier
// (JWKS, aud=client_id) and the oauth2.Config (endpoints from discovery). store
// is required (without it the flow is impossible — nowhere to keep the
// nonce/PKCE secrets).
//
// Validates security invariants (issuer HTTPS-only, client_id/redirect_url
// required) — defense-in-depth on top of the config layer's semantic validation.
func New(ctx context.Context, cfg Config, store FlowStore, logger *slog.Logger) (*Authenticator, error) {
	if !strings.HasPrefix(cfg.Issuer, "https://") {
		return nil, fmt.Errorf("auth/oidc: issuer %q must be https://", cfg.Issuer)
	}
	if cfg.ClientID == "" {
		return nil, errors.New("auth/oidc: client_id is required")
	}
	if cfg.RedirectURL == "" {
		return nil, errors.New("auth/oidc: redirect_url is required")
	}
	if store == nil {
		return nil, errors.New("auth/oidc: flow store is required (nonce/PKCE persistence)")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Custom IdP CA (optional): discovery, JWKS-fetch and token-exchange must
	// all go through it. go-oidc/oauth2 take the *http.Client from the context
	// (oidc.ClientContext / oauth2 context key).
	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	discoveryCtx := ctx
	if httpClient != nil {
		discoveryCtx = gooidc.ClientContext(ctx, httpClient)
	}

	provider, err := gooidc.NewProvider(discoveryCtx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth/oidc: discovery %q: %w", cfg.Issuer, err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes
	}
	oa := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  cfg.RedirectURL,
		Scopes:       scopes,
	}

	a := &Authenticator{
		cfg:         cfg,
		verifier:    provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth2:      oa,
		store:       store,
		httpClient:  httpClient,
		aidClaim:    orDefault(cfg.AIDClaim, defaultAIDClaim),
		groupsClaim: orDefault(cfg.GroupsClaim, defaultGroupsClaim),
		logger:      logger,
	}
	return a, nil
}

// buildHTTPClient builds an *http.Client with a custom IdP CA when TLSCA is
// set; otherwise it returns nil (the default transport with system CAs is used).
func buildHTTPClient(cfg Config) (*http.Client, error) {
	if len(cfg.TLSCA) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.TLSCA) {
		return nil, errors.New("auth/oidc: tls.ca_ref does not contain a valid PEM certificate")
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		},
	}, nil
}

// Method implements auth.Authenticator: returns "oidc" (ADR-058(a)).
func (a *Authenticator) Method() string { return "oidc" }

// BeginLogin starts the authorization-code flow: generates the state/nonce/
// PKCE verifier, saves {state→(nonce,verifier)} in FlowStore and returns an
// Authorization with the redirect URL (the S256 code_challenge goes into the
// URL, the verifier does not).
func (a *Authenticator) BeginLogin(ctx context.Context) (Authorization, error) {
	state, err := randToken()
	if err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: generate state: %w", err)
	}
	nonce, err := randToken()
	if err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: generate nonce: %w", err)
	}
	verifier := oauth2.GenerateVerifier()

	if err := a.store.Save(ctx, state, FlowState{Nonce: nonce, CodeVerifier: verifier}); err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: persist flow state: %w", err)
	}

	url := a.oauth2.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier), // PKCE S256 (mandatory, ADR-058)
	)
	return Authorization{RedirectTo: url, State: state}, nil
}

// CompleteLogin finishes the flow given (code, state): Consume state (CSRF +
// single-use), code-exchange with the PKCE verifier, id_token VALIDATION via
// the verifier (JWKS/iss/aud/exp), nonce check, claims extraction →
// auth.ExternalIdentity. Any failure surfaces as auth.ErrAuthFailed
// (anti-oracle); details go to the debug log without tokens.
func (a *Authenticator) CompleteLogin(ctx context.Context, code, state string) (auth.ExternalIdentity, error) {
	if code == "" || state == "" {
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 1. Consume state from the store: the sole anti-CSRF + single-use guard
	// (GETDEL). An unknown/already-consumed/expired state → deny.
	fs, err := a.store.Consume(ctx, state)
	if err != nil {
		a.debugFail("consume state", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	exchangeCtx := ctx
	if a.httpClient != nil {
		exchangeCtx = gooidc.ClientContext(ctx, a.httpClient)
	}

	// 2. Code-exchange with the PKCE verifier. Without the verifier
	// (perfect-CSRF / code interception) the IdP rejects the exchange —
	// PKCE enforcement.
	tok, err := a.oauth2.Exchange(exchangeCtx, code, oauth2.VerifierOption(fs.CodeVerifier))
	if err != nil {
		a.debugFail("code exchange", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 3. Extract id_token from the token-endpoint response.
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		a.logger.Debug("auth/oidc: token response has no id_token")
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 4. id_token VALIDATION: signature via JWKS, iss==issuer, aud==client_id,
	// exp/iat — all done by the go-oidc verifier.
	idToken, err := a.verifier.Verify(exchangeCtx, rawID)
	if err != nil {
		a.debugFail("verify id_token", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 5. Anti-replay: the nonce from id_token must match the stored one.
	if idToken.Nonce == "" || idToken.Nonce != fs.Nonce {
		a.logger.Debug("auth/oidc: nonce mismatch")
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 6. Extract claims → ExternalIdentity. Mapping onto AID+roles is
	// auth.Mapper's job.
	return a.identityFromClaims(idToken)
}

// identityFromClaims parses the id_token claims into auth.ExternalIdentity:
// AID from aid_claim, groups from groups_claim, optional email/
// preferred_username.
func (a *Authenticator) identityFromClaims(idToken *gooidc.IDToken) (auth.ExternalIdentity, error) {
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		a.debugFail("decode claims", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	aid := strings.ToLower(claimString(claims, a.aidClaim))
	if aid == "" {
		a.logger.Debug("auth/oidc: aid claim empty", slog.String("aid_claim", a.aidClaim))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	return auth.ExternalIdentity{
		Subject:  idToken.Subject,
		AID:      aid,
		Email:    claimString(claims, "email"),
		Username: claimString(claims, "preferred_username"),
		Groups:   claimStrings(claims, a.groupsClaim),
		Claims:   claims,
	}, nil
}

// debugFail logs the failure reason at debug level. It NEVER logs
// tokens/code/secrets — only the stage tag and the error text.
func (a *Authenticator) debugFail(stage string, err error) {
	a.logger.Debug("auth/oidc: authentication failed",
		slog.String("stage", stage), slog.Any("error", err))
}

// randToken returns a 256-bit cryptographically random token (state/nonce),
// base64url without padding (URL-safe for query/cookie).
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// claimString fetches a string claim; missing/not a string → "".
func claimString(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

// claimStrings fetches a claim as a list of strings. Supports []any (a JSON
// array, typical for groups) and a single string (some IdPs return a lone
// group as a scalar).
func claimStrings(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// compile-time assertion: *Authenticator implements auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)
