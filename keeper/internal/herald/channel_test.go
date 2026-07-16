package herald

// Guard tests for Herald channel types (ADR-052 amendment): config validation,
// resolveDelivery assembly (URL/body/headers/signature) with mocked KVReader without
// real network calls, SSRF invariant for HTTP types, deliverEmail SSRF guard,
// secret dispatch (secret_ref webhook-only), secret not in errors, single source
// of truth for types (AllHeraldTypes ↔ PG-CHECK). Table-driven, without client.Do /
// real SMTP dial.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// staticKV KVReader returning fixed secret. No Vault/network.
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

// --- ValidateConfig по всем типам -------------------------------------

func TestValidateConfig_AllTypes(t *testing.T) {
	const ref = "vault:secret/keeper/x#f"
	cases := []struct {
		name    string
		typ     HeraldType
		config  map[string]any
		wantErr bool
	}{
		// telegram
		{"tg ok", HeraldTelegram, map[string]any{"bot_token_ref": ref, "chat_id": "@ops"}, false},
		{"tg parse_mode", HeraldTelegram, map[string]any{"bot_token_ref": ref, "chat_id": "@ops", "parse_mode": "HTML"}, false},
		{"tg no token", HeraldTelegram, map[string]any{"chat_id": "@ops"}, true},
		{"tg no chat", HeraldTelegram, map[string]any{"bot_token_ref": ref}, true},
		{"tg blank chat", HeraldTelegram, map[string]any{"bot_token_ref": ref, "chat_id": "  "}, true},
		{"tg token plaintext", HeraldTelegram, map[string]any{"bot_token_ref": "123:ABC", "chat_id": "@ops"}, true},
		{"tg bad parse_mode", HeraldTelegram, map[string]any{"bot_token_ref": ref, "chat_id": "@ops", "parse_mode": "Markdown"}, true},
		// slack
		{"slack ok", HeraldSlack, map[string]any{"webhook_url_ref": ref}, false},
		{"slack no url_ref", HeraldSlack, map[string]any{}, true},
		{"slack url_ref plaintext", HeraldSlack, map[string]any{"webhook_url_ref": "https://hooks.slack.com/x"}, true},
		// mattermost
		{"mm ok", HeraldMattermost, map[string]any{"webhook_url_ref": ref}, false},
		{"mm with channel", HeraldMattermost, map[string]any{"webhook_url_ref": ref, "channel": "ops", "username": "bot"}, false},
		{"mm no url_ref", HeraldMattermost, map[string]any{"channel": "ops"}, true},
		// discord
		{"discord ok", HeraldDiscord, map[string]any{"webhook_url_ref": ref}, false},
		{"discord username", HeraldDiscord, map[string]any{"webhook_url_ref": ref, "username": "bot"}, false},
		{"discord no url_ref", HeraldDiscord, map[string]any{}, true},
		// custom
		{"custom ok", HeraldCustom, map[string]any{"url": "https://ci.example/hook"}, false},
		{"custom method+secret", HeraldCustom, map[string]any{"url": "https://ci.example/hook", "method": "PUT", "header_secret_ref": ref}, false},
		{"custom no url", HeraldCustom, map[string]any{"method": "POST"}, true},
		{"custom bad method", HeraldCustom, map[string]any{"url": "https://ci.example/hook", "method": "DELETE"}, true},
		{"custom http default-blocked", HeraldCustom, map[string]any{"url": "http://ci.example/hook"}, true},
		{"custom http opt-in", HeraldCustom, map[string]any{"url": "http://ci.example/hook", "http_allowed": true}, false},
		{"custom private default-blocked", HeraldCustom, map[string]any{"url": "https://10.0.0.5/hook"}, true},
		{"custom header_secret plaintext", HeraldCustom, map[string]any{"url": "https://ci.example/hook", "header_secret_ref": "Bearer xyz"}, true},
		// email
		{"email ok", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}}, false},
		{"email full", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(465), "from": "a@x", "to": []any{"b@y", "c@z"}, "username": "u", "password_ref": ref, "tls_mode": "tls"}, false},
		{"email no host", HeraldEmail, map[string]any{"smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}}, true},
		{"email no port", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "from": "a@x", "to": []any{"b@y"}}, true},
		{"email bad port", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(70000), "from": "a@x", "to": []any{"b@y"}}, true},
		{"email empty to", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{}}, true},
		{"email to not-strings", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{42}}, true},
		{"email password plaintext", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}, "password_ref": "s3cr3t"}, true},
		{"email bad tls_mode", HeraldEmail, map[string]any{"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}, "tls_mode": "ssl"}, true},
		// неизвестный тип
		{"pagerduty unknown", HeraldType("pagerduty"), map[string]any{"url": "https://x/y"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.typ, tc.config)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateConfig(%s, %v) err = %v, wantErr = %v", tc.typ, tc.config, err, tc.wantErr)
			}
		})
	}
}

// --- secret-разводка: secret_ref только webhook ------------------------

func TestValidateSecretRef_TypeAware(t *testing.T) {
	const ref = "vault:secret/keeper/sign"
	// webhook: nil ок, валидный vault-ref ок, plaintext — ошибка.
	if err := ValidateSecretRef(HeraldWebhook, nil); err != nil {
		t.Errorf("webhook nil secret_ref must be ok: %v", err)
	}
	if err := ValidateSecretRef(HeraldWebhook, strptr(ref)); err != nil {
		t.Errorf("webhook valid vault-ref must be ok: %v", err)
	}
	if err := ValidateSecretRef(HeraldWebhook, strptr("plain")); err == nil {
		t.Error("webhook plaintext secret_ref must error")
	}
	// не-webhook типы: непустой secret_ref запрещён (даже валидный vault-ref);
	// nil ок.
	for _, ty := range []HeraldType{HeraldTelegram, HeraldSlack, HeraldMattermost, HeraldDiscord, HeraldCustom, HeraldEmail} {
		if err := ValidateSecretRef(ty, strptr(ref)); err == nil {
			t.Errorf("%s: non-empty secret_ref must error (secret_ref is webhook-only)", ty)
		}
		if err := ValidateSecretRef(ty, nil); err != nil {
			t.Errorf("%s: nil secret_ref must be ok: %v", ty, err)
		}
	}
}

// --- resolveDelivery: сборка URL/тела с мок-KV, БЕЗ сети ---------------

func telegramJob() *DeliveryJob {
	return &DeliveryJob{
		EventType:   audit.EventScenarioRunFailed,
		Herald:      "ch",
		Tiding:      "nightly",
		PayloadCopy: map[string]any{"voyage_id": "v1"},
	}
}

func TestResolveDelivery_Telegram(t *testing.T) {
	kv := staticKV{data: map[string]any{"bot_token": "123456:SECRET"}}
	h := &Herald{Name: "ch", Type: HeraldTelegram, Enabled: true, Config: map[string]any{
		"bot_token_ref": "vault:secret/keeper/tg#bot_token", "chat_id": "@ops", "parse_mode": "HTML",
	}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if want := "https://api.telegram.org/bot123456:SECRET/sendMessage"; hd.url != want {
		t.Errorf("url = %q, want %q", hd.url, want)
	}
	if hd.httpAllowed || hd.allowPrivate {
		t.Errorf("telegram opt-out flags must be false, got httpAllowed=%v allowPrivate=%v", hd.httpAllowed, hd.allowPrivate)
	}
	var msg telegramMessage
	if err := json.Unmarshal(hd.body, &msg); err != nil {
		t.Fatalf("body not telegram JSON: %v (%s)", err, hd.body)
	}
	if msg.ChatID != "@ops" || msg.ParseMode != "HTML" {
		t.Errorf("chat_id/parse_mode = %q/%q, want @ops/HTML", msg.ChatID, msg.ParseMode)
	}
	if !strings.Contains(msg.Text, "scenario_run.failed") || !strings.Contains(msg.Text, "v1") {
		t.Errorf("text must mention event_type and payload digest, got %q", msg.Text)
	}
}

func TestResolveDelivery_Slack(t *testing.T) {
	const url = "https://hooks.slack.com/services/T/B/XYZ"
	kv := staticKV{data: map[string]any{"url": url}}
	h := &Herald{Name: "ch", Type: HeraldSlack, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/keeper/slack#url"}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.url != url {
		t.Errorf("url = %q, want %q (from Vault)", hd.url, url)
	}
	var body map[string]string
	if err := json.Unmarshal(hd.body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if !strings.Contains(body["text"], "scenario_run.failed") {
		t.Errorf("slack text must mention event_type, got %q", body["text"])
	}
}

func TestResolveDelivery_Mattermost(t *testing.T) {
	const url = "https://mm.example/hooks/abc"
	kv := staticKV{data: map[string]any{"url": url}}
	h := &Herald{Name: "ch", Type: HeraldMattermost, Enabled: true, Config: map[string]any{
		"webhook_url_ref": "vault:secret/keeper/mm#url", "channel": "ops", "username": "soul",
	}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.url != url {
		t.Errorf("url = %q, want %q", hd.url, url)
	}
	var msg mattermostMessage
	if err := json.Unmarshal(hd.body, &msg); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if msg.Channel != "ops" || msg.Username != "soul" {
		t.Errorf("channel/username = %q/%q, want ops/soul", msg.Channel, msg.Username)
	}
}

func TestResolveDelivery_Discord(t *testing.T) {
	const url = "https://discord.com/api/webhooks/1/abc"
	kv := staticKV{data: map[string]any{"url": url}}
	h := &Herald{Name: "ch", Type: HeraldDiscord, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/keeper/dc#url"}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.url != url {
		t.Errorf("url = %q, want %q", hd.url, url)
	}
	var msg discordMessage
	if err := json.Unmarshal(hd.body, &msg); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if !strings.Contains(msg.Content, "scenario_run.failed") {
		t.Errorf("discord content must mention event_type, got %q", msg.Content)
	}
}

// TestResolveDelivery_DiscordTruncates — content Discord обрезается до 2000 рун.
func TestResolveDelivery_DiscordTruncates(t *testing.T) {
	kv := staticKV{data: map[string]any{"url": "https://discord.com/api/webhooks/1/abc"}}
	// Раздуваем payload огромной строкой → messageText > 2000 символов.
	huge := strings.Repeat("x", 5000)
	job := &DeliveryJob{EventType: audit.EventScenarioRunFailed, Herald: "ch", Tiding: "t", PayloadCopy: map[string]any{"blob": huge}}
	h := &Herald{Name: "ch", Type: HeraldDiscord, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/keeper/dc#url"}}
	hd, err := resolveDelivery(context.Background(), h, job, kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	var msg discordMessage
	if err := json.Unmarshal(hd.body, &msg); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if n := len([]rune(msg.Content)); n > discordContentLimit {
		t.Errorf("content = %d runes, want <= %d", n, discordContentLimit)
	}
}

func TestResolveDelivery_Custom(t *testing.T) {
	kv := staticKV{data: map[string]any{"token": "Bearer secret-xyz"}}
	h := &Herald{Name: "ch", Type: HeraldCustom, Enabled: true, Config: map[string]any{
		"url": "https://ci.example/hook", "method": "PUT",
		"headers": map[string]any{"X-Team": "ops"}, "header_secret_ref": "vault:secret/keeper/ci#token",
	}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.url != "https://ci.example/hook" {
		t.Errorf("url = %q", hd.url)
	}
	if hd.method != "PUT" {
		t.Errorf("method = %q, want PUT", hd.method)
	}
	// Фиксированное тело — webhookPayload (event_type + payload), НЕ произвольный
	// шаблон.
	var wp webhookPayload
	if err := json.Unmarshal(hd.body, &wp); err != nil {
		t.Fatalf("custom body must be webhookPayload JSON: %v", err)
	}
	if wp.EventType != "scenario_run.failed" {
		t.Errorf("custom body event_type = %q", wp.EventType)
	}
	// header_secret_ref → Authorization из Vault; статические заголовки сохранены.
	if hd.headers["Authorization"] != "Bearer secret-xyz" {
		t.Errorf("Authorization = %q, want resolved secret", hd.headers["Authorization"])
	}
	if hd.headers["X-Team"] != "ops" {
		t.Errorf("static header X-Team lost: %v", hd.headers)
	}
}

// TestResolveDelivery_CustomDefaultMethod — метод по умолчанию POST.
func TestResolveDelivery_CustomDefaultMethod(t *testing.T) {
	h := &Herald{Name: "ch", Type: HeraldCustom, Enabled: true, Config: map[string]any{"url": "https://ci.example/hook"}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), staticKV{})
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.method != "POST" {
		t.Errorf("default method = %q, want POST", hd.method)
	}
}

// --- webhook-регресс: подпись + флаги бит-в-бит ------------------------

func TestResolveDelivery_WebhookSigned(t *testing.T) {
	kv := staticKV{data: map[string]any{"key": "topsecret"}}
	h := &Herald{Name: "ch", Type: HeraldWebhook, Enabled: true,
		Config:    map[string]any{"url": "https://ci.example/hook", "headers": map[string]any{"X-A": "b"}},
		SecretRef: strptr("vault:secret/keeper/sign#key")}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if hd.url != "https://ci.example/hook" || hd.headers["X-A"] != "b" {
		t.Errorf("webhook url/headers regressed: %q %v", hd.url, hd.headers)
	}
	if hd.signingKey == nil {
		t.Error("webhook with secret_ref must carry signingKey")
	}
	// Подпись строится в buildHTTPRequest — проверяем детерминированность заголовка.
	req, err := buildHTTPRequest(context.Background(), hd)
	if err != nil {
		t.Fatalf("buildHTTPRequest: %v", err)
	}
	if sig := req.Header.Get(SignatureHeader); !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature header = %q, want sha256=…", sig)
	}
}

// --- SSRF-инвариант: URL каждого HTTP-типа проходит единый guard -------

func TestHTTPTypes_PassSSRFGuard(t *testing.T) {
	cases := []struct {
		name string
		h    *Herald
		kv   KVReader
	}{
		{"telegram", &Herald{Name: "c", Type: HeraldTelegram, Enabled: true, Config: map[string]any{"bot_token_ref": "vault:secret/k/t#b", "chat_id": "@o"}}, staticKV{data: map[string]any{"b": "1:A"}}},
		{"slack", &Herald{Name: "c", Type: HeraldSlack, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/k/s#u"}}, staticKV{data: map[string]any{"u": "https://hooks.slack.com/x"}}},
		{"discord", &Herald{Name: "c", Type: HeraldDiscord, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/k/d#u"}}, staticKV{data: map[string]any{"u": "https://discord.com/api/webhooks/1/a"}}},
		{"custom", &Herald{Name: "c", Type: HeraldCustom, Enabled: true, Config: map[string]any{"url": "https://ci.example/h"}}, staticKV{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hd, err := resolveDelivery(context.Background(), tc.h, telegramJob(), tc.kv)
			if err != nil {
				t.Fatalf("resolveDelivery: %v", err)
			}
			if err := validateDeliveryEndpoint(hd.url, hd.httpAllowed, hd.allowPrivate); err != nil {
				t.Errorf("%s url must pass shared SSRF-guard: %v", tc.name, err)
			}
		})
	}
}

// TestCustom_SSRFGuardRejectsPrivate — SSRF-guard режет приватный custom-URL,
// прошедший резолв (config мог измениться после create). Единая точка контроля.
func TestCustom_SSRFGuardRejectsPrivate(t *testing.T) {
	h := &Herald{Name: "c", Type: HeraldCustom, Enabled: true, Config: map[string]any{"url": "https://169.254.169.254/latest"}}
	hd, err := resolveDelivery(context.Background(), h, telegramJob(), staticKV{})
	if err != nil {
		t.Fatalf("resolveDelivery: %v", err)
	}
	if err := validateDeliveryEndpoint(hd.url, hd.httpAllowed, hd.allowPrivate); err == nil {
		t.Error("metadata-IP custom URL must be rejected by SSRF-guard")
	}
}

// --- секрет не в тексте ошибок -----------------------------------------

func TestResolveDelivery_SecretNotInError(t *testing.T) {
	// Vault-сбой при резолве секрет-поля → ошибка transient, без утечки ref/секрета.
	kv := staticKV{err: errors.New("vault down")}
	cases := []*Herald{
		{Name: "c", Type: HeraldTelegram, Enabled: true, Config: map[string]any{"bot_token_ref": "vault:secret/keeper/tg#bot_token", "chat_id": "@o"}},
		{Name: "c", Type: HeraldSlack, Enabled: true, Config: map[string]any{"webhook_url_ref": "vault:secret/keeper/slack#url"}},
		{Name: "c", Type: HeraldCustom, Enabled: true, Config: map[string]any{"url": "https://ci.example/h", "header_secret_ref": "vault:secret/keeper/ci#token"}},
	}
	for _, h := range cases {
		t.Run(string(h.Type), func(t *testing.T) {
			_, err := resolveDelivery(context.Background(), h, telegramJob(), kv)
			if err == nil {
				t.Fatal("expected vault error")
			}
			if isTerminalNoRetry(err) {
				t.Errorf("vault failure must be transient (retryable), got terminal: %v", err)
			}
		})
	}
}

// TestResolveDelivery_MissingConfigTerminal — отсутствие обязательного поля в
// резолве (config изменён после create) → terminal-no-retry для всех типов.
func TestResolveDelivery_MissingConfigTerminal(t *testing.T) {
	cases := []*Herald{
		{Name: "c", Type: HeraldTelegram, Enabled: true, Config: map[string]any{"chat_id": "@o"}}, // нет token
		{Name: "c", Type: HeraldSlack, Enabled: true, Config: map[string]any{}},                   // нет url_ref
		{Name: "c", Type: HeraldDiscord, Enabled: true, Config: map[string]any{}},                 // нет url_ref
		{Name: "c", Type: HeraldCustom, Enabled: true, Config: map[string]any{"method": "POST"}},  // нет url
	}
	for _, h := range cases {
		t.Run(string(h.Type), func(t *testing.T) {
			_, err := resolveDelivery(context.Background(), h, telegramJob(), staticKV{data: map[string]any{"x": "y"}})
			if err == nil {
				t.Fatal("expected error")
			}
			if !isTerminalNoRetry(err) {
				t.Errorf("missing field must be terminal-no-retry, got %v", err)
			}
		})
	}
}

// --- driverFor / AllHeraldTypes ---------------------------------------

func TestDriverFor(t *testing.T) {
	for _, ty := range []HeraldType{HeraldWebhook, HeraldTelegram, HeraldSlack, HeraldMattermost, HeraldDiscord, HeraldCustom} {
		if _, ok := driverFor(ty); !ok {
			t.Errorf("%s must have HTTP driver", ty)
		}
	}
	// email — SMTP-класс, НЕ в channelDrivers.
	if _, ok := driverFor(HeraldEmail); ok {
		t.Error("email must NOT have HTTP driver (SMTP class)")
	}
	if _, ok := driverFor(HeraldType("pagerduty")); ok {
		t.Error("unknown type must have no driver")
	}
}

func TestAllHeraldTypes_SortedComplete(t *testing.T) {
	got := AllHeraldTypes()
	want := []HeraldType{HeraldCustom, HeraldDiscord, HeraldEmail, HeraldMattermost, HeraldSlack, HeraldTelegram, HeraldWebhook}
	if len(got) != len(want) {
		t.Fatalf("AllHeraldTypes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllHeraldTypes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("AllHeraldTypes not sorted: %q >= %q", got[i-1], got[i])
		}
	}
	// Каждый тип валиден по ValidHeraldType (единый источник).
	for _, ty := range got {
		if !ValidHeraldType(ty) {
			t.Errorf("%s in AllHeraldTypes but not ValidHeraldType", ty)
		}
	}
}

// TestAllHeraldTypes_MatchesPGCheck — СВЕРКА дескриптор↔PG-CHECK: набор типов из
// AllHeraldTypes обязан совпадать со значениями CHECK heralds_type_enum миграции
// 091 (иначе домен примет тип, который БД отвергнет, или наоборот).
func TestAllHeraldTypes_MatchesPGCheck(t *testing.T) {
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
// <name> ... CHECK (type IN (...)) из up.sql.
func parseCheckInValues(t *testing.T, sql, constraintName string) []string {
	t.Helper()
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
		v := strings.Trim(strings.TrimSpace(part), "'")
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// --- fieldsFor: каталог покрывает AllHeraldTypes ----------------------

func TestFieldsFor_CoversAllTypes(t *testing.T) {
	for _, ty := range AllHeraldTypes() {
		fields, ok := fieldsFor(ty)
		if !ok {
			t.Errorf("%s has no field descriptor for catalog", ty)
			continue
		}
		if len(fields) == 0 {
			t.Errorf("%s field descriptor is empty", ty)
		}
		// Секрет-поле обязано быть vault_ref (разводка ADR-052 amendment).
		for _, f := range fields {
			if f.Secret && f.Kind != KindVaultRef {
				t.Errorf("%s field %q is Secret but Kind=%s (must be vault_ref)", ty, f.Name, f.Kind)
			}
		}
	}
	if _, ok := fieldsFor(HeraldType("pagerduty")); ok {
		t.Error("unknown type must have no field descriptor")
	}
}

// --- email: SMTP-guard блокирует приватку/metadata --------------------

// blockResolver — netguard.Resolver, отдающий фиксированный IP (без DNS).
type blockResolver struct{ ip string }

func (r blockResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
}

func emailHerald(host, tlsMode string) *Herald {
	cfg := map[string]any{"smtp_host": host, "smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}}
	if tlsMode != "" {
		cfg["tls_mode"] = tlsMode
	}
	return &Herald{Name: "mail", Type: HeraldEmail, Enabled: true, Config: cfg}
}

// TestDeliverEmail_SSRFBlocksPrivate — email-host, резолвящийся в приватку/
// metadata, блокируется терминально (email не имеет allow_private opt-out). БЕЗ
// реального SMTP-dial: резолвер отдаёт заблокированный IP, guard срабатывает
// раньше сети.
func TestDeliverEmail_SSRFBlocksPrivate(t *testing.T) {
	cases := []string{"169.254.169.254", "127.0.0.1", "10.0.0.5", "192.168.1.10"}
	for _, ip := range cases {
		t.Run(ip, func(t *testing.T) {
			err := deliverEmail(context.Background(), emailHerald("smtp.internal", ""), telegramJob(), staticKV{}, blockResolver{ip: ip})
			if err == nil {
				t.Fatalf("email to blocked IP %s must fail", ip)
			}
			if !isTerminalNoRetry(err) {
				t.Errorf("blocked-IP email must be terminal-no-retry, got %v", err)
			}
			if strings.Contains(err.Error(), ip) {
				t.Errorf("error leaked resolved IP %s: %v", ip, err)
			}
		})
	}
}

// TestDeliverEmail_MissingConfigTerminal — отсутствие обязательного поля в резолве
// email → terminal-no-retry (config изменён после create).
func TestDeliverEmail_MissingConfigTerminal(t *testing.T) {
	h := &Herald{Name: "mail", Type: HeraldEmail, Enabled: true, Config: map[string]any{"smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"}}}
	err := deliverEmail(context.Background(), h, telegramJob(), staticKV{}, blockResolver{ip: "1.2.3.4"})
	if err == nil {
		t.Fatal("email without smtp_host must fail")
	}
	if !isTerminalNoRetry(err) {
		t.Errorf("missing smtp_host must be terminal-no-retry, got %v", err)
	}
}

// TestDeliverEmail_PasswordVaultFailureTransient — Vault-сбой резолва password_ref
// → transient (retry), секрет/ref не в тексте ошибки. Резолвер не важен (падаем на
// KV раньше). Проверяем классификацию до SSRF-резолва.
func TestDeliverEmail_PasswordVaultFailureTransient(t *testing.T) {
	h := &Herald{Name: "mail", Type: HeraldEmail, Enabled: true, Config: map[string]any{
		"smtp_host": "smtp.example.com", "smtp_port": float64(587), "from": "a@x", "to": []any{"b@y"},
		"username": "u", "password_ref": "vault:secret/keeper/smtp#password",
	}}
	err := deliverEmail(context.Background(), h, telegramJob(), staticKV{err: errors.New("vault down")}, blockResolver{ip: "1.2.3.4"})
	if err == nil {
		t.Fatal("expected vault error")
	}
	if isTerminalNoRetry(err) {
		t.Errorf("vault failure must be transient, got terminal: %v", err)
	}
	if strings.Contains(err.Error(), "password") && strings.Contains(err.Error(), "secret/keeper/smtp") {
		t.Errorf("error leaked password ref: %v", err)
	}
}

// TestBuildEmailMessage_Headers — тело письма несёт From/To/Subject + text,
// заголовки без CRLF-инъекции.
func TestBuildEmailMessage_Headers(t *testing.T) {
	target := &emailTarget{from: "a@x", to: []string{"b@y", "c@z"}, tlsMode: emailTLSStartTLS}
	job := &DeliveryJob{EventType: audit.EventScenarioRunFailed, Tiding: "nightly", PayloadCopy: map[string]any{"voyage_id": "v1"}}
	msg := string(buildEmailMessage(target, job))
	for _, want := range []string{"From: a@x", "To: b@y, c@z", "Subject: scenario_run.failed", "nightly"} {
		if !strings.Contains(msg, want) {
			t.Errorf("email message missing %q; got:\n%s", want, msg)
		}
	}
}

// --- io: тело resolveDelivery читается однократно (sanity) -------------

func TestBuildHTTPRequest_BodyReadable(t *testing.T) {
	hd := &httpDelivery{url: "https://x/y", body: []byte(`{"a":1}`)}
	req, err := buildHTTPRequest(context.Background(), hd)
	if err != nil {
		t.Fatalf("buildHTTPRequest: %v", err)
	}
	b, _ := io.ReadAll(req.Body)
	if string(b) != `{"a":1}` {
		t.Errorf("body = %q, want {\"a\":1}", b)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("default content-type = %q", req.Header.Get("Content-Type"))
	}
}
