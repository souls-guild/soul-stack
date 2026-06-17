package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// TestNewClient_EmptyAddr защищает sentinel-error для случая пустого
// addr-а (config-parser его не отлавливает — это runtime-инвариант).
func TestNewClient_EmptyAddr(t *testing.T) {
	_, err := NewClient(context.Background(), Config{})
	if err == nil {
		t.Fatal("NewClient with empty addr should return error")
	}
}

// TestNewClient_VaultRefRejected — `password_ref: vault:...` пока что
// возвращает [ErrPasswordResolveNotImplemented]. M0.5d закроет, тест
// поменяется на проверку реального resolve-а.
func TestNewClient_VaultRefRejected(t *testing.T) {
	_, err := NewClient(context.Background(), Config{
		Addr:        "127.0.0.1:0",
		PasswordRef: "vault:secret/keeper/redis",
	})
	if !errors.Is(err, ErrPasswordResolveNotImplemented) {
		t.Fatalf("err = %v, want ErrPasswordResolveNotImplemented", err)
	}
}

// TestNewClient_Miniredis — happy-path: коннект к miniredis-у, Ping, Close.
// На повторный Close — без ошибки (идемпотентность контракта).
func TestNewClient_Miniredis(t *testing.T) {
	mr := miniredis.RunT(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close should be idempotent, got %v", err)
	}
}

// TestNewClient_PingFails — addr-port на котором никто не слушает, Ping
// падает, NewClient возвращает обёрнутую ошибку.
func TestNewClient_PingFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// 127.0.0.1:1 — общесогласованный «port 1, никто не слушает».
	_, err := NewClient(ctx, Config{Addr: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("NewClient should fail on unreachable addr")
	}
}
