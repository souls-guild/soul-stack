package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// The record this Keeper writes to the Conclave is the one the step-queue check
// reads (ADR-027 amend (p)(d)(ii)): written by conclaveMeta, it must read as
// "can take step rows". Drop the capability and every linear run in the
// cluster silently falls back to inline dispatch.
func TestConclaveMeta_DeclaresStepQueue(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := keeperredis.RegisterInstance(ctx, c, "kid-1", conclaveMeta("kid-1"), 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	ok, err := keeperredis.AllLiveHaveCapability(ctx, c, keeperredis.CapabilityStepQueue)
	if err != nil || !ok {
		t.Errorf("AllLiveHaveCapability over conclaveMeta = %v (err %v), want true", ok, err)
	}
}
