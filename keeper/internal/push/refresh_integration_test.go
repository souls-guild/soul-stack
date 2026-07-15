//go:build integration

// Integration test for the S7-2 hot-reload closure (ADR-032 amendment 2026-05-27):
//
//	PG/REST CRUD mutation → publish('push-providers:changed') →
//	  each node SUBSCRIBEs → daemon-listener → SshDispatcher.RefreshProvider →
//	    ProviderRespawner.RespawnProvider → new plugin handle.
//
// Here we model the right-hand edge of the chain (publish → listener →
// RefreshProvider → respawner): the PG/REST/Service layer isn't involved,
// since it's already covered by pushprovider.Service unit/integration tests;
// the goal is to prove that the invalidation-channel front end actually
// triggers a re-spawn (exactly what the S7-2 TODO was closing).
//
// Redis fixture is miniredis (no docker), with pub/sub support out of the
// box. That gives a fast run in `make check` without depending on
// testcontainers/docker.
//
// Run:
//
//	go test -tags=integration -count=1 ./keeper/internal/push/...

package push

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// TestRefresh_RedisRoundTrip verifies that publishing to
// `push-providers:changed` actually triggers RefreshProvider, which swaps the
// SshProvider in the dispatcher (via mockRespawner). This is the chain that
// was missing in S7-2.
func TestRefresh_RedisRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("keeperredis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sub, err := keeperredis.SubscribePushProvidersChanged(ctx, client, logger)
	if err != nil {
		t.Fatalf("SubscribePushProvidersChanged: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("subscription not ready: %v", err)
	}

	oldProv := &mockProvider{authAllowed: true}
	newProv := &mockProvider{authAllowed: true}
	oldCloser := &recordingCloser{}
	newCloser := &recordingCloser{}
	resp := &mockRespawner{newProv: newProv, newCloser: newCloser}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{"vault-ssh": {Provider: oldProv, Closer: oldCloser}},
		Respawner: resp,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	// A mini-listener mirroring the daemon.runPushProviderInvalidationListener
	// contract: reads the channel, delegates to Refresh. We use wg for a
	// synchronous assert.
	refreshed := make(chan string, 4)
	var listenerWG sync.WaitGroup
	listenerWG.Add(1)
	go func() {
		defer listenerWG.Done()
		for ev := range sub.Channel() {
			if err := disp.RefreshProvider(context.Background(), ev.Name); err != nil {
				if errors.Is(err, ErrRespawnNotSupported) {
					continue
				}
				t.Errorf("RefreshProvider: %v", err)
				return
			}
			refreshed <- ev.Name
		}
	}()

	if _, err := keeperredis.PublishPushProvidersChanged(ctx, client, "vault-ssh"); err != nil {
		t.Fatalf("PublishPushProvidersChanged: %v", err)
	}

	select {
	case got := <-refreshed:
		if got != "vault-ssh" {
			t.Errorf("refreshed provider = %q, want vault-ssh", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RefreshProvider не вызван в течение 3s после publish")
	}

	if atomic.LoadInt32(&resp.calls) != 1 {
		t.Errorf("respawner calls = %d, want 1", atomic.LoadInt32(&resp.calls))
	}
	if entry, ok := disp.providerEntry("vault-ssh"); !ok || entry.Provider != newProv {
		t.Errorf("dispatcher не подменил provider после Redis-round-trip")
	}
	if oldCloser.closed.Load() != 1 {
		t.Errorf("old plugin-handle не закрыт (closed=%d)", oldCloser.closed.Load())
	}

	_ = sub.Close()
	listenerWG.Wait()
}

// TestRefresh_RedisRoundTrip_DegradedOnSpawnFail verifies that publishing
// while the respawner is broken (spawn failure) must NOT crash the listener:
// the error is logged, and the goroutine keeps reading subsequent messages.
func TestRefresh_RedisRoundTrip_DegradedOnSpawnFail(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("keeperredis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sub, err := keeperredis.SubscribePushProvidersChanged(ctx, client, logger)
	if err != nil {
		t.Fatalf("SubscribePushProvidersChanged: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	failingResp := &mockRespawner{err: errors.New("plugin spawn failed")}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{"vault-ssh": {Provider: &mockProvider{authAllowed: true}, Closer: &recordingCloser{}}},
		Respawner: failingResp,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	errs := make(chan error, 4)
	var listenerWG sync.WaitGroup
	listenerWG.Add(1)
	go func() {
		defer listenerWG.Done()
		for ev := range sub.Channel() {
			if err := disp.RefreshProvider(context.Background(), ev.Name); err != nil &&
				!errors.Is(err, ErrRespawnNotSupported) {
				errs <- err
			}
		}
	}()

	if _, err := keeperredis.PublishPushProvidersChanged(ctx, client, "vault-ssh"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-errs:
		if got == nil {
			t.Fatal("ждали ошибку refresh-а")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ошибка refresh-а не зарегистрирована в течение 3s")
	}

	if _, ok := disp.providerEntry("vault-ssh"); ok {
		t.Errorf("после spawn-fail запись провайдера должна быть удалена (degraded)")
	}

	_ = sub.Close()
	listenerWG.Wait()
}
