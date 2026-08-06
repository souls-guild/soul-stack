package handlers

// Guard tests for POST /v1/incarnations/resolve-name (NIM-331) at the handler
// layer. The composition itself and its agreement with the create path are pinned
// in the scenario package; what is at stake HERE is the disclosure boundary — a
// preview that answers "is this name free" is one careless branch away from being
// an existence oracle for names the caller could never create.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// composedName is what nameTemplateSnapshot's template yields for resolveInput.
const composedName = "cache-billing-inv-redis-sentinel"

func resolveInput() map[string]any {
	return map[string]any{"name": "cache", "project": "billing", "subproject": "inv"}
}

// newResolveHandler wires a handler over the templated snapshot with a scoper and
// a permission checker under the test's control — the two axes every case here
// varies.
func newResolveHandler(t *testing.T, db *fakeIncDB, scoper PurviewResolver, checker interface {
	Check(string, string, string, map[string]string) error
}) *IncarnationHandler {
	t.Helper()
	loader := &fakeLoader{localDir: nameTemplateSnapshot(t)}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true}, loader, nil, scoper, nil)
	h.SetPermissionChecker(checker)
	return h
}

func resolveName(t *testing.T, h *IncarnationHandler, req ResolveNameRequest) (ResolveNameResult, error) {
	t.Helper()
	claims := &jwt.Claims{Subject: "archon-alice"}
	return h.ResolveNameTyped(context.Background(), claims, req, h.GetInScopeFor(claims, "get"))
}

// notFoundDB — nothing holds any name.
func notFoundDB() *fakeIncDB {
	return &fakeIncDB{selectByNameRow: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
}

// TestResolveName_ComposesAndReportsFree — the ordinary answer the form draws:
// the name, its length against a server-sourced ceiling, and that it is free.
// max_length comes from the server on purpose — a form that restates 63 is a
// second copy of a rule, and copies drift.
func TestResolveName_ComposesAndReportsFree(t *testing.T) {
	h := newResolveHandler(t, notFoundDB(), unrestrictedScoper(), allowAllChecker{})

	res, err := resolveName(t, h, ResolveNameRequest{
		Service: "redis", CreateScenario: "create", Input: resolveInput(),
	})
	if err != nil {
		t.Fatalf("ResolveNameTyped: %v", err)
	}
	if !res.Composes || !res.Valid {
		t.Fatalf("expected a valid composed name, got %+v", res)
	}
	if res.Name != composedName {
		t.Errorf("Name = %q, want %q", res.Name, composedName)
	}
	if res.Length != len(composedName) {
		t.Errorf("Length = %d, want %d", res.Length, len(composedName))
	}
	if res.MaxLength != 63 {
		t.Errorf("MaxLength = %d, want the server-sourced ceiling 63", res.MaxLength)
	}
	if !res.Available {
		t.Error("no incarnation holds the name — Available must be true")
	}
}

// TestResolveName_TakenNamesHolderOnlyInScope is the disclosure boundary itself.
//
// "Taken" is told to anyone who gets this far: they hold incarnation.create over
// this very name (gate (b) passed), and the create would tell them with a 409
// anyway — withholding it would only replace a clear answer with a mystery. WHICH
// service holds it is narrower: naming the occupant of a name in a scope the
// caller cannot read turns a name they merely guessed into a report on someone
// else's estate (the "I can see it ⟺ I could have been given it" invariant of
// NIM-202/203).
func TestResolveName_TakenNamesHolderOnlyInScope(t *testing.T) {
	takenDB := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}

	cases := []struct {
		name       string
		scoper     PurviewResolver
		wantHolder string
	}{
		{"caller may read the occupant", unrestrictedScoper(), "redis"},
		// Scoped to a DIFFERENT incarnation: the composed name is outside what this
		// role may read, so the occupancy is reported bare.
		{"caller may not read the occupant", fakeIncScoper{incarnations: []string{"someone-elses"}}, ""},
		{"caller reads nothing at all", fakeIncScoper{empty: true}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResolveHandler(t, takenDB, tc.scoper, allowAllChecker{})

			res, err := resolveName(t, h, ResolveNameRequest{
				Service: "redis", CreateScenario: "create", Input: resolveInput(),
			})
			if err != nil {
				t.Fatalf("ResolveNameTyped: %v", err)
			}
			if res.Available {
				t.Error("the name is held — Available must be false for every caller who may create it")
			}
			if res.TakenByService != tc.wantHolder {
				t.Errorf("TakenByService = %q, want %q", res.TakenByService, tc.wantHolder)
			}
		})
	}
}

// TestResolveName_OutOfScopeNameIsRefusedNotAnswered — without gate (b) on the
// COMPOSED name the endpoint becomes the oracle by another door: compose anything,
// read its availability. A caller whose create would be refused gets the same
// refusal here, and no occupancy at all.
func TestResolveName_OutOfScopeNameIsRefusedNotAnswered(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			t.Error("occupancy must not be looked up for a name outside the caller's scope")
			return makeIncarnationRow(name)
		},
	}
	// A role scoped to a coven the request does not declare: gate (b) measures the
	// composed name and refuses.
	h := newResolveHandler(t, db, unrestrictedScoper(), scopedChecker{covens: []string{"other"}})

	res, err := resolveName(t, h, ResolveNameRequest{
		Service: "redis", CreateScenario: "create", Input: resolveInput(),
	})
	if err == nil {
		t.Fatalf("expected a refusal, got %+v", res)
	}
	var perr *problemError
	if !errors.As(err, &perr) {
		t.Fatalf("expected a *problemError, got %T", err)
	}
	if perr.Details.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403 — the same answer the create gives", perr.Details.Status)
	}
	if res.Available || res.TakenByService != "" || res.Name != "" {
		t.Errorf("a refusal must disclose nothing about the name, got %+v", res)
	}
}

// TestResolveName_UnfinishedInputIsAnAnswerNot422 — the operator is mid-typing,
// which is the NORMAL state of a live preview. A 422 per keystroke is not a
// preview, and a blank box with no explanation is the failure this endpoint was
// opened to remove.
func TestResolveName_UnfinishedInputIsAnAnswerNot422(t *testing.T) {
	h := newResolveHandler(t, notFoundDB(), unrestrictedScoper(), allowAllChecker{})

	res, err := resolveName(t, h, ResolveNameRequest{
		Service: "redis", CreateScenario: "create", Input: map[string]any{"name": "cache"},
	})
	if err != nil {
		t.Fatalf("an unfinished input is not an error: %v", err)
	}
	if !res.Composes {
		t.Fatal("the scenario composes — Composes must stay true while the input is incomplete")
	}
	if res.Valid {
		t.Fatalf("must not claim a name while components are missing, got %q", res.Name)
	}
	if res.InvalidReason == "" {
		t.Fatal("a blank preview with no reason is exactly what this endpoint removes")
	}
	if res.Available {
		t.Error("availability is meaningless without a name — must not read as free")
	}
}

// TestResolveName_UnregisteredService_422 — a service the registry does not know
// is the caller's mistake, not a preview state.
func TestResolveName_UnregisteredService_422(t *testing.T) {
	loader := &fakeLoader{localDir: nameTemplateSnapshot(t)}
	h := NewIncarnationHandler(notFoundDB(), nil, nil, &fakeResolver{ok: false}, loader, nil, unrestrictedScoper(), nil)
	h.SetPermissionChecker(allowAllChecker{})

	_, err := resolveName(t, h, ResolveNameRequest{
		Service: "redis", CreateScenario: "create", Input: resolveInput(),
	})
	var perr *problemError
	if !errors.As(err, &perr) || perr.Details.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an unregistered service, got %v", err)
	}
}

// TestIncarnation_Create_DuplicateNamesHolderInScope — under a template the
// operator never typed the colliding name; they typed the components it was
// composed from. A bare "already exists" names a string they have not seen and
// leaves them nothing to change, so the 409 names the holding service — under the
// same scope rule as the resolve.
func TestIncarnation_Create_DuplicateNamesHolderInScope(t *testing.T) {
	cases := []struct {
		name   string
		scoper PurviewResolver
		want   string
	}{
		{"caller may read the occupant", unrestrictedScoper(), "already exists (service redis)"},
		{"caller may not read the occupant", fakeIncScoper{empty: true}, "already exists"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeIncDB{
				insertRow: func() pgx.Row {
					return errRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "incarnation_pkey"}}
				},
				selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
			}
			loader := &fakeLoader{localDir: nameTemplateSnapshot(t)}
			h := NewIncarnationHandler(db, &fakeStarter{}, nil, &fakeResolver{ok: true}, loader, nil, tc.scoper, nil)
			h.SetPermissionChecker(allowAllChecker{})

			rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
			if rec.Code != http.StatusConflict {
				t.Fatalf("Code = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
			}
			var p problem.Details
			_ = json.NewDecoder(rec.Body).Decode(&p)
			if p.Detail != "incarnation "+composedName+" "+tc.want {
				t.Errorf("Detail = %q, want it to end with %q", p.Detail, tc.want)
			}
		})
	}
}
