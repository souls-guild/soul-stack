package vault_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// presentVault — fake Vault для kv-present: хранит per-path payload в памяти,
// ReadKV отдаёт сохранённое (или ErrVaultKVNotFound для несуществующего пути),
// WriteKV мерджит. Так тест видит реально записанные значения и проверяет их
// длину/алфавит. readErr/writeErr инъецируют транспортную ошибку (Vault
// недоступен/нет прав) для негативных кейсов; writes считает фактические WriteKV.
type presentVault struct {
	store    map[string]map[string]any
	readErr  error
	writeErr error
	writes   int
}

func newPresentVault(seed map[string]map[string]any) *presentVault {
	store := make(map[string]map[string]any)
	for p, m := range seed {
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		store[p] = cp
	}
	return &presentVault{store: store}
}

func (v *presentVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if v.readErr != nil {
		return nil, v.readErr
	}
	m, ok := v.store[path]
	if !ok {
		return nil, keepervault.ErrVaultKVNotFound
	}
	cp := make(map[string]any, len(m))
	for k, val := range m {
		cp[k] = val
	}
	return cp, nil
}

func (v *presentVault) WriteKV(_ context.Context, path string, data map[string]any) error {
	v.writes++
	if v.writeErr != nil {
		return v.writeErr
	}
	cp := make(map[string]any, len(data))
	for k, val := range data {
		cp[k] = val
	}
	v.store[path] = cp
	return nil
}

func presentParams(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	return mustStruct(t, m)
}

// TestPresent_AbsentPath_Generates — путь отсутствует → генерится, changed=true,
// в Vault лежит значение дефолтной длины (32) и ТОЛЬКО из ascii-printable-safe
// алфавита (дефолт). Значение в output/audit не светится (см. отдельный guard).
func TestPresent_AbsentPath_Generates(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/redis/users/admin"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("want changed=true failed=false, got %+v", ev)
	}
	got, ok := pv.store["secret/redis/users/admin"]["password"]
	if !ok {
		t.Fatal("password not written to vault")
	}
	pw := got.(string)
	if len([]rune(pw)) != 32 {
		t.Errorf("default length = %d chars, want 32", len([]rune(pw)))
	}
	assertAlphabet(t, pw, safeAlphabetRunes())
	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventVaultKVPresent {
		t.Errorf("audit events = %+v", fa.events)
	}
}

// TestPresent_PathPresent_NoOp — поле присутствует → no-op, changed=false,
// значение НЕ перезаписано, audit-event не пишется.
func TestPresent_PathPresent_NoOp(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"password": "EXISTING-do-not-touch"},
	})
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/redis/users/admin"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("want changed=false failed=false (idempotent), got %+v", ev)
	}
	if pv.store["secret/redis/users/admin"]["password"] != "EXISTING-do-not-touch" {
		t.Errorf("existing secret overwritten: %v", pv.store["secret/redis/users/admin"]["password"])
	}
	if len(fa.events) != 0 {
		t.Errorf("no-op must not write audit, got %+v", fa.events)
	}
}

// TestPresent_PartPresent_GeneratesOnlyMissing — три target-а, один уже есть →
// генерятся только два отсутствующих; присутствующий не тронут.
func TestPresent_PartPresent_GeneratesOnlyMissing(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/app/a": {"password": "A-existing"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/app/a"},
				map[string]any{"path": "secret/app/b"},
				map[string]any{"path": "secret/app/c"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("want changed=true")
	}
	if pv.store["secret/app/a"]["password"] != "A-existing" {
		t.Errorf("present secret a overwritten")
	}
	for _, p := range []string{"secret/app/b", "secret/app/c"} {
		v, ok := pv.store[p]["password"].(string)
		if !ok || v == "" {
			t.Errorf("missing secret %s not generated", p)
		}
	}
	out := stream.Last().Output.AsMap()
	generated := out["generated"].(map[string]any)
	if _, leaked := generated["secret/app/a"]; leaked {
		t.Error("output.generated lists already-present path a")
	}
	if len(generated) != 2 {
		t.Errorf("output.generated paths = %d, want 2 (b, c)", len(generated))
	}
}

// TestPresent_ExplicitPolicy_Respected — явные charset+length (step-level)
// реально отражены в выходе: hex-алфавит, длина 16.
func TestPresent_ExplicitPolicy_Respected(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"policy": map[string]any{
				"charset": "hex",
				"length":  float64(16),
			},
			"targets": []any{
				map[string]any{"path": "secret/x"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pw := pv.store["secret/x"]["password"].(string)
	if len([]rune(pw)) != 16 {
		t.Errorf("length = %d, want 16", len([]rune(pw)))
	}
	assertAlphabet(t, pw, []rune("0123456789abcdef"))
}

// TestPresent_AllowedChars_Respected — явный allowed_chars алфавит соблюдён,
// per-target override перекрывает step-level policy.
func TestPresent_AllowedChars_Respected(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"policy": map[string]any{"charset": "hex", "length": float64(20)},
			"targets": []any{
				map[string]any{
					"path":  "secret/override",
					"field": "token",
					"policy": map[string]any{
						"allowed_chars": "ABCDEF",
						"length":        float64(12),
					},
				},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pw := pv.store["secret/override"]["token"].(string)
	if len([]rune(pw)) != 12 {
		t.Errorf("override length = %d, want 12", len([]rune(pw)))
	}
	assertAlphabet(t, pw, []rune("ABCDEF"))
}

// TestPresent_MergeKeepsSiblingFields — генерация в путь с существующими
// СОСЕДНИМИ полями не теряет их (read-merge-write).
func TestPresent_MergeKeepsSiblingFields(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/u": {"username": "admin"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/u", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pv.store["secret/u"]["username"] != "admin" {
		t.Error("sibling field username lost on merge")
	}
	if _, ok := pv.store["secret/u"]["password"].(string); !ok {
		t.Error("password not generated")
	}
}

// TestPresent_SecurityNoLeak — ★GUARD: сгенерированное ЗНАЧЕНИЕ не попадает ни
// в register-output, ни в audit-payload (ADR-010, эталон sigil.KeyService.Introduce).
// Проверяем рекурсивно весь output и payload: нигде не должно быть точного
// значения секрета (ни как ключ, ни как значение, ни в подстроке).
func TestPresent_SecurityNoLeak(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/leak/check", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	secret := pv.store["secret/leak/check"]["password"].(string)
	if secret == "" {
		t.Fatal("precondition: secret not generated")
	}

	// 1. register-output не содержит значения нигде в дереве.
	out := stream.Last().Output.AsMap()
	if containsSecret(out, secret) {
		t.Errorf("SECURITY: generated secret leaked into register-output: %v", out)
	}
	// Sanity: путь/имя поля в output есть (output не пустой и осмысленный).
	gen := out["generated"].(map[string]any)
	if _, ok := gen["secret/leak/check"]; !ok {
		t.Error("output.generated must list the path (field names ok, value not)")
	}

	// 2. audit-payload не содержит значения.
	if len(fa.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(fa.events))
	}
	if containsSecret(fa.events[0].Payload, secret) {
		t.Errorf("SECURITY: generated secret leaked into audit-payload: %v", fa.events[0].Payload)
	}
}

// TestPresent_Validate — kv-present валидация: targets обязателен и непуст;
// взаимоисключимость charset/allowed_chars; границы length.
func TestPresent_Validate(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	cases := []struct {
		name   string
		params map[string]any
		wantOK bool
	}{
		{"missing targets", map[string]any{}, false},
		{"empty targets", map[string]any{"targets": []any{}}, false},
		{"ok minimal", map[string]any{"targets": []any{map[string]any{"path": "secret/x"}}}, true},
		{
			"charset and allowed_chars together",
			map[string]any{
				"policy":  map[string]any{"charset": "hex", "allowed_chars": "abc"},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
		{
			"unknown charset",
			map[string]any{
				"policy":  map[string]any{"charset": "klingon"},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
		{
			"length below min",
			map[string]any{
				"policy":  map[string]any{"length": float64(4)},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  "kv-present",
				Params: mustStruct(t, tc.params),
			})
			if rep.Ok != tc.wantOK {
				t.Errorf("Ok = %v, want %v (errors=%v)", rep.Ok, tc.wantOK, rep.Errors)
			}
		})
	}
}

// TestPresent_UnknownState — неизвестный state на том же модуле → failed.
func TestPresent_UnknownState(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-generate",
		Params: mustStruct(t, map[string]any{"targets": []any{map[string]any{"path": "x"}}}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("unknown state must fail")
	}
}

// TestPresent_WriteKVError_FailsTask — WriteKV-failure (Vault недоступен/нет прав
// на запись) → задача честно фейлит, ошибка не глотается. Аналог
// kvread_test.go::TestApply_VaultError для write-ветки. read проходит (путь
// отсутствует → генерация), падение ровно на WriteKV.
func TestPresent_WriteKVError_FailsTask(t *testing.T) {
	pv := newPresentVault(nil)
	pv.writeErr = errors.New("permission denied")
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("WriteKV error must fail task (write-failure не должна молча проглатываться)")
	}
}

// TestPresent_AuditWriteError_FailsTask — audit-write-fail → задача фейлит
// (симметрия с kvread_test.go::TestApply_AuditWriteError_FailsTask): compliance-
// запись нельзя молча пропустить. Секрет при этом В VAULT УЖЕ записан (WriteKV
// прошёл до audit) — здесь проверяем только терминальный fail задачи.
func TestPresent_AuditWriteError_FailsTask(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{err: errors.New("pg down")}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("audit write error must fail task (compliance must not be silently skipped)")
	}
}

// TestPresent_EmptyStringField_Regenerates — поле присутствует, но его значение —
// ПУСТАЯ строка → трактуется как absent (fieldPresent), генерится непустой пароль,
// changed=true. Пустой пароль бесполезен, поэтому no-op на "" был бы дырой.
func TestPresent_EmptyStringField_Regenerates(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"password": ""},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("пустая строка должна трактоваться как absent → regenerate, changed=true")
	}
	pw, _ := pv.store["secret/redis/users/admin"]["password"].(string)
	if pw == "" {
		t.Error("пустой пароль не перегенерён")
	}
}

// TestPresent_DifferentFieldPresent_GeneratesTargetKeepsSibling — путь существует с
// ДРУГИМ полем (username), целевое (password) отсутствует → генерится только
// password, существующий username сохраняется. Отличается от MergeKeepsSiblingFields
// фокусом: явно проверяем, что наличие НЕ-целевого поля не считается «секрет уже
// есть» (fieldPresent смотрит конкретное поле, не сам путь).
func TestPresent_DifferentFieldPresent_GeneratesTargetKeepsSibling(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"username": "admin"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin", "field": "password"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("целевое поле password отсутствует → должно генериться (changed=true)")
	}
	if pv.store["secret/redis/users/admin"]["username"] != "admin" {
		t.Error("sibling username утерян при генерации password")
	}
	if pw, _ := pv.store["secret/redis/users/admin"]["password"].(string); pw == "" {
		t.Error("password не сгенерирован")
	}
}

// TestPresent_DuplicateTargetGeneratesOnce — два target-а с одинаковым {path,field}
// → генерация ОДИН раз (pendingWrites-guard): один WriteKV, поле в output.generated
// не дублируется. Без guard-а второй target перегенерил бы значение и/или дал
// лишний WriteKV (новая KV-версия).
func TestPresent_DuplicateTargetGeneratesOnce(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/dup", "field": "password"},
				map[string]any{"path": "secret/dup", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pv.writes != 1 {
		t.Errorf("WriteKV вызван %d раз, ждём 1 (дубль {path,field} не должен плодить запись)", pv.writes)
	}
	gen := stream.Last().Output.AsMap()["generated"].(map[string]any)
	fields, ok := gen["secret/dup"].([]any)
	if !ok || len(fields) != 1 {
		t.Errorf("output.generated[secret/dup] = %v, ждём ровно одно поле password (без дубля)", gen["secret/dup"])
	}
}

// TestPresent_PolicyLengthBoundaries — границы length: 8 и 1024 валидны, 7 и 1025
// отвергаются. Проверка через Validate (parsePolicy) на step-level policy.
func TestPresent_PolicyLengthBoundaries(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	cases := []struct {
		length int
		wantOK bool
	}{
		{7, false}, {8, true}, {1024, true}, {1025, false},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State: "kv-present",
				Params: mustStruct(t, map[string]any{
					"policy":  map[string]any{"length": float64(tc.length)},
					"targets": []any{map[string]any{"path": "secret/x"}},
				}),
			})
			if rep.Ok != tc.wantOK {
				t.Errorf("length=%d: Ok=%v, want %v (errors=%v)", tc.length, rep.Ok, tc.wantOK, rep.Errors)
			}
		})
	}
}

// TestPresent_EmptyPathRejected — target с пустым path отвергается на Validate
// (parseTargets/parseTarget): пустой путь — некорректная цель.
func TestPresent_EmptyPathRejected(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "kv-present",
		Params: mustStruct(t, map[string]any{
			"targets": []any{map[string]any{"path": ""}},
		}),
	})
	if rep.Ok {
		t.Fatal("пустой path в target должен отвергаться")
	}
}

// TestPresent_SingleCharAlphabetRejected — allowed_chars из одного символа (или
// схлопывающийся в один после дедупликации повторов) отвергается: алфавит <2
// distinct символов вырождает генерацию в константу (0 энтропии).
func TestPresent_SingleCharAlphabetRejected(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	for _, allowed := range []string{"a", "aaaa"} {
		rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "kv-present",
			Params: mustStruct(t, map[string]any{
				"policy":  map[string]any{"allowed_chars": allowed},
				"targets": []any{map[string]any{"path": "secret/x"}},
			}),
		})
		if rep.Ok {
			t.Errorf("allowed_chars=%q (collapse-to-one) должен отвергаться (нужно >= 2 distinct)", allowed)
		}
	}
}

// --- helpers ---

// assertAlphabet проверяет, что каждый символ s входит в allowed.
func assertAlphabet(t *testing.T, s string, allowed []rune) {
	t.Helper()
	set := make(map[rune]bool, len(allowed))
	for _, r := range allowed {
		set[r] = true
	}
	for _, r := range s {
		if !set[r] {
			t.Fatalf("char %q outside allowed alphabet (value=%q)", r, s)
		}
	}
}

// safeAlphabetRunes воспроизводит ascii-printable-safe алфавит (0x21..0x7E минус
// исключённые) для assertAlphabet дефолтных тестов. Держим список исключений в
// синхроне с policy.go::excludedFromSafe.
func safeAlphabetRunes() []rune {
	const excluded = " \"'#\\`$"
	ex := make(map[byte]bool)
	for i := 0; i < len(excluded); i++ {
		ex[excluded[i]] = true
	}
	var out []rune
	for c := byte(0x21); c <= 0x7E; c++ {
		if !ex[c] {
			out = append(out, rune(c))
		}
	}
	return out
}

// containsSecret рекурсивно ищет точное значение secret в произвольном
// map/slice/string-дереве (ключи и значения). Substring-совпадение тоже считается
// утечкой (частичное раскрытие недопустимо).
func containsSecret(v any, secret string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, secret)
	case map[string]any:
		for k, val := range x {
			if strings.Contains(k, secret) || containsSecret(val, secret) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if containsSecret(val, secret) {
				return true
			}
		}
	}
	return false
}
