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
