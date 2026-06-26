package migrations

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// TestEmbed_ContainsExpectedMigrations — smoke на то, что //go:embed
// захватил up/down пары и порядок стабилен. Полная проверка применения
// миграций — через testcontainers в M0.4.1.
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

// TestEmbed_UpSQLContainsCreateTable — sanity на содержимое (предотвращает
// пустой //go:embed, если кто-то случайно переместит .sql).
func TestEmbed_UpSQLContainsCreateTable(t *testing.T) {
	b, err := FS.ReadFile("001_create_audit_log.up.sql")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "CREATE TABLE audit_log") {
		t.Errorf("up.sql does not contain CREATE TABLE audit_log; content head: %.120s", b)
	}
}

// TestEmbed_PurgeAuditOldFunction — sanity на 002: //go:embed захватил
// up-миграцию и она объявляет PL/pgSQL-функцию по ADR-022(d).
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

// TestEmbed_OperatorsTable — sanity на 003: //go:embed захватил миграцию
// реестра operators (ADR-014) с partial unique index по
// `created_by_aid IS NULL` (инвариант единственного bootstrap-Archon-а).
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

// TestEmbed_OperatorsAuthMethodLDAPOIDC — sanity на 083 (ADR-058): only-add
// расширение CHECK auth_method_valid значениями `ldap`/`oidc`. Up расширяет
// набор, down возвращает к прежнему (jwt/mtls/combined).
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

// TestEmbed_OperatorsCreatedVia — sanity на 084 (ADR-058(d)): колонка
// created_via с CHECK created_via_valid + reconcile bootstrap-строки.
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

// TestEmbed_OperatorsBootstrapIndex — sanity на 085 (ADR-058(d)): перенос
// bootstrap-инварианта на created_via='bootstrap'; down возвращает на
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

// TestEmbed_SeedArchonSystem — sanity на 086 (ADR-058(d)): посев
// системного оператора archon-system (created_via='system').
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

// TestEmbed_IncarnationTable — sanity на 005: реестр incarnation
// (ADR-009) c CHECK на status / name_format и FK на operators.
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

// TestEmbed_StateHistoryTable — sanity на 006: журнал state_history
// (ADR-009 / ADR-019) с CASCADE-FK на incarnation и индексами для типовых
// запросов истории.
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

// TestEmbed_SoulsTable — sanity на 007: реестр souls (ADR-002 / ADR-012)
// с CHECK на status/transport/sid-format, GIN-индексом по coven, FK на
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

// TestEmbed_BootstrapTokensTable — sanity на 008: реестр одноразовых
// SoulSeed-токенов с partial unique index по `used_at IS NULL`, FK на
// souls (ON DELETE CASCADE) и operators (ON DELETE SET NULL).
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

// TestEmbed_SoulSeedsTable — sanity на 009: реестр выпущенных
// SoulSeed-сертификатов с partial unique по `status='active'`,
// FK на souls (CASCADE), CHECK на fingerprint-формате.
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

// TestEmbed_ReaperFunctions — sanity на 010-014: Reaper.b SQL-функции для
// 5 правил из docs/keeper/reaper.md. Контракты:
//   - 010 expire_pending_seeds(interval, integer) — DELETE bootstrap_tokens
//     с used_at IS NULL и истёкшим expires_at (PM-переинтерпретация: правило
//     стало DELETE-only, т.к. у bootstrap_tokens нет колонки status).
//   - 011 purge_used_tokens(interval, integer) — DELETE bootstrap_tokens
//     с used_at старше max_age.
//   - 012 purge_souls(text[], interval, integer) — DELETE souls в указанных
//     статусах с COALESCE(last_seen_at, registered_at) старше max_age.
//   - 013 purge_old_seeds(text[], interval, integer) — DELETE soul_seeds
//     в указанных статусах с issued_at старше max_age.
//   - 014 mark_disconnected(interval, integer) — UPDATE souls
//     connected → disconnected при stale last_seen_at.
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

// TestEmbed_SoulsSoulprintColumns — sanity на 015: добавляются три
// колонки `souls.soulprint_*` под ADR-018 (typed-soulprint storage).
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

// TestEmbed_SoulsStatusDestroyed — sanity на 016: enum souls.status
// расширяется значением `destroyed` (ADR-017 cascade, terminal-state).
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

// TestEmbed_SoulSeedsStatusOrphaned — sanity на 017: enum soul_seeds.status
// расширяется значением `orphaned` (ADR-017 cascade).
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

// TestEmbed_IncarnationStatusDestroying — sanity на 031: enum
// incarnation.status расширяется значением `destroying` (S-D1).
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

// TestEmbed_IncarnationStatusDestroyFailed — sanity на 036 (S-D2a): enum
// incarnation.status расширяется терминальным значением `destroy_failed`. up
// добавляет значение в CHECK (drop+recreate, как 031); down сужает CHECK обратно
// к форме 031 и НЕ должен упоминать `destroy_failed`.
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
	// up расширяет, не теряя предыдущие значения (паттерн 031 + destroy_failed).
	if !strings.Contains(body, "'destroying'") {
		t.Errorf("up.sql must preserve 'destroying' in расширенном CHECK; content: %.300s", body)
	}
	d, err := FS.ReadFile("036_incarnation_status_destroy_failed.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	if strings.Contains(dbody, "'destroy_failed'") {
		t.Errorf("down.sql still references 'destroy_failed'; content: %.200s", dbody)
	}
	// down возвращает CHECK к форме 031 (сохраняет 'destroying').
	if !strings.Contains(dbody, "'destroying'") {
		t.Errorf("down.sql must restore CHECK к форме 031 (с 'destroying'); content: %.200s", dbody)
	}
}

// TestEmbed_IncarnationCovens — sanity на 046 (ADR-008 amendment a): добавляет
// колонку incarnation.covens (declared env-теги для RBAC coven-scope). up
// добавляет TEXT[] NOT NULL DEFAULT '{}'; down дропает колонку.
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

// TestEmbed_IncarnationTraits — sanity на 088 (ADR-060 amend, R1): Trait
// релоцирован per-soul → per-incarnation. up добавляет jsonb-колонку
// incarnation.traits (NOT NULL DEFAULT '{}', зеркало souls.traits 087) +
// GIN-индекс; down дропает индекс и колонку. souls.traits в down НЕ упоминается
// (эта миграция его не трогает — projection target остаётся).
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
	// down не ОПЕРИРУЕТ над souls (projection target, миграция 087 его и снимает);
	// упоминание `souls.traits` в комментарии допустимо, но никаких
	// ALTER/DROP по souls / souls_traits_idx.
	for _, bad := range []string{"ALTER TABLE souls", "souls_traits_idx"} {
		if strings.Contains(dbody, bad) {
			t.Errorf("088 down.sql must NOT operate on souls (%q); content: %.200s", bad, dbody)
		}
	}
}

// TestEmbed_IncarnationCreatedScenario — sanity на 089 (механизм нескольких
// create-сценариев, Вариант A): колонка incarnation.created_scenario
// (NOT NULL DEFAULT 'create') хранит имя стартового сценария; rerun-create
// перезапускает именно его. up добавляет колонку с DEFAULT; down дропает.
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

// TestEmbed_ApplyRunsTable — sanity на 018: реестр apply-прогонов
// (M2.x scenario-runner) с composite PK (apply_id, sid), closed-CHECK на
// status, CASCADE-FK на incarnation, SET NULL-FK на operators и тремя
// индексами (incarnation / apply_id / partial running).
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

// TestEmbed_ProvidersTable — sanity на 019: реестр Cloud-Provider-ов
// (ADR-017) с CHECK на name/type-формате (kebab), SET NULL-FK на operators
// и индексом по created_by_aid.
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

// TestEmbed_ProfilesTable — sanity на 020: реестр Cloud-Profile-ей
// (ADR-017) с CHECK на name-формате, RESTRICT-FK на providers (PM-decision —
// защита от потери данных), SET NULL-FK на operators и двумя индексами
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

// TestEmbed_PurgeApplyRunsFunction — sanity на 021: Reaper-правило
// `purge_apply_runs(interval, integer)` — DELETE finished apply_runs
// (success/failed/cancelled) с finished_at старше max_age; running не
// трогает (фиксируем наличие фильтра по статусам и finished_at в up.sql).
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

// TestEmbed_PurgeVoyagesFunction — sanity на 075: Reaper-правило
// `purge_voyages(interval, integer)` — DELETE finished voyages
// (succeeded/failed/partial_failed/cancelled) с finished_at старше max_age;
// scheduled/pending/running не трогает (фиксируем фильтр по статусам и
// finished_at + опору на ON DELETE CASCADE для voyage_targets). ADR-046 §79.
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

// TestEmbed_PurgePushRunsFunction — sanity на 076: Reaper-правило
// `purge_push_runs(interval, integer)` — DELETE finished push_runs
// (success/partial_failed/failed/cancelled) с finished_at старше max_age;
// pending/running не трогает (это правило purge_orphan_push_runs). Дочерних
// FK-таблиц у push_runs нет — per-host результаты inline в summary (051),
// поэтому каскад не фиксируем. Parity purge_apply_runs / purge_voyages.
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

// TestEmbed_PurgeArchivesFunctions — sanity на 077: три Reaper-правила retention
// архивных данных compliance-класса:
//   - purge_incarnation_archive(interval, integer) — DELETE incarnation_archive
//     по archived_at старше max_age (039);
//   - purge_state_history_archive(interval, integer) — DELETE state_history_archive
//     по archived_at старше max_age (039);
//   - purge_archived_state_history(interval, integer) — DELETE soft-deleted-снимков
//     (archived_at IS NOT NULL) из живой state_history старше max_age (048).
//
// Фиксируем наличие фильтра по archived_at и обязательный archived_at IS NOT NULL
// у правила живой state_history (защита от сноса активных снимков). Все три DROP
// в одном down.
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

// TestEmbed_ApplyTaskRegisterTable — sanity на 022: накопитель register-данных
// задач прогона (state_changes слайс 2) с composite PK (apply_id, sid, task_idx)
// и CASCADE-FK на apply_runs(apply_id, sid).
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

// TestEmbed_PurgeApplyTaskRegisterFunction — sanity на 023: Reaper-правило
// `purge_apply_task_register(interval, integer)` — DELETE register-строк
// прогонов в терминальном статусе (success/failed/cancelled) с finished_at
// старше grace; register активных (running) прогонов не трогает (фиксируем
// join к apply_runs + фильтр по status/finished_at в up.sql).
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

// TestEmbed_PurgeApplyTaskRegisterPlanIndex — sanity на 080 (ADR-056 §S1 fix
// Variant B): forward-фикс purge_apply_task_register под staged-render. up
// переключает DELETE-join со неуникального под N>1 task_idx на стабильно-
// уникальный plan_index (CTE-проекция + final DELETE-предикат); down возвращает
// тело функции к форме 023 (task_idx-join), т.к. 079.down снимает plan_index.
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
	// up НЕ должен ключевать удаление по task_idx (это и был баг).
	if strings.Contains(body, "t.task_idx = e.task_idx") {
		t.Errorf("080 up.sql still keys DELETE by task_idx; content: %.400s", body)
	}
	d, err := FS.ReadFile("080_purge_apply_task_register_plan_index.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down восстанавливает форму 023 (task_idx-join), без ссылки на колонку
	// plan_index в SQL (079.down её снимет). Комментарий-пояснение упоминать
	// plan_index может — проверяем именно SQL-предикаты/проекцию.
	if !strings.Contains(dbody, "t.task_idx = e.task_idx") {
		t.Errorf("080 down.sql does not restore task_idx-join (форма 023); content: %.300s", dbody)
	}
	for _, frag := range []string{"atr.plan_index", "t.plan_index", "e.plan_index"} {
		if strings.Contains(dbody, frag) {
			t.Errorf("080 down.sql still references column %q (079.down снимет колонку); content: %.300s", frag, dbody)
		}
	}
}

// TestEmbed_ApplyRunsPassage — sanity на 078 (staged-render Passage, ADR-056
// S1, Variant I): расширение PK apply_runs до (apply_id, sid, passage) +
// переуказание FK apply_task_register на тройку. up добавляет колонку passage
// (NOT NULL DEFAULT 0) обеим таблицам, дропает старые PK/FK и пересоздаёт их
// тройными; down возвращает парные PK/FK и снимает колонки.
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
	// down не должен оставлять тройной PK (форма 018).
	if strings.Contains(dbody, "PRIMARY KEY (apply_id, sid, passage)") {
		t.Errorf("078 down.sql still references тройной PK; content: %.300s", dbody)
	}
}

// TestEmbed_ApplyRunsFailedPlanIndex — sanity на 081 (ADR-056 §S1 fix Variant B,
// failure-канал — последняя инстанция класса global-vs-local-task_idx): apply_runs
// получает nullable-колонку failed_plan_index под ГЛОБАЛЬНЫЙ plan_index упавшей
// задачи (корреляция module/action в drift-report + no_log-подавление в barrier).
// up добавляет колонку и backfill-ит её из task_idx (N=1: локальный==глобальный);
// down дропает колонку.
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
	// Колонка nullable (как task_idx): неизвестна до первой упавшей задачи —
	// NOT NULL DEFAULT здесь был бы неверной семантикой.
	if strings.Contains(body, "failed_plan_index INT NOT NULL") {
		t.Errorf("081 up.sql: failed_plan_index должна быть nullable (как task_idx); content: %.300s", body)
	}
	d, err := FS.ReadFile("081_add_apply_runs_failed_plan_index.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if !strings.Contains(string(d), "DROP COLUMN IF EXISTS failed_plan_index") {
		t.Errorf("081 down.sql does not drop failed_plan_index; content: %.200s", d)
	}
}

// TestEmbed_IncarnationApplyingEpoch — sanity на 082 (ADR-027 amend (m), S0):
// аддитивная подготовка incarnation под standalone-orphan reconcile. up
// добавляет четыре NULLABLE applying-epoch колонки (applying_apply_id /
// applying_attempt / applying_by_kid / applying_since) + partial-индекс под
// Reaper-scan stale-applying. down (колонки nullable, без constraint → обратим)
// снимает индекс и колонки.
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
	// Колонки nullable: NOT NULL / DEFAULT здесь сломал бы fail-safe (existing
	// applying-строки с неизвестным epoch правило НЕ реклеймит по NULL by_kid).
	for _, bad := range []string{
		"applying_apply_id TEXT NOT NULL",
		"applying_by_kid   TEXT NOT NULL",
		"applying_since    TIMESTAMPTZ NOT NULL",
	} {
		if strings.Contains(body, bad) {
			t.Errorf("082 up.sql: applying-epoch колонки должны быть nullable, нашёл %q", bad)
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

// TestEmbed_AuditLogOperatorFK — sanity на 004: добавляется FK
// `audit_log.archon_aid → operators(aid)` с ON DELETE SET NULL.
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

// TestEmbed_ApplyRunsCancelRequested — sanity на 024: cluster-wide Cancel (G1)
// добавляет колонку apply_runs.cancel_requested (BOOLEAN NOT NULL DEFAULT
// false) — флаг отмены, читаемый run-goroutine-ом в barrier-поллинге.
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

// TestEmbed_ApplyRunsWardClaim — sanity на 025 (ADR-027 Phase 0): аддитивная
// подготовка apply_runs под work-queue + claim модель. up добавляет четыре
// Ward-claim колонки (claim_by_kid / claim_at / claim_expires_at / attempt),
// расширяет CHECK status значениями planned/claimed (drop+recreate, как в
// 016/017) с сохранением running/success/failed/cancelled, и закладывает
// partial-индекс под claim-скан. down (status — CHECK, не enum → обратим)
// дропает индекс/колонки и возвращает CHECK к форме 018+024.
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
	// Восстановленный в down CHECK возвращается к форме 018+024
	// (running/success/failed/cancelled) — откат значений возможен
	// (CHECK-constraint, не enum). Точная проверка содержимого CHECK на
	// SQL-side — в migrate-integration (TestIntegration_ApplyRunsWardClaim_Phase0):
	// тут sanity-grep по фрагменту, не парся весь SQL.
	if !strings.Contains(dbody, "CHECK (status IN ('running', 'success', 'failed', 'cancelled'))") {
		t.Errorf("down.sql restored CHECK не в форме 018+024; content: %.300s", dbody)
	}
}

// TestEmbed_ApplyRunsDispatchedStatus — sanity на 040 (ADR-027 amend, S2): enum
// apply_runs.status расширяется фазой `dispatched`. up добавляет значение в CHECK
// (drop+recreate, как 025/036) с сохранением planned/claimed/running/success/
// failed/cancelled; down переводит dispatched-строки в running и сужает CHECK
// обратно к форме 025 (не должен упоминать `dispatched`).
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
	// up расширяет, не теряя предыдущие значения (running остаётся vestigial-valid).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'success'", "'failed'", "'cancelled'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("040 up.sql must preserve %q in расширенном CHECK; content: %.400s", frag, body)
		}
	}
	d, err := FS.ReadFile("040_add_apply_runs_dispatched_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down переводит dispatched-строки в running ПЕРЕД сужением CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'running' WHERE status = 'dispatched'") {
		t.Errorf("040 down.sql must migrate dispatched→running before tightening CHECK; content: %.400s", dbody)
	}
	// Восстановленный CHECK не несёт dispatched (форма 025).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'))") {
		t.Errorf("040 down.sql restored CHECK не в форме 025; content: %.400s", dbody)
	}
}

// TestEmbed_ApplyRunsOrphanedStatus — sanity на 044 (Soul-reconcile, ADR-027(g),
// S6): enum apply_runs.status расширяется терминалом `orphaned`. up аддитивно
// добавляет значение в CHECK (drop+recreate, как 040), сохраняя весь прежний
// набор включая dispatched, и расширяет purge_apply_runs orphaned-ом; down
// переводит orphaned-строки в failed и сужает CHECK обратно к форме 040 (без
// orphaned), восстанавливая purge_apply_runs без orphaned.
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
	// up аддитивен — не теряет прежний набор статусов (включая dispatched).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'dispatched'", "'success'", "'failed'", "'cancelled'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("044 up.sql must preserve %q в расширенном CHECK; content: %.400s", frag, body)
		}
	}
	// purge_apply_runs расширён orphaned-ом (finished-терминал).
	if !strings.Contains(body, "status IN ('success', 'failed', 'cancelled', 'orphaned')") {
		t.Errorf("044 up.sql must extend purge_apply_runs with orphaned; content: %.600s", body)
	}

	d, err := FS.ReadFile("044_add_apply_runs_orphaned_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down переводит orphaned-строки в failed ПЕРЕД сужением CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'failed' WHERE status = 'orphaned'") {
		t.Errorf("044 down.sql must migrate orphaned→failed before tightening CHECK; content: %.400s", dbody)
	}
	// Восстановленный CHECK не несёт orphaned (форма 040, dispatched сохранён).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'))") {
		t.Errorf("044 down.sql restored CHECK не в форме 040; content: %.400s", dbody)
	}
	// purge_apply_runs сужен обратно (без orphaned).
	if !strings.Contains(dbody, "status IN ('success', 'failed', 'cancelled')") {
		t.Errorf("044 down.sql must restore purge_apply_runs without orphaned; content: %.600s", dbody)
	}
}

// TestEmbed_ApplyRunsNoMatchStatus — sanity на 045 (FINDING-01 вариант (б)):
// enum apply_runs.status расширяется терминалом `no_match` ПОВЕРХ 044 (orphaned).
// up аддитивно добавляет значение в CHECK (drop+recreate, как 040/044), сохраняя
// весь прежний набор включая orphaned, и расширяет purge_apply_runs no_match-ем;
// down переводит no_match-строки в success (дореформенный терминал нецелевого
// хоста) и сужает CHECK обратно к форме 044 (без no_match), восстанавливая
// purge_apply_runs без no_match. ВАЖНО: orphaned (044) не сломан — down НЕ трогает
// orphaned-строки и сохраняет orphaned в восстановленном CHECK.
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
	// up аддитивен — не теряет прежний набор статусов (включая orphaned из 044).
	for _, frag := range []string{"'planned'", "'claimed'", "'running'", "'dispatched'", "'success'", "'failed'", "'cancelled'", "'orphaned'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("045 up.sql must preserve %q в расширенном CHECK; content: %.400s", frag, body)
		}
	}
	// purge_apply_runs расширён no_match-ем (finished-терминал), orphaned сохранён.
	if !strings.Contains(body, "status IN ('success', 'failed', 'cancelled', 'orphaned', 'no_match')") {
		t.Errorf("045 up.sql must extend purge_apply_runs with no_match (preserving orphaned); content: %.600s", body)
	}

	d, err := FS.ReadFile("045_add_apply_runs_no_match_status.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	dbody := string(d)
	// down переводит no_match-строки в success ПЕРЕД сужением CHECK.
	if !strings.Contains(dbody, "UPDATE apply_runs SET status = 'success' WHERE status = 'no_match'") {
		t.Errorf("045 down.sql must migrate no_match→success before tightening CHECK; content: %.400s", dbody)
	}
	// down НЕ трогает orphaned-строки (не сломать 044).
	if strings.Contains(dbody, "WHERE status = 'orphaned'") {
		t.Errorf("045 down.sql must NOT touch orphaned rows; content: %.400s", dbody)
	}
	// Восстановленный CHECK не несёт no_match, но СОХРАНЯЕТ orphaned (форма 044).
	if !strings.Contains(dbody, "CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'))") {
		t.Errorf("045 down.sql restored CHECK не в форме 044 (с orphaned, без no_match); content: %.400s", dbody)
	}
	// purge_apply_runs сужен обратно (без no_match, orphaned сохранён).
	if !strings.Contains(dbody, "status IN ('success', 'failed', 'cancelled', 'orphaned')") {
		t.Errorf("045 down.sql must restore purge_apply_runs without no_match (keeping orphaned); content: %.600s", dbody)
	}
}

// TestEmbed_RBACTables — sanity на 026 (ADR-028): три таблицы rbac_* с
// CHECK на формат имени роли, FK на operators(aid) и ON DELETE CASCADE.
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

// TestEmbed_SynodTables — sanity на 069 (ADR-049): три таблицы synod* (Архон →
// Synod → Роли) тем же паттерном rbac_*: CHECK на формат имени (как rbac_roles),
// FK на operators(aid)/rbac_roles(name), ON DELETE CASCADE c обеих сторон bundle,
// индекс synod_operators(aid) под snapshot-разворот.
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

// TestEmbed_SeedClusterAdmin — sanity на 027 (ADR-028(b), E1): идемпотентный
// INSERT роли cluster-admin (builtin=true) + permission `*` через
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

// TestEmbed_PluginSigilsTable — sanity на 028 (ADR-026): реестр plugin_sigils
// (Keeper-signed allow-list плагинов) с partial UNIQUE-индексом по активным
// записям (namespace, name, ref WHERE revoked_at IS NULL), CHECK на
// sha256-формате (hex), BYTEA signature + JSONB manifest, RESTRICT-FK
// allowed_by_aid (NOT NULL) и SET NULL-FK revoked_by_aid (NULL) на operators.
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

// TestEmbed_ApplyRunsRecipe — sanity на 029 (ADR-027(c)(f)): аддитивная
// nullable-колонка apply_runs.recipe (JSONB) под just-in-time-рендер задания
// Acolyte-ом при claim. up добавляет колонку + COMMENT, down дропает её.
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

// TestEmbed_PluginSigilsManifestRaw — sanity на 030 (M1-storage): аддитивная
// nullable-колонка plugin_sigils.manifest_raw (BYTEA) под byte-exact канон
// подписанного manifest.yaml (verify/broadcast, ADR-026). up добавляет колонку
// + COMMENT, down дропает её.
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

// TestEmbed_PluginSigilsCommitSha — sanity на 038 (A1-S3): аддитивная
// nullable-колонка plugin_sigils.commit_sha (TEXT) — audit-метка происхождения
// бинаря (git-commit, ADR-026(g)), ВНЕ подписываемого блока. up добавляет
// колонку + COMMENT, down дропает её.
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

// TestEmbed_IncarnationArchiveTables — sanity на 039 (S-D3, каскад V3): две
// archive-таблицы (incarnation_archive / state_history_archive) с колонкой
// archived_at и БЕЗ FK на live incarnation (чтобы переживать DELETE+CASCADE).
// up создаёт обе таблицы + индексы; в up НЕ должно быть FK на incarnation
// (REFERENCES incarnation) — иначе архив не пережил бы каскадный снос. down
// дропает обе.
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
	// Инвариант каскада V3: НЕТ FK на live incarnation — архив переживает DELETE.
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

// TestEmbed_OracleCircuitTable — sanity на 042 (ADR-030(a), circuit-breaker S4):
// per-decree fixed-window счётчик `oracle_circuit` с PK decree, CASCADE-FK на
// decrees (re-enable = delete+recreate чистит окно) и колонками window_start /
// fire_count под атомарный UPSERT-инкремент.
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

// TestEmbed_OmensTable — sanity на 032 (ADR-025, Augur §4.1): реестр omens
// (внешние системы) с CHECK на name-формате (kebab), closed-CHECK на
// source_type enum (vault/prometheus/elk), SET NULL-FK на operators и индексом
// по created_by_aid.
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

// TestEmbed_RitesTable — sanity на 033 (ADR-025, Augur §4.2): реестр rites
// (grant-ы) с IDENTITY-PK, CASCADE-FK на omens, XOR-CHECK на субъекте
// (coven/sid), CHECK token-полей ⇒delegate, JSONB allow и тремя индексами
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

// TestEmbed_ServiceRegistryTable — sanity на 034 (managed service-registry):
// таблица service_registry с PK name (kebab-CHECK), nonempty-CHECK на git/ref,
// nullable refresh и двумя FK на operators (created_by_aid / updated_by_aid).
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

// TestEmbed_KeeperSettingsTable — sanity на 035 (cluster-wide key-value):
// таблица keeper_settings с PK key (snake-CHECK), NOT NULL value и SET NULL-FK
// updated_by_aid на operators. Well-known строки миграция НЕ вставляет.
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
	// Миграция не должна сеять well-known ключи (runtime-данные).
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

// TestEmbed_SigilSigningKeysTable — sanity на 037 (ADR-026(h), R3 multi-anchor):
// реестр trust-anchor-ключей подписи Sigil. Инвариант "ровно один primary среди
// active" материализован partial UNIQUE-индексом по (is_primary) WHERE
// status='active' AND is_primary; CHECK на status-enum (active/retired); оба FK
// (introduced_by_aid / retired_by_aid) на operators с ON DELETE SET NULL.
// Колонки приватника НЕТ — только pubkey_pem + vault_ref (security-инвариант).
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
	// Security-инвариант: приватник НИКОГДА не в PG. Никаких приватных-ключевых
	// колонок (private/secret_key/privkey) — только публичная часть + Vault-ссылка.
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

// TestEmbed_StateHistoryArchivedAt — sanity на 048 (ADR-Q19 retention):
// аддитивная nullable-колонка `state_history.archived_at` (soft-delete-флаг)
// + partial-индекс по WHERE archived_at IS NULL под фильтр активных снимков.
// up добавляет колонку TIMESTAMPTZ и CREATE INDEX state_history_active_idx;
// down дропает индекс и колонку (обратимо, soft-deleted-снимки физически
// остаются — становятся неотличимы от активных).
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

// TestEmbed_ArchiveStateHistoryFunction — sanity на 049 (ADR-Q19 retention):
// SQL-функция `archive_state_history(integer, boolean, integer)` помечает
// `archived_at = NOW()` для активных снимков `state_history` сверх N
// последних на incarnation; при keep_version_bump=true исключает snapshots
// шагов state_schema-миграции (scenario='migration'). up создаёт функцию;
// down её дропает.
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

// TestEmbed_IncarnationDriftScanColumns — sanity на 050 (ADR-031 Slice C):
// миграция добавляет колонки `last_drift_check_at` / `last_drift_summary` в
// `incarnation` и partial-индекс. Down дропает колонки (индекс падает с ними).
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

// TestEmbed_SoulsSshTarget — sanity на 053 (ADR-032 amendment 2026-05-26, S7-1):
// добавляется колонка souls.ssh_target (jsonb) + CHECK shape-guard
// souls_ssh_target_shape (типы ssh_port/ssh_user/soul_path при non-NULL).
// up создаёт колонку и constraint; down дропает constraint и колонку.
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

// TestEmbed_SoulsSshTarget_SSHProvider — sanity на 056 (ADR-032 amendment
// 2026-05-27, P2 W-1): расширенный CHECK `souls_ssh_target_shape` с optional
// `ssh_provider` (kebab-case regex). up пересоздаёт constraint с regex'ом
// `^[a-z][a-z0-9-]{0,62}$`, down возвращает прежний (без ssh_provider).
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

// TestEmbed_TidesTable — sanity на 055 (ADR-040 amendment 2026-05-27, W-1):
// реестр `tides` (top-level invocation-time chunking) с CHECK-инвариантами
// (status / on_surge_failure / running⇒claim-NOT-NULL / surge_index ≤ total),
// FK на operators(aid), двумя partial-индексами (claim_scan / pending_pickup) +
// back-link колонки apply_runs.tide_id / surge_index с partial-индексом.
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

// TestEmbed_DropTides — sanity на 061 (Wave 5 Pass 1): полное удаление Tide.
// up дропает реестр `tides` + back-link-колонки apply_runs (tide_id/surge_index)
// + partial-индекс; down пересоздаёт схему по образцу 055.
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

// TestEmbed_ErrandRuns_TableShape — sanity на 057 (ADR-041, E6-1): реестр
// `errand_runs` (top-level multi-target pull-ad-hoc invocation) с
// CHECK-инвариантами (status / on_failure / concurrency / total / done-bounds /
// attempt / running⇒claim-NOT-NULL / terminal⇒finished_at-NOT-NULL), FK на
// operators(aid) и двумя partial-индексами (pending_pickup / claim_scan).
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

// TestEmbed_Errands_HasErrandRunIdColumn — sanity на 057 (ADR-041, E6-1):
// back-link errands.errand_run_id (NULLABLE) + FK CASCADE на
// errand_runs(errand_run_id) + partial-индекс errands_errand_run_id_idx;
// down аккуратно дропает FK/индекс/колонку.
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

// TestEmbed_VoyagesTable — sanity на 059 (ADR-043, S1): реестр `voyages`
// (унифицированный батчевый прогон) с дискриминатором kind=scenario|command,
// CHECK-инвариантами (kind / status включая scheduled / on_failure /
// kind↔payload-консистентность / running⇒claim-NOT-NULL / terminal⇒finished_at /
// batch_index ≤ total), FK на operators(aid) и двумя partial-индексами
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

// TestEmbed_VoyageTargetsTable — sanity на 059 (ADR-043, S1): таблица
// `voyage_targets` (единицы прогона / Leg-разбиение) с composite PK
// (voyage_id, target_kind, target_id), CHECK на target_kind/status,
// CASCADE-FK на voyages(voyage_id) и индексом (voyage_id, batch_index).
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

// TestEmbed_CadencesTable — sanity на 066 (ADR-046, S1): реестр `cadences`
// (расписание, спавнящее Voyage) с CHECK-инвариантами (schedule_kind / overlap_policy
// / kind / schedule_consistency interval↔cron XOR / kind↔payload-консистентность /
// sane-bounds), FK на operators(aid) и partial-индексом due-скана.
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

// TestEmbed_VoyagesCadenceBackLink — sanity на 066 (ADR-046 §2): back-link
// voyages.cadence_id (NULLABLE) + FK ON DELETE SET NULL на cadences(id) +
// partial-индекс voyages_cadence_id_idx; down дропает индекс/FK/колонку.
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

// TestEmbed_CadencesIntervalFloor — sanity на 068 (ADR-046 Pass B, floor-лимит):
// up несёт pre-flight data-guard (RAISE при interval_seconds < 30 ПЕРЕД ADD
// CONSTRAINT), CHECK cadences_interval_seconds_floor (>= 30, отдельным именем — НЕ
// переопределяет _positive из 066) и partial-индекс cadences_enabled_interval_idx
// под MIN-запрос; down дропает индекс + floor-CHECK (positive/due-scan из 066 не
// трогает).
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
	// pre-flight data-guard должен стоять ПЕРЕД ADD CONSTRAINT (fail-fast до
	// проверки констрейнтом — понятный RAISE вместо сырого CHECK violation).
	if guardIdx, addIdx := strings.Index(body, "RAISE EXCEPTION"), strings.Index(body, "ADD CONSTRAINT cadences_interval_seconds_floor"); guardIdx < 0 || addIdx < 0 || guardIdx > addIdx {
		t.Errorf("068 up.sql: data-guard (RAISE) должен предшествовать ADD CONSTRAINT; guardIdx=%d addIdx=%d", guardIdx, addIdx)
	}
	// floor-CHECK — отдельное имя, НЕ переопределяет positive из 066 (тот не дропается).
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
	// down НЕ трогает 066-объекты (positive-CHECK / due-scan-индекс / таблицу).
	for _, forbidden := range []string{"cadences_interval_seconds_positive", "cadences_due_scan_idx", "DROP TABLE"} {
		if strings.Contains(dstr, forbidden) {
			t.Errorf("068 down.sql must NOT touch 066-object %q; content: %.300s", forbidden, dstr)
		}
	}
}

// TestEmbed_CadencesFailThresholdPercent — sanity на 070 (ADR-043 amendment
// 2026-06-09, Cadence-recipe S3): аддитивная колонка cadences.fail_threshold_percent
// (порог провалов процентом от spawn-scope, симметрия batch_percent из 066) +
// CHECK на диапазон [1, 100]. up — ADD COLUMN + ADD CONSTRAINT range; down —
// DROP CONSTRAINT + DROP COLUMN, не трогая 066-объекты.
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
	// Аддитивно (forward-compat): up НЕ дропает прежние объекты. (Имена 066-колонок
	// допустимы в пояснительном комментарии — проверяем именно отсутствие DROP.)
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
	// down НЕ трогает 066/068-объекты.
	for _, forbidden := range []string{"batch_percent", "cadences_interval_seconds_floor", "DROP TABLE"} {
		if strings.Contains(dstr, forbidden) {
			t.Errorf("070 down.sql must NOT touch prior object %q; content: %.300s", forbidden, dstr)
		}
	}
}

// TestEmbed_IncarnationChoirsTable — sanity на 060 (ADR-044, S-T2): таблица
// `incarnation_choirs` (Choir — declared-топология хостов внутри инкарнации) с
// composite PK (incarnation_name, choir_name), CHECK на choir_name-формате и
// min/max-size инвариантах, CASCADE-FK на incarnation(name) и SET NULL-FK на
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

// TestEmbed_IncarnationChoirVoicesTable — sanity на 060 (ADR-044, S-T2): таблица
// `incarnation_choir_voices` (Voice — членство SID в Choir-е) с composite PK
// (incarnation_name, choir_name, sid), CASCADE-FK на пару incarnation_choirs и
// на souls(sid), SET NULL-FK на operators(aid), индексом по sid. Глобального
// UNIQUE(sid) НЕТ намеренно (мультиинкарнационность, ADR-044 пункт 3).
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
	// Глобальный UNIQUE(sid) запрещён моделью (один SID — Voice в разных
	// инкарнациях): фиксируем его отсутствие.
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

// TestEmbed_TidingEphemeralPayload — sanity на 072 (ADR-052 Amendment N1):
// `tidings` расширяется четырьмя additive-колонками (ephemeral/voyage_id/
// annotations/projection), CHECK-инвариантом ephemeral⟺voyage_id и partial-
// индексом по voyage_id WHERE ephemeral. up аддитивен (не дропает прежнее);
// down снимает индекс, CHECK и все четыре колонки.
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
	// Аддитивно (forward-compat): up НЕ дропает прежние объекты `tidings`.
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
