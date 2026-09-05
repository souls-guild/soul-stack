package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

func TestRolePresent_CreatesWhenAbsent(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).role(), "present", withParams(map[string]any{
		"name":          "app_rw",
		"role_password": secretRolePass,
	}))

	assertOutcome(t, stream, true, false)
	if len(s.execed) != 1 {
		t.Fatalf("expected one statement, got %q", execedStatements(s))
	}
	stmt := s.execed[0]
	if !strings.HasPrefix(stmt.stmt, "CREATE ROLE app_rw ") {
		t.Fatalf("expected CREATE ROLE, got %q", stmt.stmt)
	}
	if !strings.Contains(stmt.stmt, "LOGIN = true") || !strings.Contains(stmt.stmt, "SUPERUSER = false") {
		t.Errorf("defaults not applied (login true, superuser false): %q", stmt.stmt)
	}
	// ★ The password is a QUOTED LITERAL, not a bound argument, and that is forced by
	// the driver: gocql prepares only DML, so a `?` here would reach the server with
	// nothing bound to it and the statement would be a syntax error (cql.go). Asserting
	// the literal is asserting the thing that actually works on a cluster — the
	// previous version of this test asserted the plugin's intent and would have passed
	// against a statement no Cassandra accepts.
	if len(stmt.args) != 0 {
		t.Errorf("nothing may be bound to a CREATE ROLE — the driver discards it, args = %v", stmt.args)
	}
	if !strings.Contains(stmt.stmt, "PASSWORD = '"+secretRolePass+"'") {
		t.Errorf("the role password must be a quoted literal in the statement: %q", stmt.stmt)
	}
	assertNoSecretInStatementText(t, s)
	assertNoSecretInEvents(t, stream)
}

// TestRolePresent_EscapesAQuoteInThePassword — a password carrying a single quote
// must not be able to end the literal early. Doubling is the whole CQL escape rule
// (a backslash is an ordinary character there), so this is the complete case.
func TestRolePresent_EscapesAQuoteInThePassword(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).role(), "present", withParams(map[string]any{
		"name": "app_rw", "role_password": "pa'ss' OR TRUE--",
	}))

	stmt := s.execed[0].stmt
	if !strings.Contains(stmt, "PASSWORD = 'pa''ss'' OR TRUE--'") {
		t.Errorf("the quote was not doubled, so the literal could end early: %q", stmt)
	}
}

// TestRolePresent_RefusesAControlCharacterInThePassword — refused on BOTH phases,
// because a runner need not call Validate.
func TestRolePresent_RefusesAControlCharacterInThePassword(t *testing.T) {
	params := withParams(map[string]any{"name": "app_rw", "role_password": "bad\npassword"})

	reply, _ := newModule(&fakeSession{}).role().Validate(context.Background(),
		&pluginv1.ValidateRequest{State: "present", Params: mustStruct(t, params)})
	if reply.GetOk() {
		t.Error("Validate accepted a control character in the password")
	}

	s := &fakeSession{}
	stream := apply(t, newModule(s).role(), "present", params)
	assertOutcome(t, stream, false, true)
	if len(s.execed) != 0 {
		t.Errorf("nothing may be issued after the refusal, got %q", execedStatements(s))
	}
}

func TestRolePresent_CreatesWithoutPasswordWhenNoneDeclared(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).role(), "present", withParams(map[string]any{"name": "reader", "login": false}))

	stmt := s.execed[0]
	if strings.Contains(stmt.stmt, "PASSWORD") {
		t.Errorf("a role declared with no password must not get a PASSWORD clause: %q", stmt.stmt)
	}
	if len(stmt.args) != 0 {
		t.Errorf("nothing to bind, got args = %v", stmt.args)
	}
}

func TestRolePresent_NoOpWhenAttributesMatch(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return roleRows(true, false), nil
	}}
	stream := apply(t, newModule(s).role(), "present", withParams(map[string]any{
		"name":          "app_rw",
		"role_password": secretRolePass,
	}))

	e := assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("a converged role must issue no statement, got %q", execedStatements(s))
	}
	// The no-op is only honest if it admits what it did not check.
	if !strings.Contains(e.GetMessage(), "password is not compared") {
		t.Errorf("the no-op must say the password was not compared, got %q", e.GetMessage())
	}
}

// TestRolePresent_AltersAttributesWithoutTouchingThePassword is the guard on the
// decision that a password is set only at CREATE. Re-applying it on every run would
// report changed forever — a state that never converges — because a salted hash
// cannot be compared with the declared password.
func TestRolePresent_AltersAttributesWithoutTouchingThePassword(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return roleRows(false, false), nil // the live role cannot log in
	}}
	stream := apply(t, newModule(s).role(), "present", withParams(map[string]any{
		"name":          "app_rw",
		"role_password": secretRolePass,
	}))

	assertOutcome(t, stream, true, false)
	stmt := s.execed[0]
	if !strings.HasPrefix(stmt.stmt, "ALTER ROLE app_rw ") {
		t.Fatalf("expected ALTER ROLE, got %q", stmt.stmt)
	}
	if strings.Contains(stmt.stmt, "PASSWORD") || len(stmt.args) != 0 {
		t.Errorf("the ALTER path must not carry the password: stmt %q args %v", stmt.stmt, stmt.args)
	}
	assertNoSecretInStatementText(t, s)
	assertNoSecretInEvents(t, stream)
}

// TestRole_DoesNotWaitForSchemaAgreement — a role is a ROW in system_auth, not a
// schema change. Waiting here would wait for something that already held.
func TestRole_DoesNotWaitForSchemaAgreement(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).role(), "present", withParams(map[string]any{"name": "app_rw"}))
	if s.agreed != 0 {
		t.Errorf("role DDL must not wait for schema agreement, waited %d times", s.agreed)
	}
}

func TestRoleAbsent_DropsWhenPresent(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return roleRows(true, false), nil
	}}
	stream := apply(t, newModule(s).role(), "absent", withParams(map[string]any{"name": "app_rw"}))

	assertOutcome(t, stream, true, false)
	if stmts := execedStatements(s); len(stmts) != 1 || stmts[0] != "DROP ROLE app_rw" {
		t.Fatalf("expected DROP ROLE app_rw, got %q", stmts)
	}
}

func TestRoleAbsent_NoOpWhenAlreadyGone(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).role(), "absent", withParams(map[string]any{"name": "app_rw"}))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("dropping an absent role must issue nothing, got %q", execedStatements(s))
	}
}

// TestRolePresent_RefusesAPasswordOnARoleThatCannotLogIn — accepting the pair would
// leave an author believing a credential exists that does not.
func TestRolePresent_RefusesAPasswordOnARoleThatCannotLogIn(t *testing.T) {
	errs := validateRolePresent(mustStruct(t, withParams(map[string]any{
		"name":          "reader",
		"login":         false,
		"role_password": secretRolePass,
	})).GetFields())

	if len(errs) == 0 {
		t.Fatal("expected a refusal for login=false with a password")
	}
	if !strings.Contains(strings.Join(errs, "; "), "has no password") {
		t.Errorf("refusal does not explain itself: %q", errs)
	}
	for _, e := range errs {
		if strings.Contains(e, secretRolePass) {
			t.Errorf("the refusal leaked the password: %q", e)
		}
	}
}
