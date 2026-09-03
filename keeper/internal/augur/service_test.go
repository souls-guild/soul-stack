package augur

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// covenSel — the terse spelling of the dimension most of these cases don't care
// about; the subject-specific cases build their selector inline.
func covenSel(c ...string) subject.Selector { return subject.Selector{Covens: c} }

func newSvc(t *testing.T, db ExecQueryRower) *Service {
	t.Helper()
	svc, err := NewService(ServiceDeps{Pool: db})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// --- CreateOmen: validation → ErrValidation ---

func TestService_CreateOmen_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		in   CreateOmenInput
	}{
		{"bad-name", CreateOmenInput{ID: "BAD..", SourceType: "vault", Endpoint: "e", AuthRef: "vault:s/p"}},
		{"bad-source", CreateOmenInput{ID: "x", SourceType: "redis", Endpoint: "e", AuthRef: "vault:s/p"}},
		{"empty-endpoint", CreateOmenInput{ID: "x", SourceType: "vault", Endpoint: "", AuthRef: "vault:s/p"}},
		{"bad-authref", CreateOmenInput{ID: "x", SourceType: "vault", Endpoint: "e", AuthRef: "plain"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := newSvc(t, &fakeDB{})
			_, err := svc.CreateOmen(context.Background(), c.in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestService_CreateOmen_HappyPath(t *testing.T) {
	db := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return staticRow{values: []any{testNow}} // RETURNING created_at
	}}
	svc := newSvc(t, db)
	o, err := svc.CreateOmen(context.Background(), CreateOmenInput{
		ID: "vault-prod", SourceType: "vault", Endpoint: "e", AuthRef: "vault:secret/k/x",
		CallerAID: ptr("archon-alice"),
	})
	if err != nil {
		t.Fatalf("CreateOmen: %v", err)
	}
	if o.ID != "vault-prod" || o.CreatedByAID == nil || *o.CreatedByAID != "archon-alice" {
		t.Errorf("omen = %+v", o)
	}
}

func TestService_CreateOmen_Duplicate(t *testing.T) {
	db := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "omens_pkey"}}
	}}
	svc := newSvc(t, db)
	_, err := svc.CreateOmen(context.Background(), CreateOmenInput{
		ID: "vault-prod", SourceType: "vault", Endpoint: "e", AuthRef: "vault:s/p",
	})
	if !errors.Is(err, ErrOmenAlreadyExists) {
		t.Fatalf("err = %v, want ErrOmenAlreadyExists", err)
	}
}

// --- CreateRite: validation → ErrValidation; not-found is propagated ---

func TestService_CreateRite_EmptyOmen(t *testing.T) {
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Subject: covenSel("web"), Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (empty omen)", err)
	}
}

func TestService_CreateRite_SubjectNotExactlyOne(t *testing.T) {
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen:    "vault-prod",
		Subject: subject.Selector{Covens: []string{"web"}, SIDs: []string{"h1"}},
		Allow:   json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (two dimensions)", err)
	}
}

// TestService_CreateRite_IncarnationMustExist pins the extra check an
// `incarnation=` subject gets on a GRANT: a mistyped service or name produces a
// Rite that grants nothing, and an operator who believes access is in place
// cannot tell that apart from a Rite that simply has not been used yet.
func TestService_CreateRite_IncarnationMustExist(t *testing.T) {
	// The existence probe is the FIRST QueryRow, before InsertRite's omen lookup.
	svc := newSvc(t, &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return staticRow{values: []any{false}}
	}})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen:    "vault-prod",
		Subject: subject.Selector{Service: "redis", Incarnation: "ghost"},
		Allow:   json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (incarnation does not exist)", err)
	}
	if !strings.Contains(err.Error(), "redis.ghost") {
		t.Errorf("err = %v, want the service.incarnation pair named", err)
	}
}

func TestService_CreateRite_OmenNotFound(t *testing.T) {
	// QueryRow default → ErrNoRows on Omen resolve inside InsertRite.
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "ghost", Subject: covenSel("web"), Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

func TestService_CreateRite_BadAllowShape(t *testing.T) {
	// vault-Omen, allow in prometheus form → InsertRite.ValidateAllow rejects;
	// Service maps to ErrValidation.
	svc := newSvc(t, insertRiteFake("vault"))
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "vault-prod", Subject: covenSel("web"), Allow: json.RawMessage(`{"queries":["up"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (allow shape)", err)
	}
}

func TestService_CreateRite_HappyPath(t *testing.T) {
	svc := newSvc(t, insertRiteFake("vault"))
	r, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "vault-prod", Subject: covenSel("web"), Allow: json.RawMessage(`{"paths":["secret/app"]}`),
		CallerAID: ptr("archon-alice"),
	})
	if err != nil {
		t.Fatalf("CreateRite: %v", err)
	}
	if r.ID != 42 || r.Omen != "vault-prod" {
		t.Errorf("rite = %+v", r)
	}
}

// --- Delete: not-found propagation ---

func TestService_DeleteOmen_NotFound(t *testing.T) {
	db := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	svc := newSvc(t, db)
	if err := svc.DeleteOmen(context.Background(), "ghost"); !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

func TestService_DeleteRite_NotFound(t *testing.T) {
	db := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	svc := newSvc(t, db)
	if err := svc.DeleteRite(context.Background(), 99); !errors.Is(err, ErrRiteNotFound) {
		t.Fatalf("err = %v, want ErrRiteNotFound", err)
	}
}
