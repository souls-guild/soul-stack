//go:build e2e_live

package harness

// Bare git-repo per-example в $TMP — service-loader Keeper-а (file://-URL)
// читает его как обычный remote.
//
// Контракт (L3b-3 slice — `smoke-nginx-live`):
//  1. NewStack создаёт $TMP/<exampleSlug>.git через `git init --bare`.
//  2. Снапшот директории cfg.ExamplePath коммитится в этот bare-repo.
//  3. file://-URL подставляется в keeper-config service_registry перед стартом
//     Keeper-процесса.
//
// На L3b-1-фазе функций здесь нет — каркас (signature-only сейчас, тело — в L3b-3).
