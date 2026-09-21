//go:build integration

package bootstraptoken

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// The recovery path (NIM-865, docs/adr/0090-bootstrap-reply-loss-recovery.md)
// is one SQL conjunction across three tables, so it is guarded here against a
// real Postgres and not against a fake pool: every condition below is a WHERE
// clause, and a fake that replays a hardcoded row would answer the same whether
// the clause is there or not.

const (
	recoverySID = "lost-reply.example.com"
	recoveryKID = "keeper-1"
	// Two distinct 64-lower-hex values standing for two keypairs. The real
	// fingerprint is SHA-256 over a SubjectPublicKeyInfo; nothing here needs it
	// to be one, only that the two differ.
	fpHost     = "1111111111111111111111111111111111111111111111111111111111111111"
	fpAttacker = "2222222222222222222222222222222222222222222222222222222222222222"
)

// burnedTokenWithSeed reproduces the defect's exact aftermath: the Keeper signed
// a certificate, burned the token and committed — and the reply never arrived,
// so the host holds nothing and has never opened a stream.
func burnedTokenWithSeed(t *testing.T) (hash string) {
	t.Helper()
	ctx := context.Background()
	resetAll(t)
	seedSoul(t, recoverySID)

	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := Insert(ctx, integrationPool, recoverySID, tok.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := Burn(ctx, integrationPool, tok.Hash(), recoverySID, recoveryKID); err != nil {
		t.Fatalf("Burn: %v", err)
	}
	kid := recoveryKID
	seed := &soulseed.SoulSeed{
		SID:          recoverySID,
		Fingerprint:  fpHost,
		SerialNumber: "serial-1",
		ExpiresAt:    time.Now().UTC().Add(720 * time.Hour),
		IssuedByKID:  &kid,
		Status:       soulseed.StatusActive,
	}
	if err := soulseed.Insert(ctx, integrationPool, seed); err != nil {
		t.Fatalf("soulseed.Insert: %v", err)
	}
	return tok.Hash()
}

func markSoulSeen(t *testing.T, sid string) {
	t.Helper()
	tag, err := integrationPool.Exec(context.Background(),
		`UPDATE souls SET last_seen_at = NOW() WHERE sid = $1`, sid)
	if err != nil {
		t.Fatalf("mark seen: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("mark seen: affected %d rows, want 1", tag.RowsAffected())
	}
}

// The defect itself: a host whose reply was lost completes its onboarding.
func TestIntegration_RedeemAgain_RecoversLostReply(t *testing.T) {
	hash := burnedTokenWithSeed(t)
	ctx := context.Background()

	// Burn still refuses it — the token IS spent, and this path does not undo
	// that. What it grants is delivery to the binding the burn already made.
	if _, err := Burn(ctx, integrationPool, hash, recoverySID, recoveryKID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Burn on a burned token = %v, want ErrTokenInvalid", err)
	}

	id, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost)
	if err != nil {
		t.Fatalf("RedeemAgain: %v", err)
	}
	if id == "" {
		t.Error("RedeemAgain returned an empty token_id")
	}

	// Still recoverable until the host actually appears: a second lost reply is
	// the same situation as the first and must not be the end of the road.
	if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost); err != nil {
		t.Errorf("second RedeemAgain: %v, want success", err)
	}
}

// ★ The one that outranks the rest. Fixing the lost reply while leaving this
// open would trade an inconvenience for a hole: the token plaintext may still
// be sitting in /etc/soul/token on a host that is up and working.
func TestIntegration_RedeemAgain_RefusedOnceTheHostHasConnected(t *testing.T) {
	hash := burnedTokenWithSeed(t)
	ctx := context.Background()

	markSoulSeen(t, recoverySID)

	if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("RedeemAgain after first contact = %v, want ErrTokenInvalid", err)
	}
}

// The anti-replay property itself, stated where it actually lives: what may not
// be repeated is binding the SID to a key. An attacker presenting a captured
// token carries their own key and is refused exactly as a second Burn refuses
// them.
func TestIntegration_RedeemAgain_RefusesADifferentKey(t *testing.T) {
	hash := burnedTokenWithSeed(t)
	ctx := context.Background()

	if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpAttacker); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("RedeemAgain with a foreign key = %v, want ErrTokenInvalid", err)
	}
	// And the legitimate key still works afterwards — a refused attempt must
	// not spend the recovery it was trying to steal.
	if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost); err != nil {
		t.Errorf("RedeemAgain with the bound key after a refused one: %v, want success", err)
	}
}

// A marker records an invalidation, not a presentation: no Soul ever held this
// token, so there is no lost reply to recover and an operator who killed it
// with force-reissue meant it.
func TestIntegration_RedeemAgain_RefusesASystemMarkedBurn(t *testing.T) {
	ctx := context.Background()

	for _, marker := range SystemKIDs() {
		t.Run(marker, func(t *testing.T) {
			resetAll(t)
			seedSoul(t, recoverySID)
			tok, err := Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if _, err := Insert(ctx, integrationPool, recoverySID, tok.Hash(), 24*time.Hour, nil); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if _, _, err := ExpireActiveBySID(ctx, integrationPool, recoverySID, marker); err != nil {
				t.Fatalf("ExpireActiveBySID: %v", err)
			}
			kid := recoveryKID
			if err := soulseed.Insert(ctx, integrationPool, &soulseed.SoulSeed{
				SID: recoverySID, Fingerprint: fpHost, SerialNumber: "serial-1",
				ExpiresAt:   time.Now().UTC().Add(720 * time.Hour),
				IssuedByKID: &kid, Status: soulseed.StatusActive,
			}); err != nil {
				t.Fatalf("soulseed.Insert: %v", err)
			}

			if _, err := RedeemAgain(ctx, integrationPool, tok.Hash(), recoverySID, recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("RedeemAgain over a %s burn = %v, want ErrTokenInvalid", marker, err)
			}
		})
	}
}

// Recovery never outlives the token's own TTL, and never applies to a token
// that was never burned — that one belongs to Burn, and admitting it here would
// be a second first-presentation path with none of Burn's guarantees.
func TestIntegration_RedeemAgain_RefusesExpiredUnburnedAndForeignSID(t *testing.T) {
	ctx := context.Background()

	t.Run("expired", func(t *testing.T) {
		hash := burnedTokenWithSeed(t)
		// created_at moves with it: `bootstrap_tokens_expires_after_created`
		// rejects an expiry that predates issuance, so backdating only the one
		// column tests nothing and fails here.
		if _, err := integrationPool.Exec(ctx,
			`UPDATE bootstrap_tokens
			    SET created_at = NOW() - interval '2 hours',
			        expires_at = NOW() - interval '1 second'
			  WHERE token_hash = $1`,
			hash); err != nil {
			t.Fatalf("expire: %v", err)
		}
		if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("RedeemAgain on an expired token = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("never burned", func(t *testing.T) {
		resetAll(t)
		seedSoul(t, recoverySID)
		tok, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, err := Insert(ctx, integrationPool, recoverySID, tok.Hash(), 24*time.Hour, nil); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		kid := recoveryKID
		if err := soulseed.Insert(ctx, integrationPool, &soulseed.SoulSeed{
			SID: recoverySID, Fingerprint: fpHost, SerialNumber: "serial-1",
			ExpiresAt:   time.Now().UTC().Add(720 * time.Hour),
			IssuedByKID: &kid, Status: soulseed.StatusActive,
		}); err != nil {
			t.Fatalf("soulseed.Insert: %v", err)
		}
		if _, err := RedeemAgain(ctx, integrationPool, tok.Hash(), recoverySID, recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("RedeemAgain on an unburned token = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("foreign sid", func(t *testing.T) {
		hash := burnedTokenWithSeed(t)
		seedSoul(t, "other.example.com")
		if _, err := RedeemAgain(ctx, integrationPool, hash, "other.example.com", recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("RedeemAgain under a foreign sid = %v, want ErrTokenInvalid", err)
		}
	})
}

// A seed that is no longer active is no longer a binding this token may be
// redeemed against — a revoked host must not be able to pull a fresh
// certificate out of the token that first onboarded it.
func TestIntegration_RedeemAgain_RefusesWhenTheSeedIsNotActive(t *testing.T) {
	hash := burnedTokenWithSeed(t)
	ctx := context.Background()

	if _, err := integrationPool.Exec(ctx,
		`UPDATE soul_seeds SET status = 'revoked' WHERE sid = $1`, recoverySID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := RedeemAgain(ctx, integrationPool, hash, recoverySID, recoveryKID, fpHost); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("RedeemAgain against a revoked seed = %v, want ErrTokenInvalid", err)
	}
}

// TestSystemKIDs_CoversEveryMarker reads the package's own source rather than
// restating the list, so declaring a sixth SystemKID* constant and forgetting
// [SystemKIDs] fails here instead of quietly opening the recovery path to it.
// A list checked against a copy of itself would pass in exactly that case.
func TestSystemKIDs_CoversEveryMarker(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}

	declared := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "SystemKID") {
							continue
						}
						// A marker whose value this scan cannot read is the
						// case the scan exists for, so it FAILS here instead of
						// being skipped. Skipping is how a drift guard quietly
						// stops guarding: `const SystemKIDNew = prefix + "new"`
						// is not a BasicLit, and an implicit-value spec has no
						// Values at all — either would have slipped through and
						// left RedeemAgain accepting that marker's burns.
						if i >= len(vs.Values) {
							t.Errorf("%s has no explicit value; keep every SystemKID* a plain string literal so this guard can read it", name.Name)
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							t.Errorf("%s is not a plain string literal; keep every SystemKID* one so this guard can read it", name.Name)
							continue
						}
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", name.Name, err)
						}
						declared[name.Name] = v
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no SystemKID* constants — the scan is broken, not the code")
	}

	listed := map[string]bool{}
	for _, v := range SystemKIDs() {
		listed[v] = true
	}
	var missing []string
	for name, v := range declared {
		if !listed[v] {
			missing = append(missing, name+"="+v)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("SystemKIDs() omits %v — RedeemAgain would accept a burn these markers wrote", missing)
	}
	// Both sides counted as sets of VALUES: comparing a value-set against a
	// name-map would report a spurious failure the day two markers deliberately
	// share a string, and would miss a duplicated entry in SystemKIDs().
	distinct := map[string]bool{}
	for _, v := range declared {
		distinct[v] = true
	}
	if len(listed) != len(distinct) {
		t.Errorf("SystemKIDs() yields %d distinct values for %d declared ones", len(listed), len(distinct))
	}
}
