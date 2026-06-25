package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// redisTemplatesDir — каталог .tmpl destiny redis относительно этого пакета.
// Тест живёт здесь по той же причине, что destiny_node_exporter_tmpl_test.go:
// пакет владеет интеграцией с text/template-движком Soul-а (shared/tmpl) и общим
// путём рендера .tmpl. L0-trial этой destiny ассертит ПЛАН и гоняет только
// CEL-фазу — text/template-фазу для шаблонов не исполняет, поэтому регресс
// порядка строк users.acl на L0 не виден.
const redisTemplatesDir = "../../../examples/destiny/redis/templates"

// renderRedisTmpl рендерит один .tmpl destiny redis через тот же shared/tmpl.Engine,
// что и Soul (strict, missingkey=error). Падение Parse/Execute = провал теста.
func renderRedisTmpl(t *testing.T, name string, root map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(redisTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestRedisUsersAcl_DeterministicOrder — ПРЯМОЙ regress-guard на tiling-критичный
// баг недетерминизма users.acl (QA 2026-06-22). Корень бага: ACL рендерился из
// СПИСКА юзеров, а Go text/template range по list сохраняет порядок источника,
// который для коллекции из CEL `.map(...)` над map наследует НЕДЕТЕРМИНИРОВАННУЮ
// итерацию Go-map → строки users.acl в разном порядке между прогонами → ложный
// change в core.file.rendered → лишний рестарт Redis (на rolling-restart флоте
// каскадный).
//
// Фикс: users.acl.tmpl range-ит по MAP (Go сортирует ключи). Тест рендерит
// РЕАЛЬНЫЙ шаблон с именами, НАРУШАЮЩИМИ порядок вставки (zeta/alpha/mike), и
// доказывает, что (а) строки идут в ОТСОРТИРОВАННОМ порядке имён, (б) результат
// стабилен на N прогонах подряд. Возврат к list-рендеру завалит оба ассерта.
func TestRedisUsersAcl_DeterministicOrder(t *testing.T) {
	// vars.users — MAP имя→{perms,state,password}, как его собирает scenario
	// (merge(list(map)) над .map(...)). Имена специально не отсортированы и не в
	// порядке, в котором Go-map их вернул бы итерацией.
	root := map[string]any{
		"vars": map[string]any{
			"users": map[string]any{
				"zeta":  map[string]any{"perms": "~* +@all", "state": "on", "password": "zeta-pass"},
				"alpha": map[string]any{"perms": "~app:* +@read", "state": "on", "password": "alpha-pass"},
				"mike":  map[string]any{"perms": "~m:* +@write", "state": "off", "password": "mike-pass"},
			},
		},
	}

	const runs = 16
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "users.acl.tmpl", root)

		// Имена юзеров идут в ОТСОРТИРОВАННОМ порядке: alpha < mike < zeta.
		lines := nonEmptyLines(out)
		if len(lines) != 3 {
			t.Fatalf("ожидалось 3 строки user, получено %d:\n%s", len(lines), out)
		}
		gotNames := []string{userName(lines[0]), userName(lines[1]), userName(lines[2])}
		wantNames := []string{"alpha", "mike", "zeta"}
		for j := range wantNames {
			if gotNames[j] != wantNames[j] {
				t.Fatalf("порядок юзеров = %v, want %v (range по map → сортировка ключей)\n%s", gotNames, wantNames, out)
			}
		}

		// Стабильность между прогонами (детерминизм): каждый прогон идентичен.
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("прогон %d дал ИНОЙ вывод, чем прогон 0 — недетерминизм:\n--- прогон 0 ---\n%s\n--- прогон %d ---\n%s", i, first, i, out)
		}
	}

	// Пароль пишется ХЕШЕМ (#<sha256>), plaintext в файл НЕ попадает.
	if strings.Contains(first, "zeta-pass") || strings.Contains(first, "alpha-pass") || strings.Contains(first, "mike-pass") {
		t.Fatalf("plaintext-пароль протёк в users.acl:\n%s", first)
	}
	if !strings.Contains(first, "#") {
		t.Fatalf("в users.acl нет хеша пароля (#<sha256>):\n%s", first)
	}
}

// TestRedisConf_ClusterAnnounceIP_PerHost — ПРЯМОЙ regress-guard на tiling-баг
// host-инвариантного cluster-announce-ip (qa 2026-06-22). Корень бага: announce-ip
// протаскивался через apply.input.config (резолвится host-ИНВАРИАНТНО — на первом
// по SID хосте, resolveApplyInput targeted[0]), поэтому ВСЕ ноды анонсировали IP
// первой ноды → cluster-bus за NAT/в облаке сломан.
//
// Фикс: cluster-announce-ip убран из apply.input.config и рендерится в redis.conf.tmpl
// из `{{ .self.network.primary_ip }}` (render_context.self ПЕР-ХОСТ, симметрично bind),
// под гейтом `cluster-enabled`. Тест рендерит РЕАЛЬНЫЙ redis.conf.tmpl с РАЗНЫМ .self
// для двух хостов и доказывает, что каждый получает СВОЙ primary_ip: host A → IP A,
// host B → IP B (а не один на всех). Возврат announce-ip в config-map завалит тест:
// config-map host-инвариантен, оба хоста получили бы один IP.
func TestRedisConf_ClusterAnnounceIP_PerHost(t *testing.T) {
	// HOST-ИНВАРИАНТНЫЙ cluster-config (то, что приходит из apply.input.config:
	// одинаков для всех хостов). announce-ip здесь НЕТ — он per-host из .self.
	clusterConfig := map[string]any{
		"cluster-enabled":      "yes",
		"cluster-config-file":  "nodes.conf",
		"cluster-node-timeout": "5000",
		"maxmemory":            "256mb",
	}

	type hostCase struct {
		sid string
		ip  string
	}
	hosts := []hostCase{
		{sid: "node-a.example.com", ip: "10.0.0.1"},
		{sid: "node-b.example.com", ip: "10.0.0.2"},
	}

	for _, h := range hosts {
		root := map[string]any{
			// render_context.self ПЕР-ХОСТ: при настоящем dispatch каждый хост рендерит
			// .tmpl со своим self; здесь моделируем это, подставляя self хоста h.
			"self": map[string]any{
				"network": map[string]any{"primary_ip": h.ip},
			},
			"vars": map[string]any{
				"password": "s3cr3t-redis-pass",
				"config":   clusterConfig,
				"data_dir": "/var/lib/redis",
				"conf_dir": "/etc/redis",
			},
		}
		out := renderRedisTmpl(t, "redis.conf.tmpl", root)

		wantAnnounce := "cluster-announce-ip " + h.ip
		if !strings.Contains(out, wantAnnounce) {
			t.Fatalf("хост %s: нет своей announce-ip-строки %q:\n%s", h.sid, wantAnnounce, out)
		}
		// Чужой IP в announce-строке этого хоста быть НЕ должен (host-инвариантный
		// баг вернулся бы именно так — фиксированный первый IP у всех).
		for _, other := range hosts {
			if other.ip == h.ip {
				continue
			}
			if strings.Contains(out, "cluster-announce-ip "+other.ip) {
				t.Fatalf("хост %s анонсирует ЧУЖОЙ IP %s — announce-ip host-инвариантен (баг вернулся):\n%s", h.sid, other.ip, out)
			}
		}
		// bind берёт тот же per-host .self.network.primary_ip — симметрия сохранена.
		if !strings.Contains(out, "bind "+h.ip+" 127.0.0.1") {
			t.Fatalf("хост %s: bind не на своём primary_ip %s:\n%s", h.sid, h.ip, out)
		}
	}
}

// TestRedisConf_ClusterAnnounceIP_StandaloneOmitsLine — вне cluster-режима (config
// без cluster-enabled) строки cluster-announce-ip в redis.conf нет: гейт
// `{{ if (index .vars.config "cluster-enabled") }}` гасит её. Доказывает, что
// standalone-рендер не получил cluster-директиву из-за per-host announce-фикса.
func TestRedisConf_ClusterAnnounceIP_StandaloneOmitsLine(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.7"}},
		"vars": map[string]any{
			"password": "s3cr3t-redis-pass",
			"data_dir": "/var/lib/redis",
			"conf_dir": "/etc/redis",
			"config": map[string]any{
				"maxmemory":  "256mb",
				"appendonly": "no",
				"save":       "900 1 300 10 60 10000",
			},
		},
	}
	out := renderRedisTmpl(t, "redis.conf.tmpl", root)
	if strings.Contains(out, "cluster-announce-ip") {
		t.Fatalf("standalone (без cluster-enabled): cluster-announce-ip не должен присутствовать:\n%s", out)
	}
	// bind по-прежнему рендерится из .self (per-host, режим-агностично).
	if !strings.Contains(out, "bind 10.0.0.7 127.0.0.1") {
		t.Fatalf("standalone: bind не на primary_ip:\n%s", out)
	}
}

// TestRedisUsersAcl_Empty — пустой map юзеров → валидный users.acl без строк
// user (default-юзер объявлен в redis.conf отдельно).
func TestRedisUsersAcl_Empty(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{"users": map[string]any{}},
	}
	out := renderRedisTmpl(t, "users.acl.tmpl", root)
	if lines := nonEmptyLines(out); len(lines) != 0 {
		t.Fatalf("пустой users → ожидалось 0 строк user, получено %d:\n%s", len(lines), out)
	}
}

// TestSentinelConf_AnnounceIP_PerHost — ПРЯМОЙ regress-guard на tiling-баг
// host-инвариантного sentinel announce-ip (brief «Sentinel-режим», аналог
// cluster-announce-ip). Корень потенциального бага: если announce-ip протащить
// через apply.input (резолвится host-ИНВАРИАНТНО на первом по SID хосте), ВСЕ
// sentinel-ы анонсировали бы IP первой ноды → gossip за NAT/в облаке сломан.
//
// Фикс: `sentinel announce-ip` рендерится в sentinel.conf.tmpl из
// `{{ .self.network.primary_ip }}` (render_context.self ПЕР-ХОСТ, симметрично
// bind/cluster-announce-ip). Тест рендерит РЕАЛЬНЫЙ sentinel.conf.tmpl с РАЗНЫМ
// .self для двух хостов: каждый обязан анонсировать СВОЙ primary_ip. monitor.ip
// (master), наоборот, HOST-ИНВАРИАНТНЫЙ — один и тот же у обоих.
func TestSentinelConf_AnnounceIP_PerHost(t *testing.T) {
	// HOST-ИНВАРИАНТНЫЕ vars монитора (одинаковы у всех хостов, как из apply.input).
	monitorVars := map[string]any{
		"master_name":     "mymaster",
		"master_ip":       "10.0.0.1", // адрес master (один на кластер)
		"master_port":     "6379",
		"quorum":          "2",
		"auth_user":       "",
		"auth_pass":       "",
		"data_dir":        "/var/lib/redis",
		"conf_dir":        "/etc/redis",
		"sentinel_config": map[string]any{},
	}

	type hostCase struct {
		sid string
		ip  string
	}
	hosts := []hostCase{
		{sid: "node-a.example.com", ip: "10.0.0.5"},
		{sid: "node-b.example.com", ip: "10.0.0.6"},
	}

	for _, h := range hosts {
		vars := map[string]any{}
		for k, v := range monitorVars {
			vars[k] = v
		}
		root := map[string]any{
			"self": map[string]any{"network": map[string]any{"primary_ip": h.ip}},
			"vars": vars,
		}
		out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)

		// announce-ip — СВОЙ primary_ip этого хоста.
		if !strings.Contains(out, "sentinel announce-ip "+h.ip) {
			t.Fatalf("хост %s: нет своей announce-ip-строки %q:\n%s", h.sid, h.ip, out)
		}
		for _, other := range hosts {
			if other.ip != h.ip && strings.Contains(out, "sentinel announce-ip "+other.ip) {
				t.Fatalf("хост %s анонсирует ЧУЖОЙ IP %s — announce-ip host-инвариантен (баг):\n%s", h.sid, other.ip, out)
			}
		}
		// monitor.ip (master) — HOST-ИНВАРИАНТНЫЙ: у обоих хостов один и тот же.
		if !strings.Contains(out, "sentinel monitor mymaster 10.0.0.1 6379 2") {
			t.Fatalf("хост %s: нет sentinel monitor master 10.0.0.1:\n%s", h.sid, out)
		}
	}
}

// TestSentinelConf_DirectivesDeterministicOrder — стартовые директивы
// sentinel_config range-ятся по MAP в ОТСОРТИРОВАННОМ порядке (детерминизм — нет
// ложного change/рестарта), а при пустом auth_pass строки auth-pass нет (gate).
// Прямой guard на детерминизм директив (аналог users.acl order-guard). Маскинг
// здесь НЕ проверяется (это выходной слой Soul/Keeper, не render-фаза) — в файл
// auth-pass пишется как есть (нужно для AUTH sentinel-а на master).
func TestSentinelConf_DirectivesDeterministicOrder(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.5"}},
		"vars": map[string]any{
			"master_name": "mymaster",
			"master_ip":   "10.0.0.1",
			"master_port": "6379",
			"quorum":      "2",
			"auth_user":   "sentinel",
			"auth_pass":   "",
			"data_dir":    "/var/lib/redis",
			"conf_dir":    "/etc/redis",
			// Намеренно не отсортированы: range по MAP обязан отсортировать ключи.
			"sentinel_config": map[string]any{
				"sentinel down-after-milliseconds mymaster": "12000",
				"loglevel":                           "notice",
				"sentinel failover-timeout mymaster": "70000",
			},
		},
	}
	const runs = 12
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("прогон %d дал ИНОЙ вывод (недетерминизм директив):\n--- 0 ---\n%s\n--- %d ---\n%s", i, first, i, out)
		}
	}
	// loglevel < sentinel down... < sentinel failover... — порядок sorted-ключей.
	iLog := strings.Index(first, "loglevel notice")
	iDown := strings.Index(first, "down-after-milliseconds mymaster 12000")
	iFail := strings.Index(first, "failover-timeout mymaster 70000")
	if iLog < 0 || iDown < 0 || iFail < 0 {
		t.Fatalf("нет ожидаемых директив:\n%s", first)
	}
	if !(iLog < iDown && iDown < iFail) {
		t.Fatalf("директивы не в отсортированном порядке (loglevel<down<failover):\n%s", first)
	}
	// auth-pass пуст → строки auth-pass нет.
	if strings.Contains(first, "sentinel auth-pass") {
		t.Fatalf("пустой auth_pass: строки auth-pass быть не должно:\n%s", first)
	}
}

// TestSentinelConf_AuthRendered — ПОЗИТИВНЫЙ guard auth-блока: при НЕпустых
// auth_user/auth_pass обе директивы `sentinel auth-user`/`sentinel auth-pass`
// рендерятся с именем мониторимого master-а (симметрия с пустым случаем в
// DirectivesDeterministicOrder, где обеих строк нет). Ловит регресс гейта
// `{{- if .vars.auth_X }}` и потерю master_name в auth-строках. AUTH sentinel-а
// на master требуется при requirepass — без этих строк failover молча сломается.
func TestSentinelConf_AuthRendered(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.5"}},
		"vars": map[string]any{
			"master_name":     "mymaster",
			"master_ip":       "10.0.0.1",
			"master_port":     "6379",
			"quorum":          "2",
			"auth_user":       "sentinel-user",
			"auth_pass":       "s3cr3t-sentinel-pass",
			"data_dir":        "/var/lib/redis",
			"conf_dir":        "/etc/redis",
			"sentinel_config": map[string]any{},
		},
	}
	out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)

	// auth-user — с именем мониторимого master-а.
	if !strings.Contains(out, "sentinel auth-user mymaster sentinel-user") {
		t.Fatalf("нет строки auth-user с master_name:\n%s", out)
	}
	// auth-pass — с именем мониторимого master-а.
	if !strings.Contains(out, "sentinel auth-pass mymaster s3cr3t-sentinel-pass") {
		t.Fatalf("нет строки auth-pass с master_name:\n%s", out)
	}
}

// sysctl drop-in /etc/sysctl.d/30-redis.conf больше НЕ рендерится шаблоном:
// host-tuning extras перешли на core.sysctl.applied (модуль сам строит
// детерминированный drop-in из map с sorted keys, ADR-015 amend). Шаблон
// redis.sysctl.conf.tmpl удалён, sorted-детерминизм проверяется unit-тестом
// модуля (soul/internal/coremod/sysctl/applied_test.go).

// TestRedisConf_Loadmodule_NoTrailingSpace — ПРЯМОЙ guard на чистоту директив
// loadmodule (Redis-модули, Redis < 8). Корень потенциального бага (brief
// «Модули-нюанс»): если loadmodule класть КЛЮЧОМ config-map, range шаблона
// `{{$key}} {{$value}}` с пустым value печатает `loadmodule /path.so ` — хвостовой
// пробел. Любая нестабильность хвоста (пробел/нет) → ложный change core.file.rendered
// → лишний рестарт Redis.
//
// Фикс: loadmodule вынесен в ОТДЕЛЬНУЮ секцию шаблона из списка .vars.loadmodules
// (`loadmodule {{ . }}` — без хвостового value). Тест рендерит РЕАЛЬНЫЙ
// redis.conf.tmpl и доказывает: (а) строки loadmodule БЕЗ хвостового пробела;
// (б) порядок строк = порядок списка (детерминизм между прогонами); (в) пути
// присутствуют целиком.
func TestRedisConf_Loadmodule_NoTrailingSpace(t *testing.T) {
	loadmodules := []any{
		"/var/lib/redis/modules/redisearch.so",
		"/var/lib/redis/modules/rejson.so",
	}
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}},
		"vars": map[string]any{
			"password":    "s3cr3t-redis-pass",
			"config":      map[string]any{"maxmemory": "256mb"},
			"loadmodules": loadmodules,
			"data_dir":    "/var/lib/redis",
			"conf_dir":    "/etc/redis",
		},
	}

	const runs = 12
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "redis.conf.tmpl", root)
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("прогон %d дал ИНОЙ вывод (недетерминизм loadmodule):\n--- 0 ---\n%s\n--- %d ---\n%s", i, first, i, out)
		}
	}

	var modLines []string
	for _, ln := range strings.Split(first, "\n") {
		if strings.HasPrefix(ln, "loadmodule") {
			modLines = append(modLines, ln)
		}
	}
	if len(modLines) != 2 {
		t.Fatalf("ожидалось 2 строки loadmodule, получено %d:\n%s", len(modLines), first)
	}
	// (а) Хвостового пробела нет — строка заканчивается ровно на .so.
	for _, ln := range modLines {
		if ln != strings.TrimRight(ln, " ") {
			t.Fatalf("строка loadmodule с хвостовым пробелом: %q", ln)
		}
		if !strings.HasSuffix(ln, ".so") {
			t.Fatalf("строка loadmodule не заканчивается на .so: %q", ln)
		}
	}
	// (б) Порядок строк = порядок списка (детерминизм списка, а не итерации map).
	want := []string{
		"loadmodule /var/lib/redis/modules/redisearch.so",
		"loadmodule /var/lib/redis/modules/rejson.so",
	}
	for i := range want {
		if modLines[i] != want[i] {
			t.Fatalf("строка %d = %q, want %q (порядок списка)", i, modLines[i], want[i])
		}
	}
}

// TestRedisConf_Loadmodule_EmptyAndAbsent — без модулей секции loadmodule в
// redis.conf нет в обоих случаях: ключ loadmodules задан пустым списком (Redis 8+:
// scenario передаёт []) И ключ вовсе отсутствует в .vars (`index .vars
// "loadmodules"` на отсутствующем ключе возвращает nil без ошибки в strict-mode,
// симметрично cluster-enabled-гейту). Прямой guard на version-gate-ветку (8+ → нет
// loadmodule) и на back-compat рендер без modules-vars.
func TestRedisConf_Loadmodule_EmptyAndAbsent(t *testing.T) {
	base := func(loadmodules any) map[string]any {
		vars := map[string]any{
			"password": "s3cr3t-redis-pass",
			"config":   map[string]any{"maxmemory": "256mb"},
			"data_dir": "/var/lib/redis",
			"conf_dir": "/etc/redis",
		}
		if loadmodules != nil {
			vars["loadmodules"] = loadmodules
		}
		return map[string]any{
			"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}},
			"vars": vars,
		}
	}

	cases := map[string]any{
		"empty list (Redis 8+ gate)": []any{},
		"absent key (no modules)":    nil,
	}
	for name, lm := range cases {
		out := renderRedisTmpl(t, "redis.conf.tmpl", base(lm))
		if strings.Contains(out, "loadmodule") {
			t.Fatalf("%s: директива loadmodule не должна присутствовать:\n%s", name, out)
		}
	}
}

// nonEmptyLines — непустые строки рендера (отбрасывает blank-строки от
// {{- -}}-обрамления комментария шаблона).
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, strings.TrimSpace(ln))
		}
	}
	return out
}

// userName — имя юзера из строки `user <name> <state> #<hash> <perms>`.
func userName(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "user" {
		return ""
	}
	return fields[1]
}
