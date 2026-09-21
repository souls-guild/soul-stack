package config

import (
	"testing"
)

// ResolvedMaxScope: nil block / nil field → default; explicit 0 → 0 (unlimited);
// an explicit value → itself. This is the source of the value the daemon feeds
// into api.Deps.VoyageMaxScope and mcp.HandlerDeps.VoyageMaxScope (DoS-guard S-med-3):
// if the method returned 0 when "unset", the cap would be dead on both paths.
func TestResolvedMaxScope(t *testing.T) {
	t.Run("nil receiver → default", func(t *testing.T) {
		var v *KeeperVoyage
		if got := v.ResolvedMaxScope(); got != DefaultVoyageMaxScope {
			t.Errorf("nil.ResolvedMaxScope() = %d, want %d", got, DefaultVoyageMaxScope)
		}
	})
	t.Run("nil field → default", func(t *testing.T) {
		v := &KeeperVoyage{}
		if got := v.ResolvedMaxScope(); got != DefaultVoyageMaxScope {
			t.Errorf("empty.ResolvedMaxScope() = %d, want %d", got, DefaultVoyageMaxScope)
		}
	})
	t.Run("explicit zero → unlimited (0)", func(t *testing.T) {
		zero := 0
		v := &KeeperVoyage{MaxScope: &zero}
		if got := v.ResolvedMaxScope(); got != 0 {
			t.Errorf("explicit-zero ResolvedMaxScope() = %d, want 0", got)
		}
	})
	t.Run("explicit value", func(t *testing.T) {
		n := 250
		v := &KeeperVoyage{MaxScope: &n}
		if got := v.ResolvedMaxScope(); got != n {
			t.Errorf("ResolvedMaxScope() = %d, want %d", got, n)
		}
	})
}

// ResolvedMaxBatchSize: nil block / nil field → default; explicit 0 → 0 (no limit);
// an explicit value → itself (parity with ResolvedMaxScope, DoS-guard S-W4).
func TestResolvedMaxBatchSize(t *testing.T) {
	t.Run("nil receiver → default", func(t *testing.T) {
		var v *KeeperVoyage
		if got := v.ResolvedMaxBatchSize(); got != DefaultVoyageMaxBatchSize {
			t.Errorf("nil.ResolvedMaxBatchSize() = %d, want %d", got, DefaultVoyageMaxBatchSize)
		}
	})
	t.Run("nil field → default", func(t *testing.T) {
		v := &KeeperVoyage{}
		if got := v.ResolvedMaxBatchSize(); got != DefaultVoyageMaxBatchSize {
			t.Errorf("empty.ResolvedMaxBatchSize() = %d, want %d", got, DefaultVoyageMaxBatchSize)
		}
	})
	t.Run("explicit zero → unlimited (0)", func(t *testing.T) {
		zero := 0
		v := &KeeperVoyage{MaxBatchSize: &zero}
		if got := v.ResolvedMaxBatchSize(); got != 0 {
			t.Errorf("explicit-zero ResolvedMaxBatchSize() = %d, want 0", got)
		}
	})
	t.Run("explicit value", func(t *testing.T) {
		n := 64
		v := &KeeperVoyage{MaxBatchSize: &n}
		if got := v.ResolvedMaxBatchSize(); got != n {
			t.Errorf("ResolvedMaxBatchSize() = %d, want %d", got, n)
		}
	})
}
