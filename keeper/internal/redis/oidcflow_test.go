package redis

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOIDCFlowStore_SaveConsume(t *testing.T) {
	c, _ := newClientMR(t)
	store, err := NewOIDCFlowStore(c, time.Minute)
	if err != nil {
		t.Fatalf("NewOIDCFlowStore: %v", err)
	}
	ctx := context.Background()

	want := OIDCFlowState{Nonce: "nonce-abc", CodeVerifier: "verifier-xyz"}
	if err := store.Save(ctx, "state-1", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Consume(ctx, "state-1")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got != want {
		t.Errorf("Consume = %+v, want %+v", got, want)
	}
}

// TestOIDCFlowStore_SingleUse — ★ anti-replay: повторный Consume того же state
// возвращает ErrOIDCFlowNotFound (GETDEL удалил запись на первом чтении).
func TestOIDCFlowStore_SingleUse(t *testing.T) {
	c, _ := newClientMR(t)
	store, _ := NewOIDCFlowStore(c, time.Minute)
	ctx := context.Background()

	_ = store.Save(ctx, "state-2", OIDCFlowState{Nonce: "n", CodeVerifier: "v"})
	if _, err := store.Consume(ctx, "state-2"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if _, err := store.Consume(ctx, "state-2"); !errors.Is(err, ErrOIDCFlowNotFound) {
		t.Fatalf("second Consume err=%v, want ErrOIDCFlowNotFound (single-use)", err)
	}
}

// TestOIDCFlowStore_UnknownState — неизвестный state → ErrOIDCFlowNotFound.
func TestOIDCFlowStore_UnknownState(t *testing.T) {
	c, _ := newClientMR(t)
	store, _ := NewOIDCFlowStore(c, time.Minute)
	if _, err := store.Consume(context.Background(), "never-saved"); !errors.Is(err, ErrOIDCFlowNotFound) {
		t.Fatalf("unknown state err=%v, want ErrOIDCFlowNotFound", err)
	}
}

// TestOIDCFlowStore_Expiry — истёкший TTL → ErrOIDCFlowNotFound.
func TestOIDCFlowStore_Expiry(t *testing.T) {
	c, mr := newClientMR(t)
	store, _ := NewOIDCFlowStore(c, time.Minute)
	ctx := context.Background()

	_ = store.Save(ctx, "state-3", OIDCFlowState{Nonce: "n", CodeVerifier: "v"})
	mr.FastForward(2 * time.Minute) // протолкнуть TTL
	if _, err := store.Consume(ctx, "state-3"); !errors.Is(err, ErrOIDCFlowNotFound) {
		t.Fatalf("expired state err=%v, want ErrOIDCFlowNotFound", err)
	}
}

// TestOIDCFlowStore_Collision — повторный Save того же state (без потребления)
// отвергается (SET NX), чтобы не затереть активный flow.
func TestOIDCFlowStore_Collision(t *testing.T) {
	c, _ := newClientMR(t)
	store, _ := NewOIDCFlowStore(c, time.Minute)
	ctx := context.Background()

	if err := store.Save(ctx, "state-4", OIDCFlowState{Nonce: "n1"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save(ctx, "state-4", OIDCFlowState{Nonce: "n2"}); err == nil {
		t.Fatal("second Save of same state must fail (NX collision)")
	}
}

func TestNewOIDCFlowStore_Guards(t *testing.T) {
	c, _ := newClientMR(t)
	if _, err := NewOIDCFlowStore(nil, time.Minute); err == nil {
		t.Error("nil client must be rejected")
	}
	if _, err := NewOIDCFlowStore(c, 0); err == nil {
		t.Error("ttl<=0 must be rejected")
	}
}
