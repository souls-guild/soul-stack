package augur

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func newSvc(t *testing.T, db ExecQueryRower) *Service {
	t.Helper()
	svc, err := NewService(ServiceDeps{Pool: db})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// --- CreateOmen: валидация → ErrValidation ---

func TestService_CreateOmen_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		in   CreateOmenInput
	}{
		{"bad-name", CreateOmenInput{Name: "BAD..", SourceType: "vault", Endpoint: "e", AuthRef: "vault:s/p"}},
		{"bad-source", CreateOmenInput{Name: "x", SourceType: "redis", Endpoint: "e", AuthRef: "vault:s/p"}},
		{"empty-endpoint", CreateOmenInput{Name: "x", SourceType: "vault", Endpoint: "", AuthRef: "vault:s/p"}},
		{"bad-authref", CreateOmenInput{Name: "x", SourceType: "vault", Endpoint: "e", AuthRef: "plain"}},
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
		Name: "vault-prod", SourceType: "vault", Endpoint: "e", AuthRef: "vault:secret/k/x",
		CallerAID: ptr("archon-alice"),
	})
	if err != nil {
		t.Fatalf("CreateOmen: %v", err)
	}
	if o.Name != "vault-prod" || o.CreatedByAID == nil || *o.CreatedByAID != "archon-alice" {
		t.Errorf("omen = %+v", o)
	}
}

func TestService_CreateOmen_Duplicate(t *testing.T) {
	db := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "omens_pkey"}}
	}}
	svc := newSvc(t, db)
	_, err := svc.CreateOmen(context.Background(), CreateOmenInput{
		Name: "vault-prod", SourceType: "vault", Endpoint: "e", AuthRef: "vault:s/p",
	})
	if !errors.Is(err, ErrOmenAlreadyExists) {
		t.Fatalf("err = %v, want ErrOmenAlreadyExists", err)
	}
}

// --- CreateRite: валидация → ErrValidation; not-found пробрасывается ---

func TestService_CreateRite_EmptyOmen(t *testing.T) {
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (empty omen)", err)
	}
}

func TestService_CreateRite_SubjectXOR(t *testing.T) {
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "vault-prod", Coven: ptr("web"), SID: ptr("h1"),
		Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (XOR)", err)
	}
}

func TestService_CreateRite_OmenNotFound(t *testing.T) {
	// QueryRow default → ErrNoRows на резолве Omen-а внутри InsertRite.
	svc := newSvc(t, &fakeDB{})
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "ghost", Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	if !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

func TestService_CreateRite_BadAllowShape(t *testing.T) {
	// vault-Omen, allow с prometheus-формой → InsertRite.ValidateAllow отвергает;
	// Service маппит в ErrValidation.
	svc := newSvc(t, insertRiteFake("vault"))
	_, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "vault-prod", Coven: ptr("web"), Allow: json.RawMessage(`{"queries":["up"]}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation (allow shape)", err)
	}
}

func TestService_CreateRite_HappyPath(t *testing.T) {
	svc := newSvc(t, insertRiteFake("vault"))
	r, err := svc.CreateRite(context.Background(), CreateRiteInput{
		Omen: "vault-prod", Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["secret/app"]}`),
		CallerAID: ptr("archon-alice"),
	})
	if err != nil {
		t.Fatalf("CreateRite: %v", err)
	}
	if r.ID != 42 || r.Omen != "vault-prod" {
		t.Errorf("rite = %+v", r)
	}
}

// --- Delete: not-found проброс ---

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
