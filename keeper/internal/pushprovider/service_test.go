package pushprovider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingPublisher is a Publisher mock that records calls.
type recordingPublisher struct {
	calls []string
	err   error
}

func (p *recordingPublisher) PublishPushProvidersChanged(_ context.Context, providerName string) error {
	p.calls = append(p.calls, providerName)
	return p.err
}

func TestService_Create_PublishesInvalidate(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeDB{
		rowFunc: func() pgx.Row { return staticRow{values: []any{now, now}} },
	}
	pub := &recordingPublisher{}
	s, err := NewService(ServiceDeps{Pool: f, Publisher: pub})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = s.Create(context.Background(), CreateInput{
		Name:      "vault-bastion",
		Params:    map[string]any{"vault_addr": "https://vault.example.com"},
		CallerAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(pub.calls) != 1 || pub.calls[0] != "vault-bastion" {
		t.Errorf("publish calls = %v", pub.calls)
	}
}

func TestService_Create_RejectsPlainSensitive(t *testing.T) {
	s, _ := NewService(ServiceDeps{Pool: &fakeDB{}})
	cases := []map[string]any{
		{"secret_id": "plain-string"},
		{"token": "abc"},
		{"password": "qwerty"},
		{"private_key": "-----BEGIN OPENSSH KEY-----"},
		{"token": 123}, // non-string sensitive
	}
	for _, params := range cases {
		_, err := s.Create(context.Background(), CreateInput{
			Name: "vault", Params: params, CallerAID: "archon-alice",
		})
		if !errors.Is(err, ErrSensitiveNotVaultRef) {
			t.Errorf("params=%v: err = %v, want ErrSensitiveNotVaultRef", params, err)
		}
	}
}

func TestService_Create_AcceptsVaultRefForSensitive(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeDB{
		rowFunc: func() pgx.Row { return staticRow{values: []any{now, now}} },
	}
	s, _ := NewService(ServiceDeps{Pool: f})
	_, err := s.Create(context.Background(), CreateInput{
		Name: "vault-bastion",
		Params: map[string]any{
			"vault_addr": "https://vault.example.com", // plain ok (not sensitive)
			"role":       "keeper",                    // plain ok
			"secret_id":  "vault:secret/keeper/vault-approle#secret_id",
		},
		CallerAID: "archon-alice",
	})
	if err != nil {
		t.Errorf("Create with vault-ref secret: %v", err)
	}
}

func TestService_Create_RejectsInvalidName(t *testing.T) {
	s, _ := NewService(ServiceDeps{Pool: &fakeDB{}})
	_, err := s.Create(context.Background(), CreateInput{
		Name: "1bad-name", CallerAID: "archon-alice",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("err = %v, want invalid name", err)
	}
}

func TestService_Create_RejectsEmptyCallerAID(t *testing.T) {
	s, _ := NewService(ServiceDeps{Pool: &fakeDB{}})
	_, err := s.Create(context.Background(), CreateInput{Name: "vault", CallerAID: ""})
	if err == nil {
		t.Error("Create(empty caller): no error")
	}
}

func TestService_Update_PublishesInvalidate(t *testing.T) {
	now := time.Now().UTC()
	updatedBy := "archon-bob"
	selectCalled := 0
	f := &fakeDB{
		execTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	// QueryRow for select-after-update.
	f.rowFunc = func() pgx.Row {
		selectCalled++
		return staticRow{values: []any{
			"vault", []byte(`{"role":"keeper"}`), now, now, "archon-alice", &updatedBy,
		}}
	}
	pub := &recordingPublisher{}
	s, _ := NewService(ServiceDeps{Pool: f, Publisher: pub})
	_, err := s.Update(context.Background(), UpdateInput{
		Name: "vault", Params: map[string]any{"role": "keeper"}, CallerAID: "archon-bob",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(pub.calls) != 1 || pub.calls[0] != "vault" {
		t.Errorf("publish calls = %v", pub.calls)
	}
	if selectCalled != 1 {
		t.Errorf("select-after-update calls = %d", selectCalled)
	}
}

func TestService_Update_NotFoundDoesNotPublish(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	pub := &recordingPublisher{}
	s, _ := NewService(ServiceDeps{Pool: f, Publisher: pub})
	_, err := s.Update(context.Background(), UpdateInput{
		Name: "missing", CallerAID: "archon-bob",
	})
	if !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("err = %v", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("publish unexpectedly called: %v", pub.calls)
	}
}

func TestService_Delete_PublishesInvalidate(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	pub := &recordingPublisher{}
	s, _ := NewService(ServiceDeps{Pool: f, Publisher: pub})
	if err := s.Delete(context.Background(), "vault"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(pub.calls) != 1 || pub.calls[0] != "vault" {
		t.Errorf("publish calls = %v", pub.calls)
	}
}

func TestService_PublishErrorSwallowed(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeDB{
		rowFunc: func() pgx.Row { return staticRow{values: []any{now, now}} },
	}
	pub := &recordingPublisher{err: errors.New("redis down")}
	s, _ := NewService(ServiceDeps{Pool: f, Publisher: pub})
	_, err := s.Create(context.Background(), CreateInput{
		Name: "vault", CallerAID: "archon-alice",
	})
	if err != nil {
		t.Errorf("publish error must be swallowed; got %v", err)
	}
}

func TestService_NopPublisherDefault(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeDB{rowFunc: func() pgx.Row { return staticRow{values: []any{now, now}} }}
	s, err := NewService(ServiceDeps{Pool: f, Publisher: nil}) // nil → nopPublisher
	if err != nil {
		t.Fatalf("NewService(nil publisher): %v", err)
	}
	_, err = s.Create(context.Background(), CreateInput{
		Name: "vault", CallerAID: "archon-alice",
	})
	if err != nil {
		t.Errorf("Create with nop publisher: %v", err)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	cases := map[string]bool{
		"secret_id":   true,
		"token":       true,
		"password":    true,
		"private_key": true,
		"vault_addr":  false,
		"role":        false,
		"":            false,
	}
	for key, want := range cases {
		if got := IsSensitiveKey(key); got != want {
			t.Errorf("IsSensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}
