package config

import "testing"

// Size limits for the plugin resolver's git-egress hardening (ADR-026(g)):
// `plugins.max_artifact_size_mb` / `plugins.max_clone_size_mb`. Both int MiB,
// defaults 256 / 1024, validation ≥ 1 MiB (0 = "unset" → default). Base —
// keeperBaseRequired (semantic_test.go).

func TestPluginSize_DefaultsWhenOmitted(t *testing.T) {
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("omitted plugin size limits must not trigger value_out_of_range")
	}
	// Omitted → fields 0; Resolved* return defaults in bytes.
	wantArtifact := int64(DefaultPluginMaxArtifactSizeMB) * 1024 * 1024
	if got := cfg.Plugins.ResolvedMaxArtifactSize(); got != wantArtifact {
		t.Fatalf("ResolvedMaxArtifactSize default: want %d, got %d", wantArtifact, got)
	}
	wantClone := int64(DefaultPluginMaxCloneSizeMB) * 1024 * 1024
	if got := cfg.Plugins.ResolvedMaxCloneSize(); got != wantClone {
		t.Fatalf("ResolvedMaxCloneSize default: want %d, got %d", wantClone, got)
	}
}

// A nil Plugins must also resolve to defaults (Resolved* methods are nil-safe).
func TestPluginSize_NilPluginsResolvesDefaults(t *testing.T) {
	var p *KeeperPlugins
	if got := p.ResolvedMaxArtifactSize(); got != int64(DefaultPluginMaxArtifactSizeMB)*1024*1024 {
		t.Fatalf("nil ResolvedMaxArtifactSize: got %d", got)
	}
	if got := p.ResolvedMaxCloneSize(); got != int64(DefaultPluginMaxCloneSizeMB)*1024*1024 {
		t.Fatalf("nil ResolvedMaxCloneSize: got %d", got)
	}
}

func TestPluginSize_ParsedAndResolved(t *testing.T) {
	src := keeperBaseRequired + `plugins:
  max_artifact_size_mb: 64
  max_clone_size_mb: 512
`
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("valid plugin size limits must not trigger value_out_of_range")
	}
	if cfg.Plugins.MaxArtifactSizeMB != 64 || cfg.Plugins.MaxCloneSizeMB != 512 {
		t.Fatalf("parsed = %d/%d, want 64/512", cfg.Plugins.MaxArtifactSizeMB, cfg.Plugins.MaxCloneSizeMB)
	}
	if got := cfg.Plugins.ResolvedMaxArtifactSize(); got != 64*1024*1024 {
		t.Fatalf("ResolvedMaxArtifactSize: want %d, got %d", 64*1024*1024, got)
	}
	if got := cfg.Plugins.ResolvedMaxCloneSize(); got != 512*1024*1024 {
		t.Fatalf("ResolvedMaxCloneSize: want %d, got %d", 512*1024*1024, got)
	}
}

func TestPluginSize_ZeroIsDefault(t *testing.T) {
	// Explicit 0 = "unset" — NOT an error, resolves to the default.
	src := keeperBaseRequired + `plugins:
  max_artifact_size_mb: 0
  max_clone_size_mb: 0
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCodeAt(diags, "value_out_of_range", "$.plugins.max_artifact_size_mb") ||
		hasCodeAt(diags, "value_out_of_range", "$.plugins.max_clone_size_mb") {
		dump(t, diags)
		t.Fatalf("explicit 0 means default, must not be value_out_of_range")
	}
}

func TestPluginSize_BelowMinRejected(t *testing.T) {
	src := keeperBaseRequired + `plugins:
  max_artifact_size_mb: -1
  max_clone_size_mb: -8
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.plugins.max_artifact_size_mb") {
		dump(t, diags)
		t.Fatalf("negative max_artifact_size_mb must trigger value_out_of_range")
	}
	if !hasCodeAt(diags, "value_out_of_range", "$.plugins.max_clone_size_mb") {
		dump(t, diags)
		t.Fatalf("negative max_clone_size_mb must trigger value_out_of_range")
	}
}
