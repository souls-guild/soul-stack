package augur

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeKV struct {
	data    map[string]any
	err     error
	gotPath string
}

func (f *fakeKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.gotPath = path
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func TestBrokerVault_MapToStruct(t *testing.T) {
	kv := &fakeKV{data: map[string]any{"username": "svc", "password": "s3cr3t"}}
	s, err := BrokerVault(context.Background(), kv, "secret/keeper/db")
	if err != nil {
		t.Fatalf("BrokerVault: %v", err)
	}
	if kv.gotPath != "secret/keeper/db" {
		t.Errorf("ReadKV path = %q, want secret/keeper/db", kv.gotPath)
	}
	fields := s.GetFields()
	if fields["username"].GetStringValue() != "svc" {
		t.Errorf("username = %q, want svc", fields["username"].GetStringValue())
	}
	if fields["password"].GetStringValue() != "s3cr3t" {
		t.Errorf("password not carried into Struct")
	}
}

// TestBrokerVault_SecretNotInError — on an encoding failure, the secret
// value must not land in the error text (only path, which isn't secret).
func TestBrokerVault_SecretNotInError(t *testing.T) {
	// A channel in the map doesn't serialize into Struct → NewStruct returns an error.
	kv := &fakeKV{data: map[string]any{"bad": make(chan int)}}
	_, err := BrokerVault(context.Background(), kv, "secret/keeper/db")
	if err == nil {
		t.Fatalf("expected encode error")
	}
	if !strings.Contains(err.Error(), "secret/keeper/db") {
		t.Errorf("error should mention path, got %v", err)
	}
}

func TestBrokerVault_ReadError(t *testing.T) {
	boom := errors.New("vault down")
	kv := &fakeKV{err: boom}
	_, err := BrokerVault(context.Background(), kv, "secret/keeper/db")
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped read error, got %v", err)
	}
}
