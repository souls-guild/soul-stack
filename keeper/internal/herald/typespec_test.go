package herald

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- generic-валидатор из дескриптора + telegram ----------------------

func TestValidateConfig_Telegram(t *testing.T) {
	const tokenRef = "vault:secret/keeper/telegram#bot_token"
	cases := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{"valid minimal", map[string]any{"bot_token_ref": tokenRef, "chat_id": "@ops"}, false},
		{"valid numeric chat_id", map[string]any{"bot_token_ref": tokenRef, "chat_id": "-1001234567890"}, false},
		{"valid parse_mode MarkdownV2", map[string]any{"bot_token_ref": tokenRef, "chat_id": "@ops", "parse_mode": "MarkdownV2"}, false},
		{"valid parse_mode HTML", map[string]any{"bot_token_ref": tokenRef, "chat_id": "@ops", "parse_mode": "HTML"}, false},
		{"valid parse_mode empty", map[string]any{"bot_token_ref": tokenRef, "chat_id": "@ops", "parse_mode": ""}, false},
		{"missing bot_token_ref", map[string]any{"chat_id": "@ops"}, true},
		{"missing chat_id", map[string]any{"bot_token_ref": tokenRef}, true},
		{"empty chat_id", map[string]any{"bot_token_ref": tokenRef, "chat_id": ""}, true},
		{"whitespace chat_id", map[string]any{"bot_token_ref": tokenRef, "chat_id": "   "}, true},
		{"bot_token_ref not vault-ref", map[string]any{"bot_token_ref": "123456:ABCDEF", "chat_id": "@ops"}, true},
		{"bot_token_ref empty", map[string]any{"bot_token_ref": "", "chat_id": "@ops"}, true},
		{"bot_token_ref not string", map[string]any{"bot_token_ref": 42, "chat_id": "@ops"}, true},
		{"unknown parse_mode", map[string]any{"bot_token_ref": tokenRef, "chat_id": "@ops", "parse_mode": "Markdown"}, true},
		{"chat_id not string", map[string]any{"bot_token_ref": tokenRef, "chat_id": 42}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(HeraldTelegram, tc.config)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateConfig(telegram, %v) err = %v, wantErr = %v", tc.config, err, tc.wantErr)
			}
		})
	}
}

// TestValidateConfig_TelegramValidatorNeverReadsSecret — доменный валидатор
// config НЕ читает Vault: bot_token_ref держит vault-REF (указатель на секрет),
// сам токен резолвится только на доставке (telegramTransport). Значит какой бы
// секрет ни лежал в Vault, он в валидаторе не участвует и в его ошибках
// появиться не может. Тест: валидный ref + непустой chat_id проходит валидацию
// БЕЗ похода в Vault (kv не передаётся в ValidateConfig вовсе).
func TestValidateConfig_TelegramValidatorNeverReadsSecret(t *testing.T) {
	// Валидный vault-ref + chat_id — валидация зелёная, Vault не требуется.
	if err := ValidateConfig(HeraldTelegram, map[string]any{
		"bot_token_ref": "vault:secret/keeper/telegram#bot_token",
		"chat_id":       "@ops",
	}); err != nil {
		t.Fatalf("valid telegram config rejected: %v", err)
	}
	// Битый chat_id при валидном ref: ошибка про chat_id, значение ref (указатель
	// на секрет) в тексте про chat_id не всплывает.
	err := ValidateConfig(HeraldTelegram, map[string]any{
		"bot_token_ref": "vault:secret/keeper/telegram#bot_token",
		"chat_id":       "   ",
	})
	if err == nil {
		t.Fatal("expected chat_id error")
	}
	if strings.Contains(err.Error(), "secret/keeper/telegram") {
		t.Errorf("chat_id error leaked bot_token_ref value: %v", err)
	}
}

// --- AllHeraldTypes: единый источник, отсортирован -------------------

func TestAllHeraldTypes_SortedAndComplete(t *testing.T) {
	got := AllHeraldTypes()
	if len(got) != len(heraldTypeSpecs) {
		t.Fatalf("AllHeraldTypes len=%d, registry len=%d", len(got), len(heraldTypeSpecs))
	}
	for _, ty := range got {
		if _, ok := heraldTypeSpecs[ty]; !ok {
			t.Errorf("AllHeraldTypes returned %q not in registry", ty)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("AllHeraldTypes not sorted or dup: %q >= %q", got[i-1], got[i])
		}
	}
	// Пилот слайса 1 фиксируем адресно: ровно webhook+telegram.
	want := []HeraldType{HeraldTelegram, HeraldWebhook}
	if len(got) != len(want) {
		t.Fatalf("AllHeraldTypes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllHeraldTypes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAllHeraldTypes_MatchesPGCheck — СВЕРКА дескриптор↔PG-CHECK: набор типов из
// AllHeraldTypes обязан совпадать со значениями CHECK heralds_type_enum в
// миграции 091 (иначе домен примет тип, который БД отвергнет INSERT-ом, или
// наоборот). Guard от расхождения двух из трёх мест (третье — huma-enum,
// сверяется в пакете api).
func TestAllHeraldTypes_MatchesPGCheck(t *testing.T) {
	// Миграция 091 — производный от herald.AllHeraldTypes контракт; читаем её
	// исходник (embed FS пакета migrations herald напрямую не импортирует).
	const migPath = "../../migrations/091_extend_heralds_type.up.sql"
	b, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("read %s: %v", migPath, err)
	}
	checkTypes := parseCheckInValues(t, string(b), "heralds_type_enum")

	got := make([]string, 0, len(AllHeraldTypes()))
	for _, ty := range AllHeraldTypes() {
		got = append(got, string(ty))
	}
	sort.Strings(got)
	sort.Strings(checkTypes)

	if strings.Join(got, ",") != strings.Join(checkTypes, ",") {
		t.Fatalf("herald.AllHeraldTypes=%v != CHECK heralds_type_enum=%v — дескриптор и PG-CHECK разошлись", got, checkTypes)
	}
}

// parseCheckInValues извлекает значения `IN ('a', 'b')` последнего ADD CONSTRAINT
// <name> ... CHECK (type IN (...)) из up.sql. Достаточно для сверочного теста.
func parseCheckInValues(t *testing.T, sql, constraintName string) []string {
	t.Helper()
	// Берём фрагмент после ADD CONSTRAINT <name> до закрывающей `)` группы IN.
	idx := strings.LastIndex(sql, "ADD CONSTRAINT "+constraintName)
	if idx < 0 {
		t.Fatalf("ADD CONSTRAINT %s not found in migration", constraintName)
	}
	tail := sql[idx:]
	re := regexp.MustCompile(`IN\s*\(([^)]*)\)`)
	m := re.FindStringSubmatch(tail)
	if m == nil {
		t.Fatalf("IN (...) not found after ADD CONSTRAINT %s", constraintName)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		v := strings.TrimSpace(part)
		v = strings.Trim(v, "'")
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// --- telegramTransport.BuildRequest (мок-KV, БЕЗ сети) ----------------

// staticKV — KVReader, отдающий фиксированный секрет. Без Vault/сети.
type staticKV struct {
	data map[string]any
	err  error
}

func (k staticKV) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	if k.err != nil {
		return nil, k.err
	}
	return k.data, nil
}

func TestTelegramTransport_BuildRequest(t *testing.T) {
	kv := staticKV{data: map[string]any{"bot_token": "123456:SECRET-TOKEN"}}
	h := &Herald{
		Name:    "ops-tg",
		Type:    HeraldTelegram,
		Enabled: true,
		Config: map[string]any{
			"bot_token_ref": "vault:secret/keeper/telegram#bot_token",
			"chat_id":       "@ops",
			"parse_mode":    "HTML",
		},
	}
	job := &DeliveryJob{
		EventType:   audit.EventScenarioRunFailed,
		Herald:      "ops-tg",
		Tiding:      "nightly",
		PayloadCopy: map[string]any{"voyage_id": "v1"},
	}

	dr, err := telegramTransport{}.BuildRequest(context.Background(), h, job, kv)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	// URL: https://api.telegram.org/bot<token>/sendMessage (токен из мок-KV).
	wantURL := "https://api.telegram.org/bot123456:SECRET-TOKEN/sendMessage"
	if dr.req.URL.String() != wantURL {
		t.Errorf("URL = %q, want %q", dr.req.URL.String(), wantURL)
	}
	if dr.req.Method != "POST" {
		t.Errorf("method = %q, want POST", dr.req.Method)
	}
	// Публичный API — opt-out флаги false (SSRF-guard проходит штатно).
	if dr.httpAllowed || dr.allowPrivate {
		t.Errorf("opt-out flags must be false for telegram, got httpAllowed=%v allowPrivate=%v", dr.httpAllowed, dr.allowPrivate)
	}

	// Тело: chat_id + text + parse_mode; text содержит event_type и payload.
	bodyBytes, _ := io.ReadAll(dr.req.Body)
	var msg telegramMessage
	if err := json.Unmarshal(bodyBytes, &msg); err != nil {
		t.Fatalf("body not telegram message JSON: %v (%s)", err, bodyBytes)
	}
	if msg.ChatID != "@ops" {
		t.Errorf("chat_id = %q, want @ops", msg.ChatID)
	}
	if msg.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", msg.ParseMode)
	}
	if !strings.Contains(msg.Text, "scenario_run.failed") {
		t.Errorf("text must mention event_type, got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "v1") {
		t.Errorf("text must include payload digest (voyage_id v1), got %q", msg.Text)
	}
}

// TestTelegramTransport_TokenNotInError — Vault-сбой резолва токена → ошибка
// transient (retryable), и сам ref/токен в текст ошибки не утекает (маскинг
// делает caller через maskErr, но и сырой текст токена держать нельзя).
func TestTelegramTransport_TokenNotInError(t *testing.T) {
	kv := staticKV{err: errors.New("vault unavailable")}
	h := &Herald{
		Name: "ops-tg", Type: HeraldTelegram, Enabled: true,
		Config: map[string]any{"bot_token_ref": "vault:secret/keeper/telegram#bot_token", "chat_id": "@ops"},
	}
	_, err := telegramTransport{}.BuildRequest(context.Background(), h, &DeliveryJob{EventType: audit.EventScenarioRunFailed}, kv)
	if err == nil {
		t.Fatal("expected vault error")
	}
	// Транзиентная (НЕ terminal-no-retry): Vault-сбой временный.
	if isTerminalNoRetry(err) {
		t.Errorf("vault failure must be transient (retryable), got terminal: %v", err)
	}
}

// TestTelegramTransport_MissingConfigTerminal — отсутствующий bot_token_ref/
// chat_id в резолве (config изменён после create) → terminal-no-retry (устойчиво).
func TestTelegramTransport_MissingConfigTerminal(t *testing.T) {
	kv := staticKV{data: map[string]any{"bot_token": "x"}}
	cases := []map[string]any{
		{"chat_id": "@ops"}, // нет bot_token_ref
		{"bot_token_ref": "vault:secret/keeper/telegram#bot_token"}, // нет chat_id
	}
	for _, cfg := range cases {
		h := &Herald{Name: "tg", Type: HeraldTelegram, Enabled: true, Config: cfg}
		_, err := telegramTransport{}.BuildRequest(context.Background(), h, &DeliveryJob{}, kv)
		if err == nil {
			t.Fatalf("config %v: expected error", cfg)
		}
		if !isTerminalNoRetry(err) {
			t.Errorf("config %v: missing field must be terminal-no-retry, got %v", cfg, err)
		}
	}
}

// TestTransportFor — HTTP-класс типы дают транспорт, неизвестный тип — нет.
func TestTransportFor(t *testing.T) {
	if _, ok := transportFor(HeraldWebhook); !ok {
		t.Error("webhook must have transport")
	}
	if _, ok := transportFor(HeraldTelegram); !ok {
		t.Error("telegram must have transport")
	}
	if _, ok := transportFor(HeraldType("mattermost")); ok {
		t.Error("unknown type must have no transport")
	}
}

// TestTelegramTransport_PassesSSRFGuard — SSRF-ИНВАРИАНТ: собранный telegram-URL
// проходит тот же guard, что webhook (validateDeliveryEndpoint с флагами из
// deliveryRequest). Публичный api.telegram.org валиден; guard — ЕДИНАЯ точка для
// всех HTTP-типов (deliver() зовёт его для любого транспорта).
func TestTelegramTransport_PassesSSRFGuard(t *testing.T) {
	kv := staticKV{data: map[string]any{"bot_token": "123:ABC"}}
	h := &Herald{
		Name: "ops-tg", Type: HeraldTelegram, Enabled: true,
		Config: map[string]any{"bot_token_ref": "vault:secret/keeper/telegram#bot_token", "chat_id": "@ops"},
	}
	dr, err := telegramTransport{}.BuildRequest(context.Background(), h, &DeliveryJob{EventType: audit.EventScenarioRunFailed}, kv)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if err := validateDeliveryEndpoint(dr.req.URL.String(), dr.httpAllowed, dr.allowPrivate); err != nil {
		t.Errorf("telegram URL must pass shared SSRF-guard, got %v", err)
	}
}
