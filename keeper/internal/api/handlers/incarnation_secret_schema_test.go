package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sealSchemaHandler собирает IncarnationHandler с loader+services для seal
// read-path-тестов secretSchemaForIncarnation. db/прочее nil — тест зовёт только
// schema-builder.
func sealSchemaHandler(loader *fakeLoader) *IncarnationHandler {
	return &IncarnationHandler{
		loader:   loader,
		services: &fakeResolver{ok: true},
	}
}

// collectStateSchemaSecrets обходит flat state_schema на secret:true (вложенность
// properties/items/additionalProperties).
func TestCollectStateSchemaSecrets(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_token": map[string]any{"type": "string", "secret": true},
			"replicas":    map[string]any{"type": "integer"},
			"tls": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":  map[string]any{"type": "string", "secret": true},
					"port": map[string]any{"type": "integer"},
				},
			},
			"acl": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
	set := audit.SecretPathSet{}
	collectStateSchemaSecrets(schema, "", set)

	for _, want := range []string{"admin_token", "tls.key", "acl[].password"} {
		if !set[want] {
			t.Errorf("secret-путь %q не собран: %v", want, set)
		}
	}
	if set["replicas"] || set["tls.port"] || set["acl[].name"] {
		t.Errorf("несекретный путь помечен — over-collect: %v", set)
	}
}

// secret НА САМОМ additionalProperties-узле (значение произвольного ключа map —
// secret) НЕ попадает в SecretPathSet: ни `map_field` (пометил бы всю map →
// over-mask на read-path), ни `map_field.*` (IsSecret такой путь не запрашивает →
// запись мёртвая). Деградация к vault+regex-слою маскинга намеренна (★-ограничение
// schema-слоя). Регрессия на ap-secret-ветку collectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesSecretLeaf(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"map_field": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string", "secret": true},
			},
		},
	}
	set := audit.SecretPathSet{}
	collectStateSchemaSecrets(schema, "", set)

	if set["map_field"] {
		t.Errorf("ap-secret-leaf пометил `map_field` — over-mask всей map: %v", set)
	}
	if set["map_field.*"] {
		t.Errorf("ap-secret-leaf пометил `map_field.*` — мёртвая запись (IsSecret такой путь не запрашивает): %v", set)
	}
	if len(set) != 0 {
		t.Errorf("ap-secret-leaf не должен давать ни одной записи (деградация к vault+regex): %v", set)
	}
}

// ap-узел БЕЗ secret, но с вложенными конкретными `properties` под secret: schema-
// слой ДОЛЖЕН покрыть точные имена (рекурсия в ap ведётся), а сам ap-узел — нет.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"users": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
	set := audit.SecretPathSet{}
	collectStateSchemaSecrets(schema, "", set)

	// ap-путь = имя map (`users`), вложенный конкретный `password` → `users.password`.
	if !set["users.password"] {
		t.Errorf("вложенный конкретный secret под ap не собран: %v", set)
	}
	if set["users"] {
		t.Errorf("сам ap-узел `users` помечен secret — over-mask: %v", set)
	}
}

// ★ Документирует ПРОБЕЛ (nit seal-review, НЕ фикс): собранный schema-путь
// `users.password` НЕ матчит реальный путь ячейки `users.<dynamic-key>.password`.
// Рекурсия в ap не вставляет сегмента под произвольный ключ → собирается
// `users.password`, а maskMapLayered ведёт путь по КОНКРЕТНОМУ ключу map
// (`users.alice.password`). [audit.SecretPathSet.IsSecret] сверяет точную форму И
// normalizeIdx — но normalizeIdx обобщает только индексы среза (`[N]`→`[]`), не
// map-ключи → совпадения нет. Schema-слой такой secret НЕ маскирует (деградация к
// vault+regex по имени `password`). Тест фиксирует ТЕКУЩЕЕ поведение: при появлении
// dynamic-key матчинга в IsSecret (отдельный слайс) ассерт `!IsSecret(...)` упадёт —
// сигнал обновить ограничение в collectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret_DynamicKeyGap(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"users": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
	set := audit.SecretPathSet{}
	collectStateSchemaSecrets(schema, "", set)

	// Собирается ap-путь без dynamic-key сегмента.
	if !set["users.password"] {
		t.Fatalf("ожидался собранный путь `users.password`: %v", set)
	}
	// ★ Текущее поведение: реальный путь ячейки с КОНКРЕТНЫМ ключом map НЕ матчится
	// schema-слоем (dynamic-key сегмент `alice` не покрыт; normalizeIdx его не трогает).
	if set.IsSecret("users.alice.password") {
		t.Errorf("IsSecret(users.alice.password) = true — gap неожиданно закрыт; обнови ограничение в collectStateSchemaSecrets")
	}
	// Контроль: обобщение idx не помогает — map-ключ не индекс среза.
	if set.IsSecret("users.bob.password") {
		t.Errorf("IsSecret(users.bob.password) = true — gap неожиданно закрыт; обнови ограничение в collectStateSchemaSecrets")
	}
}

// secretSchemaForIncarnation материализует снапшот и объединяет state_schema
// secret + create-scenario input secret под input.<name>.
func TestSecretSchemaForIncarnation_StateAndInput(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"admin_token": map[string]any{"type": "string", "secret": true},
			},
		},
		// create-scenario с secret input db_password.
		scenarioYAML: "name: create\ninput:\n  db_password: { type: string, secret: true }\n  hostname: { type: string }\n",
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}

	schema := h.secretSchemaForIncarnation(context.Background(), inc)
	if schema == nil {
		t.Fatal("schema nil — ожидалась непустая (state+input secret)")
	}
	if !schema.IsSecret("admin_token") {
		t.Errorf("state.admin_token не secret в схеме")
	}
	if !schema.IsSecret("input.db_password") {
		t.Errorf("spec.input.db_password не secret в схеме")
	}
	if schema.IsSecret("input.hostname") {
		t.Errorf("input.hostname помечен secret — over-collect")
	}
}

// loader-ошибка → nil-схема (best-effort, GET не падает).
func TestSecretSchemaForIncarnation_LoadErrorNil(t *testing.T) {
	loader := &fakeLoader{loadErr: context.DeadlineExceeded}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("при load-ошибке schema должна быть nil (best-effort): %v", schema)
	}
}

// nil-loader → nil-схема (деградация к MaskSecrets).
func TestSecretSchemaForIncarnation_NilDeps(t *testing.T) {
	h := &IncarnationHandler{}
	inc := &incarnation.Incarnation{Service: "redis"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("без loader/services schema должна быть nil: %v", schema)
	}
}

// (e) schema-объявленное secret-поле state → MASKED на read-path-проекции
// (toIncarnationGetView через секрет-схему сервиса).
func TestToIncarnationGetView_SchemaMasksDeclaredState(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				// admin_secret НЕ ловится regex по имени (нет secret-фрагмента в
				// `admin_secret`? — содержит `secret` → regex поймал бы). Берём имя БЕЗ
				// regex-фрагмента, чтобы доказать именно schema-слой.
				"join_value": map[string]any{"type": "string", "secret": true},
				"replicas":   map[string]any{"type": "integer"},
			},
		},
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{
		Service:        "redis",
		ServiceVersion: "v1",
		State: map[string]any{
			"join_value": "plaintext-secret-value",
			"replicas":   float64(3),
		},
	}
	schema := h.secretSchemaForIncarnation(context.Background(), inc)
	view := toIncarnationGetView(inc, schema)

	if view.State["join_value"] != "***MASKED***" {
		t.Errorf("schema-secret state.join_value = %v, want masked (e)", view.State["join_value"])
	}
	if view.State["replicas"] != float64(3) {
		t.Errorf("несекретный state.replicas = %v, want passthrough (нет over-masking)", view.State["replicas"])
	}
	// Хранимый state не мутирован.
	if inc.State["join_value"] != "plaintext-secret-value" {
		t.Errorf("исходный inc.State мутирован: %v", inc.State["join_value"])
	}
}

// (f) generic-поле state с конфигом → НЕ MASKED (нет over-masking) при пустой
// схеме секретов.
func TestToIncarnationGetView_GenericStateNotMasked(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"redis_config": map[string]any{"type": "object"}},
		},
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{
		Service:        "redis",
		ServiceVersion: "v1",
		State: map[string]any{
			"redis_config": map[string]any{"maxmemory": "256mb", "loglevel": "notice"},
		},
	}
	schema := h.secretSchemaForIncarnation(context.Background(), inc) // nil (нет secret)
	view := toIncarnationGetView(inc, schema)

	cfg := view.State["redis_config"].(map[string]any)
	if cfg["maxmemory"] != "256mb" || cfg["loglevel"] != "notice" {
		t.Errorf("generic redis_config замаскирован — over-masking: %v", cfg)
	}
}
