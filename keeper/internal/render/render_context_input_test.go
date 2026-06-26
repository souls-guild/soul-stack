package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// TestRenderContext_InputRootPresent — Вариант B (ADR-010 §3.2 amendment):
// render_context корня §3.2 несёт ключ `input` (резолвнутый operator-input)
// рядом с vars/self/role/essence. Шаблон читает `.input.<name>` напрямую, без
// passthrough `params.vars`. Заодно back-compat: `.vars.*` (поднятые автором в
// params.vars) НЕ сломаны — оба канала сосуществуют.
func TestRenderContext_InputRootPresent(t *testing.T) {
	const tmplPath = "templates/ctx.conf.tmpl"
	const tmplBody = "listen {{ .input.listen }}\n" +
		"level {{ .input.log_level }}\n" +
		"socket {{ .vars.socket }}\n" + // back-compat: явный params.vars-канал жив
		"family {{ .self.os.family }}\n"

	manifest := &config.ScenarioManifest{
		Name: "ctx-input",
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						// Только один поднятый vars (back-compat), остальное — через .input.
						"vars": map[string]any{"socket": "/run/app.sock"},
					},
				},
			},
		},
	}

	host := &topology.HostFacts{
		SID:       "app-1.example.com",
		Soulprint: map[string]any{"os": map[string]any{"family": "debian"}},
	}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"listen": "0.0.0.0:9100", "log_level": "info"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	fields := tasks[0].Params.GetFields()
	rc := fields[paramRenderContext].GetStructValue().AsMap()

	inputSec, ok := rc["input"].(map[string]any)
	if !ok {
		t.Fatalf("render_context.input отсутствует или не объект: %#v", rc["input"])
	}
	if inputSec["listen"] != "0.0.0.0:9100" || inputSec["log_level"] != "info" {
		t.Errorf("render_context.input неполон: %#v", inputSec)
	}
	// back-compat: vars-канал на месте.
	if vs, _ := rc["vars"].(map[string]any); vs["socket"] != "/run/app.sock" {
		t.Errorf("render_context.vars (back-compat) потерян: %#v", rc["vars"])
	}

	// Исполняем тем же движком, что Soul, render_context КОРНЕМ.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(fields[paramTemplateContent].GetStringValue(), rc)
	if err != nil {
		t.Fatalf("soul-render упал (.input.* недоступен в шаблоне?): %v", err)
	}
	for _, want := range []string{"listen 0.0.0.0:9100", "level info", "socket /run/app.sock", "family debian"} {
		if !strings.Contains(out, want) {
			t.Errorf("ожидалось %q в рендере:\n%s", want, out)
		}
	}
}

// TestRenderContext_BuildRenderContext_EmptyInput — buildRenderContext с
// injectInput=true и nil Input кладёт пустой map (не паникует, strict-mode на
// `.input.X` корректен).
func TestRenderContext_BuildRenderContext_EmptyInput(t *testing.T) {
	rc := buildRenderContext(RenderInput{}, &topology.HostFacts{}, nil, true)
	if _, ok := rc["input"].(map[string]any); !ok {
		t.Fatalf("render_context.input при nil Input должен быть пустым map, got %#v", rc["input"])
	}
}

// TestRenderContext_BuildRenderContext_InputOmitted — ★условная инъекция (Вариант
// B): injectInput=false → ключа `input` в render_context НЕТ (шаблоны на одних
// `.vars` сохраняют до-Вариант-B-вид {vars,self,role,essence}, deep-equal-фикстуры
// стабильны). vars/self/role/essence остаются всегда.
func TestRenderContext_BuildRenderContext_InputOmitted(t *testing.T) {
	rc := buildRenderContext(
		RenderInput{Input: map[string]any{"secret": "x"}},
		&topology.HostFacts{}, nil, false)
	if _, present := rc["input"]; present {
		t.Fatalf("при injectInput=false ключ input не должен присутствовать: %#v", rc)
	}
	for _, k := range []string{"vars", "self", "role", "essence"} {
		if _, ok := rc[k]; !ok {
			t.Errorf("render_context.%s должен присутствовать всегда: %#v", k, rc)
		}
	}
}

// TestRenderContext_VarsOnlyTemplate_NoInput — ★end-to-end условной инъекции
// через Render: шаблон читает ТОЛЬКО `.vars.*` → render_context НЕ содержит
// `input` (даже если в проходе есть operator-input). Это гарантия, что
// redis-подобные шаблоны (vars-only) не получают раздутый render_context и их
// deep-equal-фикстуры остаются зелёными.
func TestRenderContext_VarsOnlyTemplate_NoInput(t *testing.T) {
	const tmplPath = "templates/vars-only.conf.tmpl"
	// Тело несёт `.input` ТОЛЬКО в комментарии (как redis.conf.tmpl) — не обращение.
	const tmplBody = "# apply.input резолвится host-инвариантно, см. .input контракт\n" +
		"port {{ .vars.port }}\n"

	manifest := &config.ScenarioManifest{
		Name:  "vars-only",
		Input: config.InputSchemaMap{"listen": {Type: "string"}},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						"vars":     map[string]any{"port": "6379"},
					},
				},
			},
		},
	}
	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"listen": "0.0.0.0:9100"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if _, present := rc["input"]; present {
		t.Fatalf("vars-only шаблон НЕ должен получать render_context.input: %#v", rc)
	}
	if vs, _ := rc["vars"].(map[string]any); vs["port"] != "6379" {
		t.Errorf("render_context.vars потерян: %#v", rc["vars"])
	}
}

// TestSealS1_SecretInputSealedAndMasked — ★КЛЮЧЕВОЙ security guard-тест (ADR-010
// §7.4, механизм S-1, Вариант B). Доказывает закрытие seal-разрыва: при
// core.file.rendered с secret:true input-полем
//
//	(1) путь render_context.input.<secret> ПОМЕЧЕН sealed (in.Sealed);
//	(2) значение секрета по этому пути ЗАМАСКИРОВАНО в observable-выводе
//	    (status_details-подобный payload), plaintext НЕ утекает.
//
// Без декларативного S-1 (sealRenderContextInput) путь не был бы sealed —
// passthrough vars удалён, collectSealed по сырым params секрет уже не видит.
func TestSealS1_SecretInputSealedAndMasked(t *testing.T) {
	const tmplPath = "templates/secret.conf.tmpl"
	const secretVal = "s3cr3t-PLAINTEXT"

	manifest := &config.ScenarioManifest{
		Name: "secret-render",
		Input: config.InputSchemaMap{
			"admin_password": {Type: "string", Secret: true},
			"listen":         {Type: "string"},
		},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
					},
				},
			},
		},
	}

	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte("requirepass {{ .input.admin_password }}\n")}}

	sealed := NewSealedSet()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"admin_password": secretVal, "listen": "0.0.0.0:9100"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
		Sealed:      sealed,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// (1) путь render_context.input.admin_password помечен sealed; несекретный
	// listen — НЕ sealed (без over-seal).
	paths := sealed.Paths()
	const secretPath = "render_context.input.admin_password"
	if !paths[secretPath] {
		t.Fatalf("seal-разрыв НЕ закрыт: %q не sealed — секрет утечёт в observable. Paths=%v", secretPath, paths)
	}
	if paths["render_context.input.listen"] {
		t.Errorf("over-seal: несекретный listen помечен sealed: %v", paths)
	}

	// Секрет действительно доехал в render_context.input (для Soul он реальный — это
	// корректно, wire-канал не маскируется), но именно поэтому observable-каналы
	// обязаны его прятать по sealed-пути.
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if got, _ := rc["input"].(map[string]any); got["admin_password"] != secretVal {
		t.Fatalf("предусловие теста нарушено: секрет не доехал в render_context.input: %#v", got)
	}

	// (2) маскинг observable-payload по sealed-пути: значение секрета заменено,
	// plaintext отсутствует.
	payload := map[string]any{"render_context": map[string]any{"input": map[string]any{
		"admin_password": secretVal,
		"listen":         "0.0.0.0:9100",
	}}}
	masked := audit.MaskSecretsSealed(payload, audit.SealOpts{Sealed: paths})
	maskedInput := masked["render_context"].(map[string]any)["input"].(map[string]any)
	if maskedInput["admin_password"] == secretVal {
		t.Fatalf("seal-маскинг НЕ сработал: plaintext-секрет %q остался в observable payload", secretVal)
	}
	if maskedInput["listen"] != "0.0.0.0:9100" {
		t.Errorf("несекретный listen ошибочно замаскирован: %#v", maskedInput["listen"])
	}

	// Защита от утечки через свободный текст ошибки/деталей: тот же sealed-набор
	// прячет секрет и в строковом канале по пути render_context.input.admin_password.
	strPayload := map[string]any{secretPath: secretVal}
	maskedStr := audit.MaskSecretsSealed(strPayload, audit.SealOpts{Sealed: paths})
	if maskedStr[secretPath] == secretVal {
		t.Errorf("seal-маскинг по точному пути не сработал: %v", maskedStr)
	}
}

// TestSealS1_VarsOnlyTemplate_NoSeal — ★условный seal-гейт (Вариант B): при
// vars-only шаблоне (не читает `.input`) render_context.input НЕ инъектится →
// seal-путь render_context.input.<secret> НЕ метится, даже если в схеме есть
// secret-input. Так seal-набор синхронен реальному составу render_context (нет
// мёртвых sealed-путей на несуществующую ячейку), и секрет физически не
// попадает в render_context.input (его там нет).
func TestSealS1_VarsOnlyTemplate_NoSeal(t *testing.T) {
	const tmplPath = "templates/vars-only-secret.conf.tmpl"
	const tmplBody = "port {{ .vars.port }}\n" // не читает .input

	manifest := &config.ScenarioManifest{
		Name: "vars-only-secret",
		Input: config.InputSchemaMap{
			"admin_password": {Type: "string", Secret: true},
		},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						"vars":     map[string]any{"port": "6379"},
					},
				},
			},
		},
	}
	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	sealed := NewSealedSet()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"admin_password": "s3cr3t"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
		Sealed:      sealed,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if paths := sealed.Paths(); paths["render_context.input.admin_password"] {
		t.Errorf("vars-only шаблон не должен плодить seal-путь на input: %v", paths)
	}
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if _, present := rc["input"]; present {
		t.Fatalf("секрет не должен попасть в render_context.input у vars-only шаблона: %#v", rc)
	}
}

// TestSealS1_VaultScopeInputSealed — поле с vault_scope (по контракту требует
// secret:true) тоже попадает в sealed render_context.input.<field>. Проверяем,
// что секрет с scoped-vault не утекает в observable.
func TestSealS1_VaultScopeInputSealed(t *testing.T) {
	set := NewSealedSet()
	in := RenderInput{Scenario: &config.ScenarioManifest{Input: config.InputSchemaMap{
		"tls_key": {Type: "string", Secret: true, VaultScope: "secret/app/*"},
		"plain":   {Type: "string"},
	}}}
	sealRenderContextInput(set, in)
	paths := set.Paths()
	if !paths["render_context.input.tls_key"] {
		t.Errorf("vault_scope-поле tls_key не sealed: %v", paths)
	}
	if paths["render_context.input.plain"] {
		t.Errorf("over-seal несекретного plain: %v", paths)
	}
}

// TestSealS1_NilSetNoop — nil-Sealed (push/trial/Acolyte) → no-op, не паникует.
func TestSealS1_NilSetNoop(t *testing.T) {
	sealRenderContextInput(nil, RenderInput{Scenario: &config.ScenarioManifest{
		Input: config.InputSchemaMap{"pw": {Type: "string", Secret: true}},
	}})
}

// TestSecretInputNames_VaultScope — secretInputNames собирает и secret:true, и
// поля с vault_scope (defense-in-depth: имя секрета не зависит от инварианта
// чужого валидатора, что vault_scope ⇒ secret).
func TestSecretInputNames_VaultScope(t *testing.T) {
	scn := &config.ScenarioManifest{Input: config.InputSchemaMap{
		"a": {Type: "string", Secret: true},
		"b": {Type: "string", VaultScope: "secret/x/*"},
		"c": {Type: "string"},
	}}
	got := secretInputNames(scn)
	if !got["a"] || !got["b"] {
		t.Errorf("secret/vault_scope-имена не собраны: %v", got)
	}
	if got["c"] {
		t.Errorf("несекретное c попало в набор: %v", got)
	}
}
