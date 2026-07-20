package migrations

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// TestEmbed_ContainsExpectedMigrations -- smoke test that //go:embed
// captured up/down pairs and the order is stable. Full check of migration
// application -- via testcontainers in M0.4.1.
func TestEmbed_ContainsExpectedMigrations(t *testing.T) {
	var names []string
	if err := fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".sql") {
			names = append(names, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(names)

	want := []string{
		"001_create_audit_log.down.sql",
		"001_create_audit_log.up.sql",
		"002_create_purge_audit_old.down.sql",
		"002_create_purge_audit_old.up.sql",
		"003_create_operators.down.sql",
		"003_create_operators.up.sql",
		"004_add_audit_log_operator_fk.down.sql",
		"004_add_audit_log_operator_fk.up.sql",
		"005_create_incarnation.down.sql",
		"005_create_incarnation.up.sql",
		"006_create_state_history.down.sql",
		"006_create_state_history.up.sql",
		"007_create_souls.down.sql",
		"007_create_souls.up.sql",
		"008_create_bootstrap_tokens.down.sql",
		"008_create_bootstrap_tokens.up.sql",
		"009_create_soul_seeds.down.sql",
		"009_create_soul_seeds.up.sql",
		"010_create_expire_pending_seeds.down.sql",
		"010_create_expire_pending_seeds.up.sql",
		"011_create_purge_used_tokens.down.sql",
		"011_create_purge_used_tokens.up.sql",
		"012_create_purge_souls.down.sql",
		"012_create_purge_souls.up.sql",
		"013_create_purge_old_seeds.down.sql",
		"013_create_purge_old_seeds.up.sql",
		"014_create_mark_disconnected.down.sql",
		"014_create_mark_disconnected.up.sql",
		"015_add_souls_soulprint.down.sql",
		"015_add_souls_soulprint.up.sql",
		"016_souls_status_destroyed.down.sql",
		"016_souls_status_destroyed.up.sql",
		"017_soulseeds_status_orphaned.down.sql",
		"017_soulseeds_status_orphaned.up.sql",
		"018_create_apply_runs.down.sql",
		"018_create_apply_runs.up.sql",
		"019_create_providers.down.sql",
		"019_create_providers.up.sql",
		"020_create_profiles.down.sql",
		"020_create_profiles.up.sql",
		"021_create_purge_apply_runs.down.sql",
		"021_create_purge_apply_runs.up.sql",
		"022_create_apply_task_register.down.sql",
		"022_create_apply_task_register.up.sql",
		"023_create_purge_apply_task_register.down.sql",
		"023_create_purge_apply_task_register.up.sql",
		"024_add_apply_runs_cancel_requested.down.sql",
		"024_add_apply_runs_cancel_requested.up.sql",
		"025_add_apply_runs_ward_claim.down.sql",
		"025_add_apply_runs_ward_claim.up.sql",
		"026_create_rbac.down.sql",
		"026_create_rbac.up.sql",
		"027_seed_cluster_admin.down.sql",
		"027_seed_cluster_admin.up.sql",
		"028_create_plugin_sigils.down.sql",
		"028_create_plugin_sigils.up.sql",
		"029_add_apply_runs_recipe.down.sql",
		"029_add_apply_runs_recipe.up.sql",
		"030_add_plugin_sigils_manifest_raw.down.sql",
		"030_add_plugin_sigils_manifest_raw.up.sql",
		"031_incarnation_status_destroying.down.sql",
		"031_incarnation_status_destroying.up.sql",
		"032_create_omens.down.sql",
		"032_create_omens.up.sql",
		"033_create_rites.down.sql",
		"033_create_rites.up.sql",
		"034_create_service_registry.down.sql",
		"034_create_service_registry.up.sql",
		"035_create_keeper_settings.down.sql",
		"035_create_keeper_settings.up.sql",
		"036_incarnation_status_destroy_failed.down.sql",
		"036_incarnation_status_destroy_failed.up.sql",
		"037_create_sigil_signing_keys.down.sql",
		"037_create_sigil_signing_keys.up.sql",
		"038_add_plugin_sigils_commit_sha.down.sql",
		"038_add_plugin_sigils_commit_sha.up.sql",
		"039_create_incarnation_archive.down.sql",
		"039_create_incarnation_archive.up.sql",
		"040_add_apply_runs_dispatched_status.down.sql",
		"040_add_apply_runs_dispatched_status.up.sql",
		"041_create_oracle.down.sql",
		"041_create_oracle.up.sql",
		"042_create_oracle_circuit.down.sql",
		"042_create_oracle_circuit.up.sql",
		"043_mark_disconnected_lease_aware.down.sql",
		"043_mark_disconnected_lease_aware.up.sql",
		"044_add_apply_runs_orphaned_status.down.sql",
		"044_add_apply_runs_orphaned_status.up.sql",
		"045_add_apply_runs_no_match_status.down.sql",
		"045_add_apply_runs_no_match_status.up.sql",
		"046_add_incarnation_covens.down.sql",
		"046_add_incarnation_covens.up.sql",
		"047_incarnation_status_drift.down.sql",
		"047_incarnation_status_drift.up.sql",
		"048_state_history_archived_at.down.sql",
		"048_state_history_archived_at.up.sql",
		"049_create_archive_state_history.down.sql",
		"049_create_archive_state_history.up.sql",
		"050_add_incarnation_drift_scan.down.sql",
		"050_add_incarnation_drift_scan.up.sql",
		"051_create_push_runs.down.sql",
		"051_create_push_runs.up.sql",
		"052_create_errands.down.sql",
		"052_create_errands.up.sql",
		"053_add_souls_ssh_target.down.sql",
		"053_add_souls_ssh_target.up.sql",
		"054_create_push_providers.down.sql",
		"054_create_push_providers.up.sql",
		"055_create_tides.down.sql",
		"055_create_tides.up.sql",
		"056_add_ssh_provider_to_ssh_target.down.sql",
		"056_add_ssh_provider_to_ssh_target.up.sql",
		"057_create_errand_runs.down.sql",
		"057_create_errand_runs.up.sql",
		"058_relax_aid_format.down.sql",
		"058_relax_aid_format.up.sql",
		"059_create_voyages.down.sql",
		"059_create_voyages.up.sql",
		"060_create_choirs.down.sql",
		"060_create_choirs.up.sql",
		"061_drop_tides.down.sql",
		"061_drop_tides.up.sql",
		"062_drop_errand_runs.down.sql",
		"062_drop_errand_runs.up.sql",
		"063_voyage_targets_dispatch_idx.down.sql",
		"063_voyage_targets_dispatch_idx.up.sql",
		"064_voyages_batch_mode.down.sql",
		"064_voyages_batch_mode.up.sql",
		"065_voyages_batch_strategies.down.sql",
		"065_voyages_batch_strategies.up.sql",
		"066_create_cadences.down.sql",
		"066_create_cadences.up.sql",
		"067_rbac_roles_default_scope.down.sql",
		"067_rbac_roles_default_scope.up.sql",
		"068_cadences_interval_floor.down.sql",
		"068_cadences_interval_floor.up.sql",
		"069_create_synods.down.sql",
		"069_create_synods.up.sql",
		"070_cadences_fail_threshold_percent.down.sql",
		"070_cadences_fail_threshold_percent.up.sql",
		"071_create_heralds_tidings.down.sql",
		"071_create_heralds_tidings.up.sql",
		"072_tiding_ephemeral_payload.down.sql",
		"072_tiding_ephemeral_payload.up.sql",
		"073_tiding_task_selector.down.sql",
		"073_tiding_task_selector.up.sql",
		"074_tiding_created_from_cadence.down.sql",
		"074_tiding_created_from_cadence.up.sql",
		"075_create_purge_voyages.down.sql",
		"075_create_purge_voyages.up.sql",
		"076_create_purge_push_runs.down.sql",
		"076_create_purge_push_runs.up.sql",
		"077_create_purge_archives.down.sql",
		"077_create_purge_archives.up.sql",
		"078_add_apply_runs_passage.down.sql",
		"078_add_apply_runs_passage.up.sql",
		"079_apply_task_register_plan_index.down.sql",
		"079_apply_task_register_plan_index.up.sql",
		"080_purge_apply_task_register_plan_index.down.sql",
		"080_purge_apply_task_register_plan_index.up.sql",
		"081_add_apply_runs_failed_plan_index.down.sql",
		"081_add_apply_runs_failed_plan_index.up.sql",
		"082_add_incarnation_applying_epoch.down.sql",
		"082_add_incarnation_applying_epoch.up.sql",
		"083_operators_auth_method_ldap_oidc.down.sql",
		"083_operators_auth_method_ldap_oidc.up.sql",
		"084_operators_created_via.down.sql",
		"084_operators_created_via.up.sql",
		"085_operators_bootstrap_index.down.sql",
		"085_operators_bootstrap_index.up.sql",
		"086_seed_archon_system.down.sql",
		"086_seed_archon_system.up.sql",
		"087_add_souls_traits.down.sql",
		"087_add_souls_traits.up.sql",
		"088_add_incarnation_traits.down.sql",
		"088_add_incarnation_traits.up.sql",
		"089_add_incarnation_created_scenario.down.sql",
		"089_add_incarnation_created_scenario.up.sql",
		"090_incarnation_created_scenario_nullable.down.sql",
		"090_incarnation_created_scenario_nullable.up.sql",
		"091_extend_heralds_type.down.sql",
		"091_extend_heralds_type.up.sql",
		"092_create_warrant.down.sql",
		"092_create_warrant.up.sql",
		"093_create_purge_old_certs.down.sql",
		"093_create_purge_old_certs.up.sql",
		"094_add_providers_fqdn_suffix.down.sql",
		"094_add_providers_fqdn_suffix.up.sql",
		"095_rename_permission_rerun_last.down.sql",
		"095_rename_permission_rerun_last.up.sql",
		"096_create_apply_run_plan.down.sql",
		"096_create_apply_run_plan.up.sql",
		"097_create_purge_apply_run_plan.down.sql",
		"097_create_purge_apply_run_plan.up.sql",
		"098_add_apply_run_plan_params.down.sql",
		"098_add_apply_run_plan_params.up.sql",
		"099_create_incarnation_membership.down.sql",
		"099_create_incarnation_membership.up.sql",
		"100_rbac_drop_pattern_selectors.down.sql",
		"100_rbac_drop_pattern_selectors.up.sql",
	}
	if len(names) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(names), len(want), names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d] = %q, want %q", i, names[i], n)
		}
	}
}

// TestEmbed_UpSQLContainsCreateTable -- sanity on content (prevents
// an empty //go:embed if someone accidentally moves the .sql).
func TestEmbed_UpSQLContainsCreateTable(t *testing.T) {
	b, err := FS.ReadFile("001_create_audit_log.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "CREATE TABLE audit_log") {
		t.Errorf("up.sql does not contain CREATE TABLE audit_log; content head: %.120s", b)
	}
}

// TestEmbed_PurgeAuditOldFunction -- sanity on 002: //go:embed captured
// the up migration and it declares a PL/pgSQL function per ADR-022(d).
func TestEmbed_PurgeAuditOldFunction(t *testing.T) {
	b, err := FS.ReadFile("002_create_purge_audit_old.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "CREATE OR REPLACE FUNCTION purge_audit_old") {
		t.Errorf("up.sql does not declare purge_audit_old; content head: %.200s", b)
	}
	d, err := FS.ReadFile("002_create_purge_audit_old.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_audit_old") {
		t.Errorf("down.sql does not drop purge_audit_old; content: %.200s", d)
	}
}

// TestEmbed_OperatorsTable -- sanity on 003: //go:embed captured the
// operators registry migration (ADR-014) with a partial unique index on
// `created_by_aid IS NULL` (invariant of the single bootstrap Archon).
func TestEmbed_OperatorsTable(t *testing.T) {
	b, err := FS.ReadFile("003_create_operators.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE operators",
		"aid_format",
		"auth_method_valid",
		"created_by_aid_fk",
		"operators_first_archon_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("003_create_operators.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS operators") {
		t.Errorf("down.sql does not drop operators; content: %.200s", d)
	}
}

// TestEmbed_OperatorsAuthMethodLDAPOIDC -- sanity on 083 (ADR-058): only-add
// extension of the auth_method_valid CHECK with `ldap`/`oidc` values. Up extends
// the set, down restores the prior one (jwt/mtls/combined).
func TestEmbed_OperatorsAuthMethodLDAPOIDC(t *testing.T) {
	b, err := FS.ReadFile("083_operators_auth_method_ldap_oidc.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"DROP CONSTRAINT auth_method_valid",
		"'jwt', 'mtls', 'combined', 'ldap', 'oidc'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("083 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("083_operators_auth_method_ldap_oidc.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "'jwt', 'mtls', 'combined'") {
		t.Errorf("083 down.sql does not restore prior set; content: %.200s", d)
	}
}

// TestEmbed_HeraldsTypeExtended -- sanity on 091 (ADR-052 amendment): only-add
// extension of the heralds_type_enum CHECK with channel types telegram/slack/mattermost/
// discord/custom (HTTP) and email (SMTP). Up extends the set with all 7, down
// restores the prior one (webhook only).
func TestEmbed_HeraldsTypeExtended(t *testing.T) {
	b, err := FS.ReadFile("091_extend_heralds_type.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	frags := []string{"DROP CONSTRAINT heralds_type_enum"}
	for _, ty := range []string{"webhook", "telegram", "slack", "mattermost", "discord", "custom", "email"} {
		frags = append(frags, "'"+ty+"'")
	}
	for _, frag := range frags {
		if !strings.Contains(body, frag) {
			t.Errorf("091 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("091_extend_heralds_type.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "('webhook')") {
		t.Errorf("091 down.sql does not restore webhook-only set; content: %.200s", d)
	}
}

// TestEmbed_OperatorsCreatedVia -- sanity on 084 (ADR-058(d)): column
// created_via with created_via_valid CHECK + bootstrap row reconcile.
func TestEmbed_OperatorsCreatedVia(t *testing.T) {
	b, err := FS.ReadFile("084_operators_created_via.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ADD COLUMN created_via",
		"created_via_valid",
		"'bootstrap','user','ldap','oidc','system'",
		"created_via = 'bootstrap'",
		"created_via = 'system'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("084 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("084_operators_created_via.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN created_via") {
		t.Errorf("084 down.sql does not drop created_via; content: %.200s", d)
	}
}

// TestEmbed_OperatorsBootstrapIndex -- sanity on 085 (ADR-058(d)): moves
// the bootstrap invariant to created_via='bootstrap'; down restores it to
// created_by_aid IS NULL.
func TestEmbed_OperatorsBootstrapIndex(t *testing.T) {
	b, err := FS.ReadFile("085_operators_bootstrap_index.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"DROP INDEX operators_first_archon_idx",
		"WHERE created_via = 'bootstrap'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("085 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("085_operators_bootstrap_index.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "WHERE created_by_aid IS NULL") {
		t.Errorf("085 down.sql does not restore created_by_aid IS NULL index; content: %.200s", d)
	}
}

// TestEmbed_SeedArchonSystem -- sanity on 086 (ADR-058(d)): seeds
// the system operator archon-system (created_via='system').
func TestEmbed_SeedArchonSystem(t *testing.T) {
	b, err := FS.ReadFile("086_seed_archon_system.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"INSERT INTO operators",
		"'archon-system'",
		"'system'",
		"ON CONFLICT (aid) DO NOTHING",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("086 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("086_seed_archon_system.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DELETE FROM operators WHERE aid = 'archon-system'") {
		t.Errorf("086 down.sql does not delete archon-system; content: %.200s", d)
	}
}

// TestEmbed_IncarnationTable -- sanity on 005: incarnation registry
// (ADR-009) with CHECK on status / name_format and FK to operators.
func TestEmbed_IncarnationTable(t *testing.T) {
	b, err := FS.ReadFile("005_create_incarnation.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE incarnation",
		"incarnation_name_format",
		"incarnation_status_valid",
		"incarnation_created_by_aid_fk",
		"incarnation_service_idx",
		"incarnation_status_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("005_create_incarnation.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS incarnation") {
		t.Errorf("down.sql does not drop incarnation; content: %.200s", d)
	}
}

// TestEmbed_StateHistoryTable -- sanity on 006: state_history journal
// (ADR-009 / ADR-019) with CASCADE FK to incarnation and indexes for typical
// history queries.
func TestEmbed_StateHistoryTable(t *testing.T) {
	b, err := FS.ReadFile("006_create_state_history.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE state_history",
		"state_history_incarnation_fk",
		"ON DELETE CASCADE",
		"state_history_changed_by_aid_fk",
		"state_history_incarnation_at_idx",
		"state_history_apply_id_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("006_create_state_history.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS state_history") {
		t.Errorf("down.sql does not drop state_history; content: %.200s", d)
	}
}

// TestEmbed_SoulsTable -- sanity on 007: souls registry (ADR-002 / ADR-012)
// with CHECK on status/transport/sid format, GIN index on coven, FK to
// operators.
func TestEmbed_SoulsTable(t *testing.T) {
	b, err := FS.ReadFile("007_create_souls.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE souls",
		"souls_sid_format",
		"souls_transport_valid",
		"souls_status_valid",
		"souls_created_by_aid_fk",
		"souls_status_idx",
		"souls_coven_idx",
		"souls_pending_requested_at_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("007_create_souls.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS souls") {
		t.Errorf("down.sql does not drop souls; content: %.200s", d)
	}
}

// TestEmbed_BootstrapTokensTable -- sanity on 008: registry of one-time
// SoulSeed tokens with a partial unique index on `used_at IS NULL`, FK to
// souls (ON DELETE CASCADE) and operators (ON DELETE SET NULL).
func TestEmbed_BootstrapTokensTable(t *testing.T) {
	b, err := FS.ReadFile("008_create_bootstrap_tokens.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE bootstrap_tokens",
		"bootstrap_tokens_sid_fk",
		"bootstrap_tokens_created_by_aid_fk",
		"bootstrap_tokens_expires_after_created",
		"bootstrap_tokens_token_hash_format",
		"bootstrap_tokens_active_by_sid_idx",
		"bootstrap_tokens_token_hash_idx",
		"bootstrap_tokens_used_at_idx",
		"bootstrap_tokens_expires_at_idx",
		"WHERE used_at IS NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("008_create_bootstrap_tokens.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS bootstrap_tokens") {
		t.Errorf("down.sql does not drop bootstrap_tokens; content: %.200s", d)
	}
}

// TestEmbed_SoulSeedsTable -- sanity on 009: registry of issued
// SoulSeed certificates with a partial unique on `status='active'`,
// FK to souls (CASCADE), CHECK on fingerprint format.
func TestEmbed_SoulSeedsTable(t *testing.T) {
	b, err := FS.ReadFile("009_create_soul_seeds.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE soul_seeds",
		"soul_seeds_sid_fk",
		"soul_seeds_status_valid",
		"soul_seeds_fingerprint_format",
		"soul_seeds_expires_after_issued",
		"soul_seeds_active_by_sid_idx",
		"soul_seeds_fingerprint_idx",
		"soul_seeds_serial_number_idx",
		"soul_seeds_status_idx",
		"soul_seeds_expires_at_idx",
		"WHERE status = 'active'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("009_create_soul_seeds.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS soul_seeds") {
		t.Errorf("down.sql does not drop soul_seeds; content: %.200s", d)
	}
}

// TestEmbed_ReaperFunctions -- sanity on 010-014: Reaper SQL functions for
// 5 rules from docs/keeper/reaper.md. Contracts:
//   - 010 expire_pending_seeds(interval, integer) -- DELETE bootstrap_tokens
//     with used_at IS NULL and expired expires_at (PM reinterpretation: the rule
//     became DELETE-only, since bootstrap_tokens has no status column).
//   - 011 purge_used_tokens(interval, integer) -- DELETE bootstrap_tokens
//     with used_at older than max_age.
//   - 012 purge_souls(text[], interval, integer) -- DELETE souls in the given
//     statuses with COALESCE(last_seen_at, registered_at) older than max_age.
//   - 013 purge_old_seeds(text[], interval, integer) -- DELETE soul_seeds
//     in the given statuses with issued_at older than max_age.
//   - 014 mark_disconnected(interval, integer) -- UPDATE souls
//     connected -> disconnected on stale last_seen_at.
func TestEmbed_ReaperFunctions(t *testing.T) {
	cases := []struct {
		file     string
		createOK string
		dropOK   string
	}{
		{
			"010_create_expire_pending_seeds",
			"CREATE OR REPLACE FUNCTION expire_pending_seeds(max_age interval",
			"DROP FUNCTION IF EXISTS expire_pending_seeds(interval, integer)",
		},
		{
			"011_create_purge_used_tokens",
			"CREATE OR REPLACE FUNCTION purge_used_tokens(max_age interval",
			"DROP FUNCTION IF EXISTS purge_used_tokens(interval, integer)",
		},
		{
			"012_create_purge_souls",
			"CREATE OR REPLACE FUNCTION purge_souls(",
			"DROP FUNCTION IF EXISTS purge_souls(text[], interval, integer)",
		},
		{
			"013_create_purge_old_seeds",
			"CREATE OR REPLACE FUNCTION purge_old_seeds(",
			"DROP FUNCTION IF EXISTS purge_old_seeds(text[], interval, integer)",
		},
		{
			"014_create_mark_disconnected",
			"CREATE OR REPLACE FUNCTION mark_disconnected(stale_after interval",
			"DROP FUNCTION IF EXISTS mark_disconnected(interval, integer)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			up, err := FS.ReadFile(tc.file + ".up.sql")
			if err != nil {
				t.Fatalf("read up: %v", err)
			}
			if !strings.Contains(string(up), tc.createOK) {
				t.Errorf("up.sql missing CREATE; want %q; head: %.200s", tc.createOK, up)
			}
			down, err := FS.ReadFile(tc.file + ".down.sql")
			if err != nil {
				t.Fatalf("read down: %v", err)
			}
			if !strings.Contains(string(down), tc.dropOK) {
				t.Errorf("down.sql missing DROP; want %q; content: %.200s", tc.dropOK, down)
			}
		})
	}
}

// TestEmbed_SoulsSoulprintColumns -- sanity on 015: adds three
// `souls.soulprint_*` columns under ADR-018 (typed-soulprint storage).
func TestEmbed_SoulsSoulprintColumns(t *testing.T) {
	b, err := FS.ReadFile("015_add_souls_soulprint.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE souls",
		"soulprint_facts",
		"soulprint_collected_at",
		"soulprint_received_at",
		"JSONB",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("015_add_souls_soulprint.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN") {
		t.Errorf("down.sql does not drop columns; content: %.200s", d)
	}
}

// TestEmbed_SoulsStatusDestroyed -- sanity on 016: the souls.status enum
// gets extended with the `destroyed` value (ADR-017 cascade, terminal state).
func TestEmbed_SoulsStatusDestroyed(t *testing.T) {
	b, err := FS.ReadFile("016_souls_status_destroyed.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE souls",
		"DROP CONSTRAINT souls_status_valid",
		"ADD CONSTRAINT souls_status_valid",
		"'destroyed'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("016_souls_status_destroyed.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if strings.Contains(string(d), "'destroyed'") {
		t.Errorf("down.sql still references 'destroyed'; content: %.200s", d)
	}
}

// TestEmbed_SoulSeedsStatusOrphaned -- sanity on 017: the soul_seeds.status enum
// gets extended with the `orphaned` value (ADR-017 cascade).
func TestEmbed_SoulSeedsStatusOrphaned(t *testing.T) {
	b, err := FS.ReadFile("017_soulseeds_status_orphaned.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE soul_seeds",
		"DROP CONSTRAINT soul_seeds_status_valid",
		"ADD CONSTRAINT soul_seeds_status_valid",
		"'orphaned'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("017_soulseeds_status_orphaned.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if strings.Contains(string(d), "'orphaned'") {
		t.Errorf("down.sql still references 'orphaned'; content: %.200s", d)
	}
}

// TestEmbed_IncarnationStatusDestroying -- sanity on 031: the
// incarnation.status enum gets extended with the `destroying` value (S-D1).
func TestEmbed_IncarnationStatusDestroying(t *testing.T) {
	b, err := FS.ReadFile("031_incarnation_status_destroying.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"DROP CONSTRAINT incarnation_status_valid",
		"ADD CONSTRAINT incarnation_status_valid",
		"'destroying'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("031_incarnation_status_destroying.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if strings.Contains(string(d), "'destroying'") {
		t.Errorf("down.sql still references 'destroying'; content: %.200s", d)
	}
}

// TestEmbed_IncarnationStatusDestroyFailed -- sanity on 036 (S-D2a): the
// incarnation.status enum gets extended with the terminal `destroy_failed` value. up
// adds the value to the CHECK (drop+recreate, like 031); down narrows the CHECK back
// to the 031 form and must NOT mention `destroy_failed`.
func TestEmbed_IncarnationStatusDestroyFailed(t *testing.T) {
	b, err := FS.ReadFile("036_incarnation_status_destroy_failed.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"DROP CONSTRAINT incarnation_status_valid",
		"ADD CONSTRAINT incarnation_status_valid",
		"'destroy_failed'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	// up extends without losing previous values (pattern 031 + destroy_failed).
	if !strings.Contains(body, "'destroying'") {
		t.Errorf("up.sql must preserve 'destroying' in the extended CHECK; content: %.300s", body)
	}
	d, err := FS.ReadFile("036_incarnation_status_destroy_failed.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	if strings.Contains(dbody, "'destroy_failed'") {
		t.Errorf("down.sql still references 'destroy_failed'; content: %.200s", dbody)
	}
	// down restores the CHECK to the 031 form (keeps 'destroying').
	if !strings.Contains(dbody, "'destroying'") {
		t.Errorf("down.sql must restore CHECK to the 031 form (with 'destroying'); content: %.200s", dbody)
	}
}

// TestEmbed_IncarnationCovens -- sanity on 046 (ADR-008 amendment a): adds
// the incarnation.covens column (declared env tags for RBAC coven scope). up
// adds TEXT[] NOT NULL DEFAULT '{}'; down drops the column.
func TestEmbed_IncarnationCovens(t *testing.T) {
	b, err := FS.ReadFile("046_add_incarnation_covens.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"ADD COLUMN covens TEXT[] NOT NULL DEFAULT '{}'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("046_add_incarnation_covens.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS covens") {
		t.Errorf("down.sql does not drop covens column; content: %.200s", d)
	}
}

// TestEmbed_IncarnationTraits -- sanity on 088 (ADR-060 amend, R1): Trait
// relocated per-soul -> per-incarnation. up adds a jsonb column
// incarnation.traits (NOT NULL DEFAULT '{}', mirroring souls.traits 087) +
// GIN index; down drops the index and column. souls.traits is NOT mentioned in
// down (this migration doesn't touch it -- the projection target remains).
func TestEmbed_IncarnationTraits(t *testing.T) {
	b, err := FS.ReadFile("088_add_incarnation_traits.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"ADD COLUMN traits JSONB NOT NULL DEFAULT '{}'::jsonb",
		"CREATE INDEX incarnation_traits_idx",
		"USING GIN (traits)",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("088 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("088_add_incarnation_traits.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS incarnation_traits_idx",
		"DROP COLUMN IF EXISTS traits",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("088 down.sql missing %q; content: %.200s", frag, dbody)
		}
	}
	// down does NOT operate on souls (projection target, migration 087 removes it);
	// mentioning `souls.traits` in a comment is allowed, but no
	// ALTER/DROP on souls / souls_traits_idx.
	for _, bad := range []string{"ALTER TABLE souls", "souls_traits_idx"} {
		if strings.Contains(dbody, bad) {
			t.Errorf("088 down.sql must NOT operate on souls (%q); content: %.200s", bad, dbody)
		}
	}
}

// TestEmbed_IncarnationCreatedScenario -- sanity on 089 (mechanism for several
// create scenarios, Variant A): the incarnation.created_scenario column
// (NOT NULL DEFAULT 'create') holds the starting scenario name; rerun-last
// restarts exactly that one. up adds the column with DEFAULT; down drops it.
func TestEmbed_IncarnationCreatedScenario(t *testing.T) {
	b, err := FS.ReadFile("089_add_incarnation_created_scenario.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"ADD COLUMN created_scenario TEXT NOT NULL DEFAULT 'create'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("089 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("089_add_incarnation_created_scenario.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS created_scenario") {
		t.Errorf("089 down.sql does not drop created_scenario; content: %.200s", d)
	}
}

// TestEmbed_IncarnationCreatedScenarioNullable -- sanity on 090 (Phase 2 of create
// variants): a bare incarnation via NULL. up removes NOT NULL + DEFAULT from
// created_scenario (NULL = created without a bootstrap scenario, StatusReady without a run).
// down restores the 089 form (NOT NULL DEFAULT 'create'), but must FIRST
// run backfill NULL -> 'create' and ONLY THEN SET NOT NULL -- otherwise ALTER ...
// SET NOT NULL would fail on bare rows. Order (backfill BEFORE SET NOT NULL) --
// the riskiest spot in the rollback; pin it explicitly by text position.
func TestEmbed_IncarnationCreatedScenarioNullable(t *testing.T) {
	b, err := FS.ReadFile("090_incarnation_created_scenario_nullable.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"ALTER COLUMN created_scenario DROP NOT NULL",
		"ALTER COLUMN created_scenario DROP DEFAULT",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("090 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("090_incarnation_created_scenario_nullable.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down restores NOT NULL + DEFAULT (rollback to 089).
	for _, frag := range []string{
		"SET DEFAULT 'create'",
		"SET NOT NULL",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("090 down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
	// Backfill NULL -> 'create' must precede the real SET NOT NULL -- otherwise
	// the rollback breaks on bare rows. Compare by position in the text. `SET NOT
	// NULL` is also mentioned in the header comment (before backfill), so the real
	// ALTER is taken via LastIndex (the DDL statement comes last in the file).
	backfillPos := strings.Index(dbody, "SET created_scenario = 'create'")
	notNullPos := strings.LastIndex(dbody, "SET NOT NULL")
	if backfillPos < 0 {
		t.Fatalf("090 down.sql missing backfill UPDATE ... SET created_scenario = 'create'; content: %.300s", dbody)
	}
	if backfillPos > notNullPos {
		t.Errorf("090 down.sql: backfill NULL->'create' (pos %d) must precede SET NOT NULL (pos %d) -- otherwise ALTER fails on bare rows", backfillPos, notNullPos)
	}
	// Backfill targets exactly the NULL rows (bare), doesn't overwrite existing choices.
	if !strings.Contains(dbody, "WHERE created_scenario IS NULL") {
		t.Errorf("090 down.sql backfill must target only NULL rows (WHERE created_scenario IS NULL); content: %.300s", dbody)
	}
}

// TestEmbed_ApplyRunsTable -- sanity on 018: registry of apply runs
// (M2.x scenario runner) with composite PK (apply_id, sid), closed CHECK on
// status, CASCADE FK to incarnation, SET NULL FK to operators and three
// indexes (incarnation / apply_id / partial running).
func TestEmbed_ApplyRunsTable(t *testing.T) {
	b, err := FS.ReadFile("018_create_apply_runs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE apply_runs",
		"PRIMARY KEY (apply_id, sid)",
		"apply_runs_status_valid",
		"apply_runs_incarnation_fk",
		"ON DELETE CASCADE",
		"apply_runs_started_by_aid_fk",
		"ON DELETE SET NULL",
		"apply_runs_incarnation_idx",
		"apply_runs_apply_idx",
		"apply_runs_status_idx",
		"WHERE status = 'running'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("018_create_apply_runs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS apply_runs") {
		t.Errorf("down.sql does not drop apply_runs; content: %.200s", d)
	}
}

// TestEmbed_ProvidersTable -- sanity on 019: registry of Cloud Providers
// (ADR-017) with CHECK on name/type format (kebab), SET NULL FK to operators
// and an index on created_by_aid.
func TestEmbed_ProvidersTable(t *testing.T) {
	b, err := FS.ReadFile("019_create_providers.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE providers",
		"providers_name_format",
		"providers_type_format",
		"providers_created_by_aid_fk",
		"ON DELETE SET NULL",
		"providers_created_by_aid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("019_create_providers.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS providers") {
		t.Errorf("down.sql does not drop providers; content: %.200s", d)
	}
}

// TestEmbed_ProfilesTable -- sanity on 020: registry of Cloud Profiles
// (ADR-017) with CHECK on name format, RESTRICT FK to providers (PM decision --
// protection from data loss), SET NULL FK to operators and two indexes
// (provider / created_by_aid).
func TestEmbed_ProfilesTable(t *testing.T) {
	b, err := FS.ReadFile("020_create_profiles.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE profiles",
		"profiles_name_format",
		"profiles_provider_fk",
		"ON DELETE RESTRICT",
		"profiles_created_by_aid_fk",
		"ON DELETE SET NULL",
		"profiles_provider_idx",
		"profiles_created_by_aid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("020_create_profiles.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS profiles") {
		t.Errorf("down.sql does not drop profiles; content: %.200s", d)
	}
}

// TestEmbed_PurgeApplyRunsFunction -- sanity on 021: Reaper rule
// `purge_apply_runs(interval, integer)` -- DELETE finished apply_runs
// (success/failed/cancelled) with finished_at older than max_age; running is not
// touched (pinning the status/finished_at filter in up.sql).
func TestEmbed_PurgeApplyRunsFunction(t *testing.T) {
	b, err := FS.ReadFile("021_create_purge_apply_runs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval",
		"DELETE FROM apply_runs",
		"status IN ('success', 'failed', 'cancelled')",
		"finished_at IS NOT NULL",
		"finished_at < NOW() - max_age",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("021_create_purge_apply_runs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_apply_runs(interval, integer)") {
		t.Errorf("down.sql does not drop purge_apply_runs; content: %.200s", d)
	}
}

// TestEmbed_PurgeVoyagesFunction -- sanity on 075: Reaper rule
// `purge_voyages(interval, integer)` -- DELETE finished voyages
// (succeeded/failed/partial_failed/cancelled) with finished_at older than max_age;
// scheduled/pending/running is not touched (pinning the status/finished_at filter and
// reliance on ON DELETE CASCADE for voyage_targets). ADR-046 SS79.
func TestEmbed_PurgeVoyagesFunction(t *testing.T) {
	b, err := FS.ReadFile("075_create_purge_voyages.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_voyages(max_age interval",
		"DELETE FROM voyages",
		"status IN ('succeeded', 'failed', 'partial_failed', 'cancelled')",
		"finished_at IS NOT NULL",
		"finished_at < NOW() - max_age",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("075_create_purge_voyages.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_voyages(interval, integer)") {
		t.Errorf("down.sql does not drop purge_voyages; content: %.200s", d)
	}
}

// TestEmbed_PurgePushRunsFunction -- sanity on 076: Reaper rule
// `purge_push_runs(interval, integer)` -- DELETE finished push_runs
// (success/partial_failed/failed/cancelled) with finished_at older than max_age;
// pending/running is not touched (that's the purge_orphan_push_runs rule). push_runs
// has no child FK tables -- per-host results are inline in the summary (051),
// so cascade isn't pinned. Parity with purge_apply_runs / purge_voyages.
func TestEmbed_PurgePushRunsFunction(t *testing.T) {
	b, err := FS.ReadFile("076_create_purge_push_runs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_push_runs(max_age interval",
		"DELETE FROM push_runs",
		"status IN ('success', 'partial_failed', 'failed', 'cancelled')",
		"finished_at IS NOT NULL",
		"finished_at < NOW() - max_age",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("076_create_purge_push_runs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_push_runs(interval, integer)") {
		t.Errorf("down.sql does not drop purge_push_runs; content: %.200s", d)
	}
}

// TestEmbed_PurgeArchivesFunctions -- sanity on 077: three Reaper rules for
// retaining compliance-class archive data:
//   - purge_incarnation_archive(interval, integer) -- DELETE incarnation_archive
//     by archived_at older than max_age (039);
//   - purge_state_history_archive(interval, integer) -- DELETE state_history_archive
//     by archived_at older than max_age (039);
//   - purge_archived_state_history(interval, integer) -- DELETE soft-deleted
//     snapshots (archived_at IS NOT NULL) from live state_history older than max_age (048).
//
// Pins the archived_at filter presence and the mandatory archived_at IS NOT NULL
// for the live state_history rule (protects active snapshots from being wiped). All three DROPs
// in one down.
func TestEmbed_PurgeArchivesFunctions(t *testing.T) {
	b, err := FS.ReadFile("077_create_purge_archives.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_incarnation_archive(max_age interval",
		"DELETE FROM incarnation_archive",
		"CREATE OR REPLACE FUNCTION purge_state_history_archive(max_age interval",
		"DELETE FROM state_history_archive",
		"CREATE OR REPLACE FUNCTION purge_archived_state_history(max_age interval",
		"DELETE FROM state_history",
		"archived_at IS NOT NULL",
		"archived_at < NOW() - max_age",
		"LIMIT batch_size",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("077 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("077_create_purge_archives.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP FUNCTION IF EXISTS purge_incarnation_archive(interval, integer)",
		"DROP FUNCTION IF EXISTS purge_state_history_archive(interval, integer)",
		"DROP FUNCTION IF EXISTS purge_archived_state_history(interval, integer)",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("077 down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_ApplyTaskRegisterTable -- sanity on 022: accumulator of register data
// for run tasks (state_changes slice 2) with composite PK (apply_id, sid, task_idx)
// and CASCADE FK to apply_runs(apply_id, sid).
func TestEmbed_ApplyTaskRegisterTable(t *testing.T) {
	b, err := FS.ReadFile("022_create_apply_task_register.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE apply_task_register",
		"PRIMARY KEY (apply_id, sid, task_idx)",
		"apply_task_register_apply_run_fk",
		"REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE",
		"apply_task_register_apply_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("022_create_apply_task_register.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS apply_task_register") {
		t.Errorf("down.sql does not drop apply_task_register; content: %.200s", d)
	}
}

// TestEmbed_PurgeApplyTaskRegisterFunction -- sanity on 023: Reaper rule
// `purge_apply_task_register(interval, integer)` -- DELETE register rows
// of runs in a terminal status (success/failed/cancelled) with finished_at
// older than grace; register of active (running) runs is not touched (pinning the
// join to apply_runs + status/finished_at filter in up.sql).
func TestEmbed_PurgeApplyTaskRegisterFunction(t *testing.T) {
	b, err := FS.ReadFile("023_create_purge_apply_task_register.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval",
		"DELETE FROM apply_task_register",
		"JOIN apply_runs ar",
		"ar.status IN ('success', 'failed', 'cancelled')",
		"ar.finished_at IS NOT NULL",
		"ar.finished_at < NOW() - grace_period",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("023_create_purge_apply_task_register.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_apply_task_register(interval, integer)") {
		t.Errorf("down.sql does not drop purge_apply_task_register; content: %.200s", d)
	}
}

// TestEmbed_PurgeApplyTaskRegisterPlanIndex -- sanity on 080 (ADR-056 SsS1 fix
// Variant B): forward fix of purge_apply_task_register for staged rendering. up
// switches the DELETE join from a non-unique-under-N>1 task_idx to a stably
// unique plan_index (CTE projection + final DELETE predicate); down restores
// the function body to the 023 form (task_idx join), since 079.down removes plan_index.
func TestEmbed_PurgeApplyTaskRegisterPlanIndex(t *testing.T) {
	b, err := FS.ReadFile("080_purge_apply_task_register_plan_index.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval",
		"SELECT atr.apply_id, atr.sid, atr.plan_index",
		"ar.passage = atr.passage",
		"t.plan_index = e.plan_index",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("080 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// up must NOT key the delete by task_idx (that was the bug).
	if strings.Contains(body, "t.task_idx = e.task_idx") {
		t.Errorf("080 up.sql still keys DELETE by task_idx; content: %.400s", body)
	}
	d, err := FS.ReadFile("080_purge_apply_task_register_plan_index.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down restores the 023 form (task_idx join), without referencing the
	// plan_index column in SQL (079.down removes it). The explanatory comment can
	// mention plan_index -- we check exactly the SQL predicates/projection.
	if !strings.Contains(dbody, "t.task_idx = e.task_idx") {
		t.Errorf("080 down.sql does not restore task_idx-join (form 023); content: %.300s", dbody)
	}
	for _, frag := range []string{"atr.plan_index", "t.plan_index", "e.plan_index"} {
		if strings.Contains(dbody, frag) {
			t.Errorf("080 down.sql still references column %q (079.down will remove the column); content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_ApplyRunsPassage -- sanity on 078 (staged-render Passage, ADR-056
// S1, Variant I): extends the apply_runs PK to (apply_id, sid, passage) +
// repoints the apply_task_register FK to the triple. up adds the passage column
// (NOT NULL DEFAULT 0) to both tables, drops the old PK/FK and recreates them
// as triples; down restores the paired PK/FK and drops the columns.
func TestEmbed_ApplyRunsPassage(t *testing.T) {
	b, err := FS.ReadFile("078_add_apply_runs_passage.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"ADD COLUMN passage INT NOT NULL DEFAULT 0",
		"ALTER TABLE apply_task_register",
		"DROP CONSTRAINT apply_task_register_apply_run_fk",
		"DROP CONSTRAINT apply_runs_pkey",
		"ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid, passage)",
		"FOREIGN KEY (apply_id, sid, passage) REFERENCES apply_runs (apply_id, sid, passage) ON DELETE CASCADE",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("078 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("078_add_apply_runs_passage.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid)",
		"FOREIGN KEY (apply_id, sid) REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE",
		"DROP COLUMN passage",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("078 down.sql missing %q; content: %.400s", frag, dbody)
		}
	}
	// down must not leave the triple PK (form 018).
	if strings.Contains(dbody, "PRIMARY KEY (apply_id, sid, passage)") {
		t.Errorf("078 down.sql still references the triple PK; content: %.300s", dbody)
	}
}

// TestEmbed_ApplyRunsFailedPlanIndex -- sanity on 081 (ADR-056 SsS1 fix Variant B,
// failure channel -- the last instance of the global-vs-local-task_idx class): apply_runs
// gets a nullable failed_plan_index column for the GLOBAL plan_index of the failed
// task (module/action correlation in the drift report + no_log suppression in the barrier).
// up adds the column and backfills it from task_idx (N=1: local==global);
// down drops the column.
func TestEmbed_ApplyRunsFailedPlanIndex(t *testing.T) {
	b, err := FS.ReadFile("081_add_apply_runs_failed_plan_index.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"ADD COLUMN failed_plan_index INT",
		"SET failed_plan_index = task_idx",
		"WHERE task_idx IS NOT NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("081 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// The column is nullable (like task_idx): unknown until the first failed task --
	// NOT NULL DEFAULT here would be the wrong semantics.
	if strings.Contains(body, "failed_plan_index INT NOT NULL") {
		t.Errorf("081 up.sql: failed_plan_index must be nullable (like task_idx); content: %.300s", body)
	}
	d, err := FS.ReadFile("081_add_apply_runs_failed_plan_index.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS failed_plan_index") {
		t.Errorf("081 down.sql does not drop failed_plan_index; content: %.200s", d)
	}
}

// TestEmbed_IncarnationApplyingEpoch -- sanity on 082 (ADR-027 amend (m), S0):
// additive prep of incarnation for standalone-orphan reconcile. up
// adds four NULLABLE applying-epoch columns (applying_apply_id /
// applying_attempt / applying_by_kid / applying_since) + a partial index for
// the Reaper stale-applying scan. down (columns nullable, no constraint -> reversible)
// drops the index and columns.
func TestEmbed_IncarnationApplyingEpoch(t *testing.T) {
	b, err := FS.ReadFile("082_add_incarnation_applying_epoch.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"applying_apply_id TEXT",
		"applying_attempt  INTEGER",
		"applying_by_kid   TEXT",
		"applying_since    TIMESTAMPTZ",
		"CREATE INDEX incarnation_applying_scan_idx",
		"WHERE status = 'applying'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("082 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	// Columns are nullable: NOT NULL / DEFAULT here would break fail-safe (existing
	// applying rows with unknown epoch must NOT be reclaimed based on a NULL by_kid).
	for _, bad := range []string{
		"applying_apply_id TEXT NOT NULL",
		"applying_by_kid   TEXT NOT NULL",
		"applying_since    TIMESTAMPTZ NOT NULL",
	} {
		if strings.Contains(body, bad) {
			t.Errorf("082 up.sql: applying-epoch columns must be nullable, found %q", bad)
		}
	}
	d, err := FS.ReadFile("082_add_incarnation_applying_epoch.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	down := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS incarnation_applying_scan_idx",
		"DROP COLUMN IF EXISTS applying_apply_id",
		"DROP COLUMN IF EXISTS applying_since",
	} {
		if !strings.Contains(down, frag) {
			t.Errorf("082 down.sql missing %q; content: %.300s", frag, down)
		}
	}
}

// TestEmbed_AuditLogOperatorFK -- sanity on 004: adds the FK
// `audit_log.archon_aid -> operators(aid)` with ON DELETE SET NULL.
func TestEmbed_AuditLogOperatorFK(t *testing.T) {
	b, err := FS.ReadFile("004_add_audit_log_operator_fk.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE audit_log",
		"audit_log_archon_aid_fk",
		"REFERENCES operators",
		"ON DELETE SET NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("004_add_audit_log_operator_fk.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP CONSTRAINT IF EXISTS audit_log_archon_aid_fk") {
		t.Errorf("down.sql does not drop FK; content: %.200s", d)
	}
}

// TestEmbed_ApplyRunsCancelRequested -- sanity on 024: cluster-wide Cancel (G1)
// adds the apply_runs.cancel_requested column (BOOLEAN NOT NULL DEFAULT
// false) -- a cancellation flag read by the run goroutine in barrier polling.
func TestEmbed_ApplyRunsCancelRequested(t *testing.T) {
	b, err := FS.ReadFile("024_add_apply_runs_cancel_requested.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"cancel_requested",
		"BOOLEAN NOT NULL DEFAULT false",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("024_add_apply_runs_cancel_requested.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS cancel_requested") {
		t.Errorf("down.sql does not drop cancel_requested; content: %.200s", d)
	}
}

// TestEmbed_ApplyRunsWardClaim -- sanity on 025 (ADR-027 Phase 0): additive
// prep of apply_runs for the work-queue + claim model. up adds four
// Ward-claim columns (claim_by_kid / claim_at / claim_expires_at / attempt),
// extends the status CHECK with planned/claimed values (drop+recreate, as in
// 016/017) preserving running/success/failed/cancelled, and lays a
// partial index for the claim scan. down (status -- CHECK, not enum -> reversible)
// drops the index/columns and restores the CHECK to the 018+024 form.
func TestEmbed_ApplyRunsWardClaim(t *testing.T) {
	b, err := FS.ReadFile("025_add_apply_runs_ward_claim.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"ADD COLUMN claim_by_kid",
		"ADD COLUMN claim_at",
		"ADD COLUMN claim_expires_at",
		"ADD COLUMN attempt",
		"DROP CONSTRAINT apply_runs_status_valid",
		"ADD CONSTRAINT apply_runs_status_valid",
		"'planned'",
		"'claimed'",
		"'running'",
		"'success'",
		"'failed'",
		"'cancelled'",
		"CREATE INDEX apply_runs_claim_scan_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("025_add_apply_runs_ward_claim.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS apply_runs_claim_scan_idx",
		"DROP COLUMN IF EXISTS attempt",
		"DROP COLUMN IF EXISTS claim_expires_at",
		"DROP COLUMN IF EXISTS claim_at",
		"DROP COLUMN IF EXISTS claim_by_kid",
		"ADD CONSTRAINT apply_runs_status_valid",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
	// The restored down CHECK returns to the 018+024 form
	// (running/success/failed/cancelled) -- rolling back values is possible
	// (CHECK constraint, not enum). Exact CHECK content check on the
	// SQL side is in migrate-integration (TestIntegration_ApplyRunsWardClaim_Phase0):
	// here it's a sanity grep on a fragment, not parsing the whole SQL.
	if !strings.Contains(dbody, "CHECK (status IN ('running', 'success', 'failed', 'cancelled'))") {
		t.Errorf("down.sql restored CHECK not in the 018+024 form; content: %.300s", dbody)
	}
}

// TestEmbed_ApplyRunsDispatchedStatus -- sanity on 040 (ADR-027 amend, S2): the
// apply_runs.status enum gets extended with the `dispatched` phase. up adds the value
// to the CHECK (drop+recreate, as in 025/036) preserving planned/claimed/running/success/
// failed/cancelled; down moves dispatched rows to running and narrows the CHECK
// back to the 025 form (must not mention `dispatched`).
func TestEmbed_ApplyRunsDispatchedStatus(t *testing.T) {
	b, err := FS.ReadFile("040_add_apply_runs_dispatched_status.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"DROP CONSTRAINT apply_runs_status_valid",
		"ADD CONSTRAINT apply_runs_status_valid",
		"'dispatched'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("040 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// up extends without losing previous values (running stays vestigially valid).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'success'", "'failed'", "'cancelled'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("040 up.sql must preserve %q in the extended CHECK; content: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("040_add_apply_runs_dispatched_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down moves dispatched rows to running BEFORE narrowing the CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'running' WHERE status = 'dispatched'") {
		t.Errorf("040 down.sql must migrate dispatched→running before tightening CHECK; content: %.400s", dbody)
	}
	// The restored CHECK doesn't carry dispatched (form 025).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'))") {
		t.Errorf("040 down.sql restored CHECK not in the 025 form; content: %.400s", dbody)
	}
}

// TestEmbed_ApplyRunsOrphanedStatus -- sanity on 044 (Soul-reconcile, ADR-027(g),
// S6): the apply_runs.status enum gets extended with the terminal `orphaned`. up additively
// adds the value to the CHECK (drop+recreate, as in 040), preserving the entire prior
// set including dispatched, and extends purge_apply_runs with orphaned; down
// moves orphaned rows to failed and narrows the CHECK back to the 040 form (without
// orphaned), restoring purge_apply_runs without orphaned.
func TestEmbed_ApplyRunsOrphanedStatus(t *testing.T) {
	b, err := FS.ReadFile("044_add_apply_runs_orphaned_status.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"DROP CONSTRAINT apply_runs_status_valid",
		"ADD CONSTRAINT apply_runs_status_valid",
		"'orphaned'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("044 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// up is additive -- doesn't lose the previous status set (including dispatched).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'dispatched'", "'success'", "'failed'", "'cancelled'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("044 up.sql must preserve %q in the extended CHECK; content: %.400s", frag, body)
		}
	}
	// purge_apply_runs is extended with orphaned (a finished terminal).
	if !strings.Contains(body, "status IN ('success', 'failed', 'cancelled', 'orphaned')") {
		t.Errorf("044 up.sql must extend purge_apply_runs with orphaned; content: %.600s", body)
	}

	d, err := FS.ReadFile("044_add_apply_runs_orphaned_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down moves orphaned rows to failed BEFORE narrowing the CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'failed' WHERE status = 'orphaned'") {
		t.Errorf("044 down.sql must migrate orphaned→failed before tightening CHECK; content: %.400s", dbody)
	}
	// The restored CHECK doesn't carry orphaned (form 040, dispatched preserved).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'))") {
		t.Errorf("044 down.sql restored CHECK not in the 040 form; content: %.400s", dbody)
	}
	// purge_apply_runs is narrowed back (without orphaned).
	if !strings.Contains(dbody, "status IN ('success', 'failed', 'cancelled')") {
		t.Errorf("044 down.sql must restore purge_apply_runs without orphaned; content: %.600s", dbody)
	}
}

// TestEmbed_ApplyRunsNoMatchStatus -- sanity on 045 (FINDING-01 variant (b)):
// the apply_runs.status enum gets extended with the terminal `no_match` ON TOP OF 044 (orphaned).
// up additively adds the value to the CHECK (drop+recreate, as in 040/044), preserving
// the entire prior set including orphaned, and extends purge_apply_runs with no_match;
// down moves no_match rows to success (the pre-reform terminal for a non-target
// host) and narrows the CHECK back to the 044 form (without no_match), restoring
// purge_apply_runs without no_match. IMPORTANT: orphaned (044) is not broken -- down does NOT touch
// orphaned rows and keeps orphaned in the restored CHECK.
func TestEmbed_ApplyRunsNoMatchStatus(t *testing.T) {
	b, err := FS.ReadFile("045_add_apply_runs_no_match_status.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"DROP CONSTRAINT apply_runs_status_valid",
		"ADD CONSTRAINT apply_runs_status_valid",
		"'no_match'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("045 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// up is additive -- doesn't lose the previous status set (including orphaned from 044).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'dispatched'", "'success'", "'failed'", "'cancelled'", "'orphaned'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("045 up.sql must preserve %q in the extended CHECK; content: %.400s", frag, body)
		}
	}
	// purge_apply_runs is extended with no_match (a finished terminal), orphaned is preserved.
	if !strings.Contains(body, "status IN ('success', 'failed', 'cancelled', 'orphaned', 'no_match')") {
		t.Errorf("045 up.sql must extend purge_apply_runs with no_match (preserving orphaned); content: %.600s", body)
	}

	d, err := FS.ReadFile("045_add_apply_runs_no_match_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down moves no_match rows to success BEFORE narrowing the CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'success' WHERE status = 'no_match'") {
		t.Errorf("045 down.sql must migrate no_match→success before tightening CHECK; content: %.400s", dbody)
	}
	// down does NOT touch orphaned rows (to not break 044).
	if strings.Contains(dbody, "WHERE status = 'orphaned'") {
		t.Errorf("045 down.sql must NOT touch orphaned rows; content: %.400s", dbody)
	}
	// The restored CHECK doesn't carry no_match, but KEEPS orphaned (form 044).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'))") {
		t.Errorf("045 down.sql restored CHECK not in the 044 form (with orphaned, without no_match); content: %.400s", dbody)
	}
	// purge_apply_runs is narrowed back (without no_match, orphaned preserved).
	if !strings.Contains(dbody, "status IN ('success', 'failed', 'cancelled', 'orphaned')") {
		t.Errorf("045 down.sql must restore purge_apply_runs without no_match (keeping orphaned); content: %.600s", dbody)
	}
}

// TestEmbed_RBACTables -- sanity on 026 (ADR-028): three rbac_* tables with
// CHECK on role name format, FK to operators(aid) and ON DELETE CASCADE.
func TestEmbed_RBACTables(t *testing.T) {
	b, err := FS.ReadFile("026_create_rbac.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE rbac_roles",
		"CREATE TABLE rbac_role_permissions",
		"CREATE TABLE rbac_role_operators",
		"rbac_roles_name_format",
		"rbac_roles_created_by_aid_fk",
		"rbac_role_permissions_role_fk",
		"rbac_role_operators_role_fk",
		"rbac_role_operators_aid_fk",
		"rbac_role_operators_granted_by_aid_fk",
		"ON DELETE CASCADE",
		"rbac_role_operators_aid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("026 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("026_create_rbac.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP TABLE IF EXISTS rbac_role_operators",
		"DROP TABLE IF EXISTS rbac_role_permissions",
		"DROP TABLE IF EXISTS rbac_roles",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("026 down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_SynodTables -- sanity on 069 (ADR-049): three synod* tables (Archon ->
// Synod -> Roles) with the same rbac_* pattern: CHECK on name format (as rbac_roles),
// FK to operators(aid)/rbac_roles(name), ON DELETE CASCADE on both sides of the bundle,
// index on synod_operators(aid) for snapshot unrolling.
func TestEmbed_SynodTables(t *testing.T) {
	b, err := FS.ReadFile("069_create_synods.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE synods",
		"CREATE TABLE synod_operators",
		"CREATE TABLE synod_roles",
		"synods_name_format",
		"synods_created_by_aid_fk",
		"synod_operators_synod_fk",
		"synod_operators_aid_fk",
		"synod_operators_added_by_aid_fk",
		"synod_roles_synod_fk",
		"synod_roles_role_fk",
		"synod_roles_granted_by_aid_fk",
		"REFERENCES synods (name) ON DELETE CASCADE",
		"REFERENCES rbac_roles (name) ON DELETE CASCADE",
		"ON DELETE CASCADE",
		"synod_operators_aid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("069 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("069_create_synods.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP TABLE IF EXISTS synod_roles",
		"DROP TABLE IF EXISTS synod_operators",
		"DROP TABLE IF EXISTS synods",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("069 down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_SeedClusterAdmin -- sanity on 027 (ADR-028(b), E1): idempotent
// INSERT of the cluster-admin role (builtin=true) + `*` permission via
// ON CONFLICT DO NOTHING.
func TestEmbed_SeedClusterAdmin(t *testing.T) {
	b, err := FS.ReadFile("027_seed_cluster_admin.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"INSERT INTO rbac_roles",
		"'cluster-admin'",
		"true",
		"ON CONFLICT (name) DO NOTHING",
		"INSERT INTO rbac_role_permissions",
		"'*'",
		"ON CONFLICT (role_name, permission) DO NOTHING",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("027 up.sql missing %q; content: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("027_seed_cluster_admin.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DELETE FROM rbac_roles WHERE name = 'cluster-admin'") {
		t.Errorf("027 down.sql does not delete cluster-admin; content: %.200s", string(d))
	}
}

// TestEmbed_PluginSigilsTable -- sanity on 028 (ADR-026): registry of plugin_sigils
// (Keeper-signed plugin allow-list) with a partial UNIQUE index on active
// records (namespace, name, ref WHERE revoked_at IS NULL), CHECK on the
// sha256 format (hex), BYTEA signature + JSONB manifest, RESTRICT FK
// allowed_by_aid (NOT NULL) and SET NULL FK revoked_by_aid (NULL) to operators.
func TestEmbed_PluginSigilsTable(t *testing.T) {
	b, err := FS.ReadFile("028_create_plugin_sigils.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE plugin_sigils",
		"signature       BYTEA       NOT NULL",
		"manifest        JSONB       NOT NULL",
		"plugin_sigils_sha256_format",
		"plugin_sigils_allowed_by_aid_fk",
		"plugin_sigils_revoked_by_aid_fk",
		"ON DELETE SET NULL",
		"plugin_sigils_allowed_by_aid_idx",
		"CREATE UNIQUE INDEX plugin_sigils_active_idx",
		"WHERE revoked_at IS NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("028 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("028_create_plugin_sigils.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS plugin_sigils") {
		t.Errorf("028 down.sql does not drop plugin_sigils; content: %.200s", string(d))
	}
}

// TestEmbed_ApplyRunsRecipe -- sanity on 029 (ADR-027(c)(f)): additive
// nullable apply_runs.recipe column (JSONB) for the just-in-time rendering of
// a job by the Acolyte on claim. up adds the column + COMMENT, down drops it.
func TestEmbed_ApplyRunsRecipe(t *testing.T) {
	b, err := FS.ReadFile("029_add_apply_runs_recipe.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE apply_runs",
		"ADD COLUMN recipe JSONB",
		"COMMENT ON COLUMN apply_runs.recipe",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("029 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("029_add_apply_runs_recipe.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS recipe") {
		t.Errorf("029 down.sql does not drop recipe; content: %.200s", string(d))
	}
}

// TestEmbed_PluginSigilsManifestRaw -- sanity on 030 (M1 storage): additive
// nullable plugin_sigils.manifest_raw column (BYTEA) for the byte-exact canonical
// signed manifest.yaml (verify/broadcast, ADR-026). up adds the column
// + COMMENT, down drops it.
func TestEmbed_PluginSigilsManifestRaw(t *testing.T) {
	b, err := FS.ReadFile("030_add_plugin_sigils_manifest_raw.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE plugin_sigils",
		"ADD COLUMN manifest_raw BYTEA",
		"COMMENT ON COLUMN plugin_sigils.manifest_raw",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("030 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("030_add_plugin_sigils_manifest_raw.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS manifest_raw") {
		t.Errorf("030 down.sql does not drop manifest_raw; content: %.200s", string(d))
	}
}

// TestEmbed_PluginSigilsCommitSha -- sanity on 038 (A1-S3): additive
// nullable plugin_sigils.commit_sha column (TEXT) -- audit label for the
// binary's origin (git commit, ADR-026(g)), OUTSIDE the signed block. up adds the
// column + COMMENT, down drops it.
func TestEmbed_PluginSigilsCommitSha(t *testing.T) {
	b, err := FS.ReadFile("038_add_plugin_sigils_commit_sha.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE plugin_sigils",
		"ADD COLUMN commit_sha TEXT",
		"COMMENT ON COLUMN plugin_sigils.commit_sha",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("038 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("038_add_plugin_sigils_commit_sha.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS commit_sha") {
		t.Errorf("038 down.sql does not drop commit_sha; content: %.200s", string(d))
	}
}

// TestEmbed_IncarnationArchiveTables -- sanity on 039 (S-D3, cascade V3): two
// archive tables (incarnation_archive / state_history_archive) with an
// archived_at column and NO FK to the live incarnation (to survive DELETE+CASCADE).
// up creates both tables + indexes; up must NOT have an FK to incarnation
// (REFERENCES incarnation) -- otherwise the archive wouldn't survive a cascading
// delete. down drops both.
func TestEmbed_IncarnationArchiveTables(t *testing.T) {
	b, err := FS.ReadFile("039_create_incarnation_archive.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE incarnation_archive",
		"CREATE TABLE state_history_archive",
		"archived_at",
		"incarnation_archive_name_idx",
		"state_history_archive_incarnation_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("039 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// Cascade V3 invariant: NO FK to the live incarnation -- the archive survives DELETE.
	if strings.Contains(body, "REFERENCES incarnation") {
		t.Errorf("039 up.sql must NOT reference live incarnation (archive survives cascade); content head: %.400s", body)
	}
	d, err := FS.ReadFile("039_create_incarnation_archive.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP TABLE IF EXISTS state_history_archive",
		"DROP TABLE IF EXISTS incarnation_archive",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("039 down.sql missing %q; content: %.200s", frag, dbody)
		}
	}
}

// TestEmbed_OracleCircuitTable -- sanity on 042 (ADR-030(a), circuit breaker S4):
// per-decree fixed-window counter `oracle_circuit` with PK decree, CASCADE FK to
// decrees (re-enable = delete+recreate clears the window) and window_start /
// fire_count columns for an atomic UPSERT increment.
func TestEmbed_OracleCircuitTable(t *testing.T) {
	b, err := FS.ReadFile("042_create_oracle_circuit.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE oracle_circuit",
		"decree        TEXT        PRIMARY KEY",
		"window_start  TIMESTAMPTZ NOT NULL",
		"fire_count    INT         NOT NULL DEFAULT 0",
		"oracle_circuit_decree_fk",
		"REFERENCES decrees (name) ON DELETE CASCADE",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("042 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("042_create_oracle_circuit.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS oracle_circuit") {
		t.Errorf("042 down.sql does not drop oracle_circuit; content: %.200s", string(d))
	}
}

// TestEmbed_OmensTable -- sanity on 032 (ADR-025, Augur SS4.1): registry of omens
// (external systems) with CHECK on name format (kebab), closed CHECK on
// the source_type enum (vault/prometheus/elk), SET NULL FK to operators and an index
// on created_by_aid.
func TestEmbed_OmensTable(t *testing.T) {
	b, err := FS.ReadFile("032_create_omens.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE omens",
		"omens_name_format",
		"omens_source_type_enum",
		"'vault', 'prometheus', 'elk'",
		"omens_created_by_aid_fk",
		"ON DELETE SET NULL",
		"omens_created_by_aid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("032 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("032_create_omens.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS omens") {
		t.Errorf("032 down.sql does not drop omens; content: %.200s", string(d))
	}
}

// TestEmbed_RitesTable -- sanity on 033 (ADR-025, Augur SS4.2): registry of rites
// (grants) with an IDENTITY PK, CASCADE FK to omens, XOR CHECK on the subject
// (coven/sid), CHECK on token fields implying delegate, JSONB allow and three indexes
// (omen / partial sid / partial coven).
func TestEmbed_RitesTable(t *testing.T) {
	b, err := FS.ReadFile("033_create_rites.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE rites",
		"GENERATED ALWAYS AS IDENTITY PRIMARY KEY",
		"allow          JSONB       NOT NULL",
		"rites_omen_fk",
		"ON DELETE CASCADE",
		"rites_subject_xor",
		"(coven IS NOT NULL) <> (sid IS NOT NULL)",
		"rites_coven_format",
		"rites_token_fields_vault_only",
		"rites_created_by_aid_fk",
		"ON DELETE SET NULL",
		"rites_omen_idx",
		"CREATE INDEX rites_sid_idx",
		"CREATE INDEX rites_coven_idx",
		"WHERE sid IS NOT NULL",
		"WHERE coven IS NOT NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("033 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("033_create_rites.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS rites") {
		t.Errorf("033 down.sql does not drop rites; content: %.200s", string(d))
	}
}

// TestEmbed_ServiceRegistryTable -- sanity on 034 (managed service registry):
// the service_registry table with PK name (kebab CHECK), nonempty CHECK on git/ref,
// nullable refresh and two FKs to operators (created_by_aid / updated_by_aid).
func TestEmbed_ServiceRegistryTable(t *testing.T) {
	b, err := FS.ReadFile("034_create_service_registry.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE service_registry",
		"service_registry_name_format",
		"service_registry_git_nonempty",
		"service_registry_ref_nonempty",
		"service_registry_created_by_fk",
		"service_registry_updated_by_fk",
		"updated_at",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("034 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("034_create_service_registry.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS service_registry") {
		t.Errorf("034 down.sql does not drop service_registry; content: %.200s", string(d))
	}
}

// TestEmbed_KeeperSettingsTable -- sanity on 035 (cluster-wide key-value):
// the keeper_settings table with PK key (snake CHECK), NOT NULL value and SET NULL FK
// updated_by_aid to operators. The migration does NOT insert well-known rows.
func TestEmbed_KeeperSettingsTable(t *testing.T) {
	b, err := FS.ReadFile("035_create_keeper_settings.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE keeper_settings",
		"keeper_settings_key_format",
		"value          TEXT        NOT NULL",
		"ON DELETE SET NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("035 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// The migration must not seed well-known keys (runtime data).
	if strings.Contains(body, "INSERT INTO keeper_settings") {
		t.Errorf("035 up.sql must not seed settings rows; content head: %.400s", body)
	}
	d, err := FS.ReadFile("035_create_keeper_settings.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS keeper_settings") {
		t.Errorf("035 down.sql does not drop keeper_settings; content: %.200s", string(d))
	}
}

// TestEmbed_SigilSigningKeysTable -- sanity on 037 (ADR-026(h), R3 multi-anchor):
// registry of trust-anchor signing keys for the Sigil. The invariant "exactly one primary among
// active" is materialized by a partial UNIQUE index on (is_primary) WHERE
// status='active' AND is_primary; CHECK on the status enum (active/retired); both FKs
// (introduced_by_aid / retired_by_aid) to operators with ON DELETE SET NULL.
// There is NO private-key column -- only the public part + a vault_ref (security invariant).
func TestEmbed_SigilSigningKeysTable(t *testing.T) {
	b, err := FS.ReadFile("037_create_sigil_signing_keys.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE sigil_signing_keys",
		"key_id            TEXT        NOT NULL UNIQUE",
		"pubkey_pem        TEXT        NOT NULL",
		"vault_ref         TEXT        NOT NULL",
		"is_primary        BOOLEAN     NOT NULL DEFAULT false",
		"sigil_signing_keys_status_enum",
		"CHECK (status IN ('active', 'retired'))",
		"sigil_signing_keys_introduced_by_fk",
		"sigil_signing_keys_retired_by_fk",
		"ON DELETE SET NULL",
		"CREATE UNIQUE INDEX sigil_signing_keys_one_primary",
		"WHERE status = 'active' AND is_primary",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("037 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	// Security invariant: the private key NEVER goes into PG. No private key
	// columns (private/secret_key/privkey) -- only the public part + a Vault reference.
	for _, forbidden := range []string{"private_key", "privkey", "secret_key", "private_pem"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("037 up.sql must NOT store private key material; found %q", forbidden)
		}
	}
	d, err := FS.ReadFile("037_create_sigil_signing_keys.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS sigil_signing_keys") {
		t.Errorf("037 down.sql does not drop sigil_signing_keys; content: %.200s", string(d))
	}
}

// TestEmbed_StateHistoryArchivedAt -- sanity on 048 (ADR-Q19 retention):
// additive nullable `state_history.archived_at` column (soft-delete flag)
// + partial index on WHERE archived_at IS NULL for filtering active snapshots.
// up adds a TIMESTAMPTZ column and CREATE INDEX state_history_active_idx;
// down drops the index and column (reversible, soft-deleted snapshots physically
// remain -- become indistinguishable from active ones).
func TestEmbed_StateHistoryArchivedAt(t *testing.T) {
	b, err := FS.ReadFile("048_state_history_archived_at.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE state_history",
		"ADD COLUMN archived_at TIMESTAMPTZ",
		"CREATE INDEX state_history_active_idx",
		"WHERE archived_at IS NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("048 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("048_state_history_archived_at.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS state_history_active_idx",
		"DROP COLUMN IF EXISTS archived_at",
	} {
		if !strings.Contains(dbody, frag) {
			t.Errorf("048 down.sql missing %q; content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_ArchiveStateHistoryFunction -- sanity on 049 (ADR-Q19 retention):
// the `archive_state_history(integer, boolean, integer)` SQL function marks
// `archived_at = NOW()` for active state_history snapshots beyond N
// most recent per incarnation; when keep_version_bump=true excludes snapshots
// of state_schema migration steps (scenario='migration'). up creates the function;
// down drops it.
func TestEmbed_ArchiveStateHistoryFunction(t *testing.T) {
	b, err := FS.ReadFile("049_create_archive_state_history.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION archive_state_history(",
		"keep_last_n        integer",
		"keep_version_bump  boolean",
		"batch              integer",
		"row_number() OVER (",
		"PARTITION BY incarnation_name",
		"archived_at IS NULL",
		"rn > keep_last_n",
		"scenario <> 'migration'",
		"SET archived_at = NOW()",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("049 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("049_create_archive_state_history.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS archive_state_history(integer, boolean, integer)") {
		t.Errorf("049 down.sql does not drop archive_state_history; content: %.200s", string(d))
	}
}

// TestEmbed_IncarnationDriftScanColumns -- sanity on 050 (ADR-031 Slice C):
// the migration adds `last_drift_check_at` / `last_drift_summary` columns to
// `incarnation` and a partial index. Down drops the columns (the index goes with them).
func TestEmbed_IncarnationDriftScanColumns(t *testing.T) {
	b, err := FS.ReadFile("050_add_incarnation_drift_scan.up.sql")
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE incarnation",
		"ADD COLUMN last_drift_check_at TIMESTAMPTZ",
		"ADD COLUMN last_drift_summary  JSONB",
		"CREATE INDEX incarnation_last_drift_check_at_idx",
		"WHERE last_drift_check_at IS NOT NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("050 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("050_add_incarnation_drift_scan.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP COLUMN IF EXISTS last_drift_summary",
		"DROP COLUMN IF EXISTS last_drift_check_at",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("050 down.sql missing %q; content: %.200s", frag, dstr)
		}
	}
}

// TestEmbed_SoulsSshTarget -- sanity on 053 (ADR-032 amendment 2026-05-26, S7-1):
// adds the souls.ssh_target column (jsonb) + a shape-guard CHECK
// souls_ssh_target_shape (types of ssh_port/ssh_user/soul_path when non-NULL).
// up creates the column and constraint; down drops the constraint and column.
func TestEmbed_SoulsSshTarget(t *testing.T) {
	b, err := FS.ReadFile("053_add_souls_ssh_target.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE souls ADD COLUMN IF NOT EXISTS ssh_target jsonb",
		"ADD CONSTRAINT souls_ssh_target_shape",
		"jsonb_typeof(ssh_target->'ssh_port') = 'number'",
		"jsonb_typeof(ssh_target->'ssh_user') = 'string'",
		"jsonb_typeof(ssh_target->'soul_path') = 'string'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("053 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("053_add_souls_ssh_target.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS souls_ssh_target_shape",
		"DROP COLUMN IF EXISTS ssh_target",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("053 down.sql missing %q; content: %.200s", frag, dstr)
		}
	}
}

// TestEmbed_SoulsSshTarget_SSHProvider -- sanity on 056 (ADR-032 amendment
// 2026-05-27, P2 W-1): extended `souls_ssh_target_shape` CHECK with an optional
// `ssh_provider` (kebab-case regex). up recreates the constraint with the regex
// `^[a-z][a-z0-9-]{0,62}$`, down restores the previous one (without ssh_provider).
func TestEmbed_SoulsSshTarget_SSHProvider(t *testing.T) {
	b, err := FS.ReadFile("056_add_ssh_provider_to_ssh_target.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS souls_ssh_target_shape",
		"ADD CONSTRAINT souls_ssh_target_shape",
		"ssh_target ? 'ssh_provider'",
		"jsonb_typeof(ssh_target->'ssh_provider') = 'string'",
		"'^[a-z][a-z0-9-]{0,62}$'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("056 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("056_add_ssh_provider_to_ssh_target.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS souls_ssh_target_shape",
		"ADD CONSTRAINT souls_ssh_target_shape",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("056 down.sql missing %q; content: %.200s", frag, dstr)
		}
	}
}

// TestEmbed_TidesTable -- sanity on 055 (ADR-040 amendment 2026-05-27, W-1):
// registry of `tides` (top-level invocation-time chunking) with CHECK invariants
// (status / on_surge_failure / running implies claim-NOT-NULL / surge_index <= total),
// FK to operators(aid), two partial indexes (claim_scan / pending_pickup) +
// back-link columns apply_runs.tide_id / surge_index with a partial index.
func TestEmbed_TidesTable(t *testing.T) {
	b, err := FS.ReadFile("055_create_tides.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE tides",
		"tide_id                TEXT PRIMARY KEY",
		"target_resolved_souls  JSONB NOT NULL",
		"tides_total_surges_positive",
		"tides_surge_size_positive",
		"tides_concurrency_override_positive",
		"tides_on_surge_failure_valid",
		"'abort'",
		"'continue'",
		"tides_status_valid",
		"'pending'",
		"'running'",
		"'succeeded'",
		"'failed'",
		"'partial_failed'",
		"'cancelled'",
		"tides_running_claim_consistency",
		"tides_surge_index_within_total",
		"tides_started_by_aid_fk",
		"FOREIGN KEY (started_by_aid) REFERENCES operators (aid)",
		"CREATE INDEX tides_claim_scan_idx",
		"CREATE INDEX tides_pending_pickup_idx",
		"ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS tide_id TEXT",
		"ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS surge_index INT",
		"apply_runs_tide_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("055 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("055_create_tides.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS apply_runs_tide_idx",
		"DROP COLUMN IF EXISTS surge_index",
		"DROP COLUMN IF EXISTS tide_id",
		"DROP TABLE IF EXISTS tides",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("055 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_DropTides -- sanity on 061 (Wave 5 Pass 1): full removal of Tide.
// up drops the `tides` registry + apply_runs back-link columns (tide_id/surge_index)
// + partial index; down recreates the schema per the 055 pattern.
func TestEmbed_DropTides(t *testing.T) {
	b, err := FS.ReadFile("061_drop_tides.up.sql")
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	up := string(b)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS apply_runs_tide_idx",
		"DROP COLUMN IF EXISTS surge_index",
		"DROP COLUMN IF EXISTS tide_id",
		"DROP TABLE IF EXISTS tides",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("061 up.sql missing %q; content: %.300s", frag, up)
		}
	}
	d, err := FS.ReadFile("061_drop_tides.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	down := string(d)
	for _, frag := range []string{
		"CREATE TABLE tides",
		"ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS tide_id TEXT",
		"ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS surge_index INT",
		"apply_runs_tide_idx",
	} {
		if !strings.Contains(down, frag) {
			t.Errorf("061 down.sql missing %q; content head: %.400s", frag, down)
		}
	}
}

// TestEmbed_ErrandRuns_TableShape -- sanity on 057 (ADR-041, E6-1): registry of
// `errand_runs` (top-level multi-target pull-ad-hoc invocation) with
// CHECK invariants (status / on_failure / concurrency / total / done bounds /
// attempt / running implies claim-NOT-NULL / terminal implies finished_at-NOT-NULL), FK to
// operators(aid) and two partial indexes (pending_pickup / claim_scan).
func TestEmbed_ErrandRuns_TableShape(t *testing.T) {
	b, err := FS.ReadFile("057_create_errand_runs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE errand_runs",
		"errand_run_id          TEXT        PRIMARY KEY",
		"target_resolved_souls  JSONB       NOT NULL",
		"concurrency            INT         NOT NULL DEFAULT 50",
		"on_failure             TEXT        NOT NULL DEFAULT 'continue'",
		"total_errands          INT         NOT NULL",
		"current_done           INT         NOT NULL DEFAULT 0",
		"status                 TEXT        NOT NULL DEFAULT 'pending'",
		"errand_runs_status_valid",
		"'pending'",
		"'running'",
		"'succeeded'",
		"'failed'",
		"'partial_failed'",
		"'cancelled'",
		"errand_runs_on_failure_valid",
		"'abort'",
		"'continue'",
		"errand_runs_concurrency_positive",
		"concurrency >= 1 AND concurrency <= 500",
		"errand_runs_total_positive",
		"total_errands >= 1",
		"errand_runs_done_bounds",
		"current_done >= 0 AND current_done <= total_errands",
		"errand_runs_attempt_positive",
		"errand_runs_running_claim_consistency",
		"errand_runs_terminal_finished_at",
		"errand_runs_started_by_aid_fk",
		"FOREIGN KEY (started_by_aid) REFERENCES operators (aid)",
		"CREATE INDEX errand_runs_pending_pickup_idx",
		"WHERE status = 'pending'",
		"CREATE INDEX errand_runs_claim_scan_idx",
		"WHERE status = 'running'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("057 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("057_create_errand_runs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS errand_runs_claim_scan_idx",
		"DROP INDEX IF EXISTS errand_runs_pending_pickup_idx",
		"DROP TABLE IF EXISTS errand_runs",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("057 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_Errands_HasErrandRunIdColumn -- sanity on 057 (ADR-041, E6-1):
// back-link errands.errand_run_id (NULLABLE) + FK CASCADE to
// errand_runs(errand_run_id) + partial index errands_errand_run_id_idx;
// down carefully drops the FK/index/column.
func TestEmbed_Errands_HasErrandRunIdColumn(t *testing.T) {
	b, err := FS.ReadFile("057_create_errand_runs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE errands ADD COLUMN errand_run_id TEXT",
		"ALTER TABLE errands ADD CONSTRAINT errands_errand_run_id_fkey",
		"FOREIGN KEY (errand_run_id) REFERENCES errand_runs (errand_run_id) ON DELETE CASCADE",
		"CREATE INDEX errands_errand_run_id_idx",
		"WHERE errand_run_id IS NOT NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("057 up.sql missing back-link %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("057_create_errand_runs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS errands_errand_run_id_idx",
		"DROP CONSTRAINT IF EXISTS errands_errand_run_id_fkey",
		"DROP COLUMN IF EXISTS errand_run_id",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("057 down.sql missing back-link %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_VoyagesTable -- sanity on 059 (ADR-043, S1): registry of `voyages`
// (unified batch run) with a discriminator kind=scenario|command,
// CHECK invariants (kind / status including scheduled / on_failure /
// kind<->payload consistency / running implies claim-NOT-NULL / terminal implies finished_at /
// batch_index <= total), FK to operators(aid) and two partial indexes
// (pending_pickup / claim_scan).
func TestEmbed_VoyagesTable(t *testing.T) {
	b, err := FS.ReadFile("059_create_voyages.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE voyages",
		"voyage_id              TEXT        PRIMARY KEY",
		"target_resolved        JSONB       NOT NULL",
		"input                  JSONB       NOT NULL DEFAULT '{}'",
		"dry_run                BOOLEAN     NOT NULL DEFAULT false",
		"voyages_kind_valid",
		"kind IN ('scenario', 'command')",
		"voyages_status_valid",
		"'scheduled'",
		"'pending'",
		"'running'",
		"'succeeded'",
		"'failed'",
		"'partial_failed'",
		"'cancelled'",
		"voyages_on_failure_valid",
		"'abort'",
		"'continue'",
		"voyages_kind_payload_consistency",
		"voyages_batch_index_within_total",
		"voyages_running_claim_consistency",
		"voyages_terminal_finished_at",
		"voyages_started_by_aid_fk",
		"FOREIGN KEY (started_by_aid) REFERENCES operators (aid)",
		"CREATE INDEX voyages_pending_pickup_idx",
		"WHERE status = 'pending'",
		"CREATE INDEX voyages_claim_scan_idx",
		"WHERE status = 'running'",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("059 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("059_create_voyages.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS voyages_claim_scan_idx",
		"DROP INDEX IF EXISTS voyages_pending_pickup_idx",
		"DROP TABLE IF EXISTS voyages",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("059 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_VoyageTargetsTable -- sanity on 059 (ADR-043, S1): the
// `voyage_targets` table (run units / Leg split) with composite PK
// (voyage_id, target_kind, target_id), CHECK on target_kind/status,
// CASCADE FK to voyages(voyage_id) and an index (voyage_id, batch_index).
func TestEmbed_VoyageTargetsTable(t *testing.T) {
	b, err := FS.ReadFile("059_create_voyages.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE voyage_targets",
		"PRIMARY KEY (voyage_id, target_kind, target_id)",
		"voyage_targets_target_kind_valid",
		"target_kind IN ('incarnation', 'sid')",
		"voyage_targets_status_valid",
		"'awaiting'",
		"'no_match'",
		"voyage_targets_voyage_fk",
		"REFERENCES voyages (voyage_id) ON DELETE CASCADE",
		"CREATE INDEX voyage_targets_batch_idx",
		"(voyage_id, batch_index)",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("059 up.sql missing voyage_targets %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("059_create_voyages.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS voyage_targets_batch_idx",
		"DROP TABLE IF EXISTS voyage_targets",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("059 down.sql missing voyage_targets %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_CadencesTable -- sanity on 066 (ADR-046, S1): registry of `cadences`
// (a schedule spawning Voyage) with CHECK invariants (schedule_kind / overlap_policy
// / kind / schedule_consistency interval<->cron XOR / kind<->payload consistency /
// sane bounds), FK to operators(aid) and a partial index for the due scan.
func TestEmbed_CadencesTable(t *testing.T) {
	b, err := FS.ReadFile("066_create_cadences.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE cadences",
		"id                     TEXT        PRIMARY KEY",
		"enabled                BOOLEAN     NOT NULL DEFAULT true",
		"target                 JSONB       NOT NULL",
		"input                  JSONB       NOT NULL DEFAULT '{}'",
		"cadences_schedule_kind_valid",
		"schedule_kind IN ('interval', 'cron')",
		"cadences_overlap_policy_valid",
		"overlap_policy IN ('skip', 'queue', 'parallel')",
		"cadences_kind_valid",
		"kind IN ('scenario', 'command')",
		"cadences_schedule_consistency",
		"cadences_kind_payload_consistency",
		"cadences_interval_seconds_positive",
		"cadences_batch_mode_valid",
		"cadences_batch_size_positive",
		"cadences_batch_percent_range",
		"cadences_concurrency_positive",
		"cadences_fail_threshold_positive",
		"cadences_on_failure_valid",
		"cadences_created_by_aid_fk",
		"FOREIGN KEY (created_by_aid) REFERENCES operators (aid)",
		"CREATE INDEX cadences_due_scan_idx",
		"WHERE enabled",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("066 up.sql missing %q; content head: %.600s", frag, body)
		}
	}
	d, err := FS.ReadFile("066_create_cadences.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS cadences_due_scan_idx",
		"DROP TABLE IF EXISTS cadences",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("066 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_VoyagesCadenceBackLink -- sanity on 066 (ADR-046 SS2): back-link
// voyages.cadence_id (NULLABLE) + FK ON DELETE SET NULL to cadences(id) +
// partial index voyages_cadence_id_idx; down drops the index/FK/column.
func TestEmbed_VoyagesCadenceBackLink(t *testing.T) {
	b, err := FS.ReadFile("066_create_cadences.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE voyages",
		"ADD COLUMN cadence_id TEXT",
		"ADD CONSTRAINT voyages_cadence_id_fk",
		"FOREIGN KEY (cadence_id) REFERENCES cadences (id) ON DELETE SET NULL",
		"CREATE INDEX voyages_cadence_id_idx",
		"WHERE cadence_id IS NOT NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("066 up.sql missing back-link %q; content head: %.600s", frag, body)
		}
	}
	d, err := FS.ReadFile("066_create_cadences.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS voyages_cadence_id_idx",
		"DROP CONSTRAINT IF EXISTS voyages_cadence_id_fk",
		"DROP COLUMN IF EXISTS cadence_id",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("066 down.sql missing back-link %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_CadencesIntervalFloor -- sanity on 068 (ADR-046 Pass B, floor limit):
// up carries a pre-flight data guard (RAISE when interval_seconds < 30 BEFORE ADD
// CONSTRAINT), the cadences_interval_seconds_floor CHECK (>= 30, a separate name -- does NOT
// redefine _positive from 066) and the cadences_enabled_interval_idx partial index
// for the MIN query; down drops the index + the floor CHECK (does not touch
// positive/due-scan from 066).
func TestEmbed_CadencesIntervalFloor(t *testing.T) {
	b, err := FS.ReadFile("068_cadences_interval_floor.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"DO $$",
		"IF EXISTS (SELECT 1 FROM cadences WHERE interval_seconds < 30)",
		"RAISE EXCEPTION",
		"ADD CONSTRAINT cadences_interval_seconds_floor",
		"CHECK (interval_seconds IS NULL OR interval_seconds >= 30)",
		"CREATE INDEX cadences_enabled_interval_idx",
		"ON cadences (interval_seconds)",
		"WHERE enabled",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("068 up.sql missing %q; content head: %.600s", frag, body)
		}
	}
	// The pre-flight data guard must precede ADD CONSTRAINT (fail-fast before
	// the constraint check -- a clear RAISE instead of a raw CHECK violation).
	if guardIdx, addIdx := strings.Index(body, "RAISE EXCEPTION"), strings.Index(body, "ADD CONSTRAINT cadences_interval_seconds_floor"); guardIdx < 0 || addIdx < 0 || guardIdx > addIdx {
		t.Errorf("068 up.sql: the data guard (RAISE) must precede ADD CONSTRAINT; guardIdx=%d addIdx=%d", guardIdx, addIdx)
	}
	// floor-CHECK -- a separate name, does NOT redefine positive from 066 (that one isn't dropped).
	if strings.Contains(body, "DROP CONSTRAINT") && strings.Contains(body, "cadences_interval_seconds_positive") {
		t.Errorf("068 up.sql must NOT redefine cadences_interval_seconds_positive; content: %.400s", body)
	}
	d, err := FS.ReadFile("068_cadences_interval_floor.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS cadences_enabled_interval_idx",
		"DROP CONSTRAINT IF EXISTS cadences_interval_seconds_floor",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("068 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
	// down does NOT touch 066 objects (positive CHECK / due-scan index / table).
	for _, forbidden := range []string{"cadences_interval_seconds_positive", "cadences_due_scan_idx", "DROP TABLE"} {
		if strings.Contains(dstr, forbidden) {
			t.Errorf("068 down.sql must NOT touch 066-object %q; content: %.300s", forbidden, dstr)
		}
	}
}

// TestEmbed_CadencesFailThresholdPercent -- sanity on 070 (ADR-043 amendment
// 2026-06-09, Cadence-recipe S3): additive cadences.fail_threshold_percent column
// (failure threshold as a percentage of spawn scope, symmetric with batch_percent from 066) +
// CHECK on the range [1, 100]. up -- ADD COLUMN + ADD CONSTRAINT range; down --
// DROP CONSTRAINT + DROP COLUMN, not touching 066 objects.
func TestEmbed_CadencesFailThresholdPercent(t *testing.T) {
	b, err := FS.ReadFile("070_cadences_fail_threshold_percent.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE cadences",
		"ADD COLUMN fail_threshold_percent INT",
		"ADD CONSTRAINT cadences_fail_threshold_percent_range",
		"fail_threshold_percent IS NULL OR (fail_threshold_percent >= 1 AND fail_threshold_percent <= 100)",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("070 up.sql missing %q; content head: %.600s", frag, body)
		}
	}
	// Additive (forward-compat): up does NOT drop prior objects. (066 column names
	// are fine in an explanatory comment -- we check specifically for the absence of DROP.)
	for _, forbidden := range []string{"DROP CONSTRAINT", "DROP TABLE", "DROP COLUMN"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("070 up.sql must NOT drop prior object (%q); content: %.400s", forbidden, body)
		}
	}
	d, err := FS.ReadFile("070_cadences_fail_threshold_percent.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS cadences_fail_threshold_percent_range",
		"DROP COLUMN IF EXISTS fail_threshold_percent",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("070 down.sql missing %q; content: %.300s", frag, dstr)
		}
	}
	// down does NOT touch 066/068 objects.
	for _, forbidden := range []string{"batch_percent", "cadences_interval_seconds_floor", "DROP TABLE"} {
		if strings.Contains(dstr, forbidden) {
			t.Errorf("070 down.sql must NOT touch prior object %q; content: %.300s", forbidden, dstr)
		}
	}
}

// TestEmbed_IncarnationChoirsTable -- sanity on 060 (ADR-044, S-T2): the
// `incarnation_choirs` table (Choir -- declared host topology within an incarnation) with
// composite PK (incarnation_name, choir_name), CHECK on choir_name format and
// min/max-size invariants, CASCADE FK to incarnation(name) and SET NULL FK to
// operators(aid).
func TestEmbed_IncarnationChoirsTable(t *testing.T) {
	b, err := FS.ReadFile("060_create_choirs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE incarnation_choirs",
		"PRIMARY KEY (incarnation_name, choir_name)",
		"incarnation_choirs_name_format",
		"^[a-z][a-z0-9_-]*$",
		"incarnation_choirs_min_size_positive",
		"incarnation_choirs_max_size_positive",
		"incarnation_choirs_min_le_max",
		"min_size <= max_size",
		"incarnation_choirs_incarnation_fk",
		"REFERENCES incarnation (name) ON DELETE CASCADE",
		"incarnation_choirs_created_by_aid_fk",
		"REFERENCES operators (aid) ON DELETE SET NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("060 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	d, err := FS.ReadFile("060_create_choirs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS incarnation_choirs") {
		t.Errorf("060 down.sql does not drop incarnation_choirs; content: %.300s", string(d))
	}
}

// TestEmbed_IncarnationChoirVoicesTable -- sanity on 060 (ADR-044, S-T2): the
// `incarnation_choir_voices` table (Voice -- SID membership in a Choir) with composite PK
// (incarnation_name, choir_name, sid), CASCADE FK to the incarnation_choirs pair and
// to souls(sid), SET NULL FK to operators(aid), an index on sid. Deliberately
// NO global UNIQUE(sid) (multi-incarnation, ADR-044 item 3).
func TestEmbed_IncarnationChoirVoicesTable(t *testing.T) {
	b, err := FS.ReadFile("060_create_choirs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE incarnation_choir_voices",
		"PRIMARY KEY (incarnation_name, choir_name, sid)",
		"incarnation_choir_voices_position_non_negative",
		"incarnation_choir_voices_choir_fk",
		"REFERENCES incarnation_choirs (incarnation_name, choir_name) ON DELETE CASCADE",
		"incarnation_choir_voices_sid_fk",
		"REFERENCES souls (sid) ON DELETE CASCADE",
		"incarnation_choir_voices_added_by_aid_fk",
		"REFERENCES operators (aid) ON DELETE SET NULL",
		"CREATE INDEX incarnation_choir_voices_sid_idx",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("060 up.sql missing voices %q; content head: %.500s", frag, body)
		}
	}
	// A global UNIQUE(sid) is forbidden by the model (a single SID can be a Voice
	// across different incarnations): pinning its absence.
	if strings.Contains(body, "UNIQUE (sid)") || strings.Contains(body, "UNIQUE(sid)") {
		t.Errorf("060 up.sql must NOT declare global UNIQUE(sid) (multi-incarnation); content: %.500s", body)
	}
	d, err := FS.ReadFile("060_create_choirs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS incarnation_choir_voices_sid_idx",
		"DROP TABLE IF EXISTS incarnation_choir_voices",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("060 down.sql missing voices %q; content: %.300s", frag, dstr)
		}
	}
}

// TestEmbed_TidingEphemeralPayload -- sanity on 072 (ADR-052 Amendment N1):
// `tidings` gets extended with four additive columns (ephemeral/voyage_id/
// annotations/projection), a CHECK invariant ephemeral<->voyage_id and a partial
// index on voyage_id WHERE ephemeral. up is additive (does not drop prior data);
// down drops the index, CHECK and all four columns.
func TestEmbed_TidingEphemeralPayload(t *testing.T) {
	b, err := FS.ReadFile("072_tiding_ephemeral_payload.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"ALTER TABLE tidings",
		"ADD COLUMN ephemeral",
		"ADD COLUMN voyage_id",
		"ADD COLUMN annotations JSONB",
		"ADD COLUMN projection",
		"TEXT[]",
		"ADD CONSTRAINT tidings_ephemeral_voyage_consistent",
		"CHECK (ephemeral = (voyage_id IS NOT NULL))",
		"CREATE INDEX tidings_ephemeral_voyage_idx",
		"ON tidings (voyage_id) WHERE ephemeral",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("072 up.sql missing %q; content head: %.500s", frag, body)
		}
	}
	// Additive (forward-compat): up must NOT drop prior `tidings` objects.
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "DROP INDEX"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("072 up.sql must NOT drop prior object (%q); content: %.400s", forbidden, body)
		}
	}
	d, err := FS.ReadFile("072_tiding_ephemeral_payload.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dstr := string(d)
	for _, frag := range []string{
		"DROP INDEX IF EXISTS tidings_ephemeral_voyage_idx",
		"DROP CONSTRAINT IF EXISTS tidings_ephemeral_voyage_consistent",
		"DROP COLUMN IF EXISTS projection",
		"DROP COLUMN IF EXISTS annotations",
		"DROP COLUMN IF EXISTS voyage_id",
		"DROP COLUMN IF EXISTS ephemeral",
	} {
		if !strings.Contains(dstr, frag) {
			t.Errorf("072 down.sql missing %q; content: %.400s", frag, dstr)
		}
	}
}

// TestEmbed_WarrantTable -- sanity on 092 (cert-rotation Var1): registry of warrant
// (incarnation service TLS certs) with a partial unique on (incarnation_id, kind)
// WHERE status='active', CASCADE FK to incarnation(name), CHECK on kind/status/
// fingerprint format, indexes on not_after (Reaper scan axis) and status
// (retention). down drops the table.
func TestEmbed_WarrantTable(t *testing.T) {
	b, err := FS.ReadFile("092_create_warrant.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE TABLE warrant",
		"cert_id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid()",
		"warrant_incarnation_fk",
		"REFERENCES incarnation (name) ON DELETE CASCADE",
		"warrant_kind_valid",
		"kind IN ('cert', 'key', 'ca')",
		"warrant_status_valid",
		"status IN ('active', 'superseded', 'expired', 'rotating', 'failed')",
		"warrant_fingerprint_format",
		"warrant_active_by_incarnation_kind_idx",
		"WHERE status = 'active'",
		"warrant_not_after_idx",
		"warrant_status_idx",
		"auto_rotate               BOOLEAN     NOT NULL DEFAULT true",
		"rotate_threshold_override INTERVAL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("092 up.sql missing %q; content head: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("092_create_warrant.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP TABLE IF EXISTS warrant") {
		t.Errorf("092 down.sql does not drop warrant; content: %.200s", d)
	}
}

// TestEmbed_PurgeOldCertsFunction -- sanity on 093 (R4, cert-rotation Var1):
// Reaper rule `purge_old_certs(text[], interval, integer)` -- DELETE warrant in
// the given statuses (superseded/expired/failed) with issued_at older than max_age;
// active/rotating is not touched (statuses filter). Parity with purge_old_seeds (013).
func TestEmbed_PurgeOldCertsFunction(t *testing.T) {
	b, err := FS.ReadFile("093_create_purge_old_certs.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"CREATE OR REPLACE FUNCTION purge_old_certs(",
		"DELETE FROM warrant",
		"status = ANY(target_statuses)",
		"issued_at < NOW() - max_age",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("093 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("093_create_purge_old_certs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP FUNCTION IF EXISTS purge_old_certs(text[], interval, integer)") {
		t.Errorf("093 down.sql does not drop purge_old_certs; content: %.200s", d)
	}
}

// TestEmbed_RenamePermissionRerunLast -- data fix 095: rename
// `incarnation.create-rerun` -> `incarnation.rerun-last` in rbac_role_permissions.
// The catalog was renamed without a deprecated alias -- without the migration a custom role with
// the old string would silently get a 403 on rerun-last.
func TestEmbed_RenamePermissionRerunLast(t *testing.T) {
	b, err := FS.ReadFile("095_rename_permission_rerun_last.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)
	for _, frag := range []string{
		"UPDATE rbac_role_permissions",
		"SET permission = 'incarnation.rerun-last'",
		"'incarnation.create-rerun'",
		"NOT EXISTS",
		"DELETE FROM rbac_role_permissions",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("095 up.sql missing %q; content head: %.300s", frag, body)
		}
	}
	d, err := FS.ReadFile("095_rename_permission_rerun_last.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "SET permission = 'incarnation.create-rerun'") {
		t.Errorf("095 down.sql does not restore old name; content: %.200s", d)
	}
}
