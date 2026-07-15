package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// --- mock IdP: discovery + JWKS + token-endpoint, signs id_token with RSA ---

const (
	testClientID = "soul-keeper"
	testKID      = "test-key-1"
)

// mockIdP is a minimal OIDC provider on httptest for unit tests. It holds an
// RSA key, serves discovery/JWKS, and /token returns a pre-built id_token
// (controlled via the claims/sign-key fields — for negative scenarios).
type mockIdP struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	signKey *rsa.PrivateKey // key used to SIGN the id_token (usually == key; a foreign one for the bad-sig test)

	// idTokenClaims are the claims that will land in the id_token on the next
	// /token call. The test sets them before CompleteLogin.
	mu            sync.Mutex
	idTokenClaims map[string]any
	// gotCodeVerifier is the code_verifier received on the last /token call
	// (used to check PKCE enforcement: without it the test emulates a denial).
	gotCodeVerifier string
	// requirePKCE — if true, /token returns an error when code_verifier is
	// missing.
	requirePKCE bool
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	m := &mockIdP{key: key, signKey: key, requirePKCE: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 m.issuer(),
			"authorization_endpoint": m.issuer() + "/authorize",
			"token_endpoint":         m.issuer() + "/token",
			"jwks_uri":               m.issuer() + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		jwk := jose.JSONWebKey{Key: &m.key.PublicKey, KeyID: testKID, Algorithm: "RS256", Use: "sig"}
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		m.gotCodeVerifier = r.Form.Get("code_verifier")
		claims := m.idTokenClaims
		signKey := m.signKey
		requirePKCE := m.requirePKCE
		m.mu.Unlock()

		if requirePKCE && m.gotCodeVerifier == "" {
			// PKCE-enforcing IdP: without the verifier, the exchange is impossible.
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "invalid_grant", "error_description": "PKCE verifier required"})
			return
		}

		idToken := signIDToken(m.issuer(), claims, signKey)
		writeJSON(w, map[string]any{
			"access_token": "dummy-access",
			"token_type":   "Bearer",
			"id_token":     idToken,
			"expires_in":   3600,
		})
	})
	// TLS server: the issuer must be https:// (New's security invariant).
	m.srv = httptest.NewTLSServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIdP) issuer() string { return m.srv.URL }

// caPEM is the TLS server's leaf certificate in PEM (for Config.TLSCA →
// httpClient trusts the mock IdP). httptest signs the leaf with this very
// certificate (self-signed), so it also works as the CA for the test.
func (m *mockIdP) caPEM() []byte {
	cert := m.srv.Certificate()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func (m *mockIdP) setClaims(c map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idTokenClaims = c
}

func (m *mockIdP) setSignKey(k *rsa.PrivateKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signKey = k
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// signIDToken signs claims with the RS256 signKey and kid=testKID.
func signIDToken(issuer string, claims map[string]any, signKey *rsa.PrivateKey) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: signKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKID),
	)
	if err != nil {
		panic(err)
	}
	full := map[string]any{"iss": issuer, "aud": testClientID}
	for k, v := range claims {
		full[k] = v
	}
	payload, _ := json.Marshal(full)
	obj, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return s
}

// fakeFlowStore is an in-memory FlowStore for unit tests (no Redis). Consume
// is single-use (deletes the entry) to reproduce anti-replay behavior.
type fakeFlowStore struct {
	mu sync.Mutex
	m  map[string]FlowState
}

func newFakeFlowStore() *fakeFlowStore { return &fakeFlowStore{m: map[string]FlowState{}} }

func (s *fakeFlowStore) Save(_ context.Context, state string, fs FlowState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[state] = fs
	return nil
}

func (s *fakeFlowStore) Consume(_ context.Context, state string) (FlowState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.m[state]
	if !ok {
		return FlowState{}, ErrFlowNotFound
	}
	delete(s.m, state)
	return fs, nil
}

// newTestAuthenticator spins up an Authenticator against the mock IdP.
func newTestAuthenticator(t *testing.T, idp *mockIdP, store FlowStore) *Authenticator {
	t.Helper()
	a, err := New(context.Background(), Config{
		Issuer:      idp.issuer(),
		ClientID:    testClientID,
		RedirectURL: "https://keeper.example.com/auth/oidc/callback",
		Scopes:      []string{"openid", "groups"},
		TLSCA:       idp.caPEM(),
		AIDClaim:    "preferred_username",
		GroupsClaim: "groups",
	}, store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// stateFromURL extracts state from the authorization URL.
func stateFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	return u.Query().Get("state")
}

// --- (0) New validates invariants ---

func TestNew_RejectsNonHTTPSIssuer(t *testing.T) {
	_, err := New(context.Background(), Config{
		Issuer: "http://idp.example.com", ClientID: "x", RedirectURL: "https://k/cb",
	}, newFakeFlowStore(), nil)
	if err == nil {
		t.Fatal("expected New to reject http:// issuer")
	}
}

func TestNew_RequiresStore(t *testing.T) {
	idp := newMockIdP(t)
	_, err := New(context.Background(), Config{
		Issuer: idp.issuer(), ClientID: testClientID, RedirectURL: "https://k/cb",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected New to require non-nil flow store")
	}
}

// --- (1) BeginLogin: PKCE S256 challenge + state in the URL, verifier stays server-side ---

func TestBeginLogin_EmitsPKCEChallengeAndState(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, err := a.BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, _ := url.Parse(authz.RedirectTo)
	q := u.Query()
	if q.Get("code_challenge") == "" {
		t.Error("authorization URL must carry PKCE code_challenge")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("state") == "" {
		t.Error("authorization URL must carry state")
	}
	if q.Get("nonce") == "" {
		t.Error("authorization URL must carry nonce")
	}
	// code_verifier stays server-side (NOT in the URL).
	if strings.Contains(authz.RedirectTo, store.m[authz.State].CodeVerifier) {
		t.Error("code_verifier must NOT appear in redirect URL")
	}
	if store.m[authz.State].CodeVerifier == "" {
		t.Error("flow store must hold the code_verifier")
	}
}

// --- (2) happy path: a valid id_token + nonce → identity ---

func TestCompleteLogin_Happy(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	state := authz.State
	idp.setClaims(map[string]any{
		"sub":                "subject-123",
		"preferred_username": "Alice",
		"email":              "alice@example.com",
		"groups":             []any{"ops", "admins"},
		"nonce":              store.m[state].Nonce,
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
	})

	ext, err := a.CompleteLogin(context.Background(), "auth-code", state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if ext.AID != "alice" {
		t.Errorf("AID = %q, want alice (lowercased preferred_username)", ext.AID)
	}
	if ext.Subject != "subject-123" {
		t.Errorf("Subject = %q, want subject-123", ext.Subject)
	}
	if len(ext.Groups) != 2 || ext.Groups[0] != "ops" {
		t.Errorf("Groups = %v, want [ops admins]", ext.Groups)
	}
	// PKCE enforcement: the IdP received code_verifier.
	if idp.gotCodeVerifier == "" {
		t.Error("token exchange must send PKCE code_verifier")
	}
}

// --- (3) ★ PKCE-enforced: code-exchange WITHOUT the verifier is rejected by the IdP ---

func TestCompleteLogin_PKCEEnforced(t *testing.T) {
	idp := newMockIdP(t) // requirePKCE=true
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	// Replace the verifier with an empty one in the store → the exchange will
	// go out without code_verifier → the PKCE IdP rejects it (invalid_grant).
	store.mu.Lock()
	fs := store.m[authz.State]
	fs.CodeVerifier = ""
	store.m[authz.State] = fs
	store.mu.Unlock()

	idp.setClaims(map[string]any{"sub": "x", "preferred_username": "alice", "nonce": authz.State})
	_, err := a.CompleteLogin(context.Background(), "auth-code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("exchange without PKCE verifier must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (4) ★ state-mismatch (unknown/replayed state) → deny ---

func TestCompleteLogin_StateMismatch(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	_, _ = a.BeginLogin(context.Background())
	// a state that is not in the store (CSRF / forgery).
	_, err := a.CompleteLogin(context.Background(), "auth-code", "forged-state")
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("unknown state must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (4b) ★ single-use: a repeated callback with the same state → deny (replay) ---

func TestCompleteLogin_StateSingleUse(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	idp.setClaims(map[string]any{
		"sub": "s", "preferred_username": "alice",
		"nonce": store.m[authz.State].Nonce,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.CompleteLogin(context.Background(), "code", authz.State); err != nil {
		t.Fatalf("first CompleteLogin: %v", err)
	}
	// state has been consumed (single-use Consume) → the repeat is rejected.
	if _, err := a.CompleteLogin(context.Background(), "code", authz.State); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("replayed state must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (5) ★ nonce-mismatch → deny (anti-replay id_token) ---

func TestCompleteLogin_NonceMismatch(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	idp.setClaims(map[string]any{
		"sub": "s", "preferred_username": "alice",
		"nonce": "WRONG-NONCE", // does not match the stored one
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	_, err := a.CompleteLogin(context.Background(), "code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("nonce mismatch must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (6) ★ id_token: invalid signature → deny ---

func TestCompleteLogin_BadSignature(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	// id_token is signed with a FOREIGN key (not the one in JWKS).
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp.setSignKey(otherKey)
	idp.setClaims(map[string]any{
		"sub": "s", "preferred_username": "alice",
		"nonce": store.m[authz.State].Nonce,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	_, err := a.CompleteLogin(context.Background(), "code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("bad id_token signature must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (7) ★ id_token: expired → deny ---

func TestCompleteLogin_Expired(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	idp.setClaims(map[string]any{
		"sub": "s", "preferred_username": "alice",
		"nonce": store.m[authz.State].Nonce,
		"exp":   time.Now().Add(-time.Hour).Unix(), // expired
		"iat":   time.Now().Add(-2 * time.Hour).Unix(),
	})
	_, err := a.CompleteLogin(context.Background(), "code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("expired id_token must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (8) ★ id_token: wrong aud → deny ---

func TestCompleteLogin_WrongAudience(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	idp.setClaims(map[string]any{
		"sub": "s", "preferred_username": "alice",
		"aud":   "some-other-client", // != testClientID
		"nonce": store.m[authz.State].Nonce,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	_, err := a.CompleteLogin(context.Background(), "code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("wrong aud must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (9) empty code/state short-circuit → deny ---

func TestCompleteLogin_EmptyInputs(t *testing.T) {
	idp := newMockIdP(t)
	a := newTestAuthenticator(t, idp, newFakeFlowStore())
	if _, err := a.CompleteLogin(context.Background(), "", "state"); !errors.Is(err, auth.ErrAuthFailed) {
		t.Errorf("empty code must fail with ErrAuthFailed, got %v", err)
	}
	if _, err := a.CompleteLogin(context.Background(), "code", ""); !errors.Is(err, auth.ErrAuthFailed) {
		t.Errorf("empty state must fail with ErrAuthFailed, got %v", err)
	}
}

// --- (10) empty aid claim → deny ---

func TestCompleteLogin_EmptyAIDClaim(t *testing.T) {
	idp := newMockIdP(t)
	store := newFakeFlowStore()
	a := newTestAuthenticator(t, idp, store)

	authz, _ := a.BeginLogin(context.Background())
	idp.setClaims(map[string]any{
		"sub":   "s",
		"nonce": store.m[authz.State].Nonce,
		"exp":   time.Now().Add(time.Hour).Unix(),
		// preferred_username (aid_claim) is absent → AID is empty.
	})
	_, err := a.CompleteLogin(context.Background(), "code", authz.State)
	if !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("empty aid claim must fail with ErrAuthFailed, got %v", err)
	}
}
