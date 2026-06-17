package firewall

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyFirewalld добавляет/удаляет одно правило через firewall-cmd. НИКОГДА не
// запускает службу firewalld и не меняет default/target зоны — только
// add/remove конкретного правила, причём с --permanent + явным reload
// (правило должно пережить рестарт; reload применяет permanent в runtime, НЕ
// перезапускает службу и НЕ трогает default policy).
//
// Источник (source) реализуется через rich-rule (firewalld не позволяет
// привязать source к простому --add-port). Без source — простой --add-port.
// deny-правила требуют rich-rule с reject/drop (простой port — это всегда
// accept), поэтому action=deny → rich-rule.
func (m *Module) applyFirewalld(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], state string, r rule) error {
	zoneArgs := zoneArgs(r.zone)

	present, perr := m.firewalldRulePresent(ctx, r, zoneArgs)
	if perr != nil {
		return util.SendFailed(stream, perr.Error())
	}

	switch state {
	case "present":
		if present {
			return finalRule(stream, false, "firewalld", r)
		}
		if err := m.firewalldMutate(ctx, "--add", r, zoneArgs); err != nil {
			return util.SendFailed(stream, err.Error())
		}
		return finalRule(stream, true, "firewalld", r)
	case "absent":
		if !present {
			return finalRule(stream, false, "firewalld", r)
		}
		if err := m.firewalldMutate(ctx, "--remove", r, zoneArgs); err != nil {
			return util.SendFailed(stream, err.Error())
		}
		return finalRule(stream, true, "firewalld", r)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", state))
	}
}

func zoneArgs(zone string) []string {
	if zone == "" {
		return nil
	}
	return []string{"--zone", zone}
}

// firewalldRulePresent проверяет наличие правила. Простое allow-правило без
// source ищем в `--list-ports`; всё остальное (source или deny) — в
// `--list-rich-rules`.
func (m *Module) firewalldRulePresent(ctx context.Context, r rule, zoneArgs []string) (bool, error) {
	if r.source == "" && r.action == "allow" {
		args := append([]string{"--list-ports"}, zoneArgs...)
		res := m.Runner.Run(ctx, "firewall-cmd", args...)
		if rerr := cmdError("firewall-cmd", args, res); rerr != nil {
			return false, rerr
		}
		return firewalldPortPresent(res.Stdout, r.port, r.proto), nil
	}

	args := append([]string{"--list-rich-rules"}, zoneArgs...)
	res := m.Runner.Run(ctx, "firewall-cmd", args...)
	if rerr := cmdError("firewall-cmd", args, res); rerr != nil {
		return false, rerr
	}
	return firewalldRichRulePresent(res.Stdout, r), nil
}

// firewalldMutate выполняет --permanent add/remove + reload. op = "--add" |
// "--remove". reload применяет permanent-конфиг в runtime БЕЗ рестарта службы
// и БЕЗ изменения default-policy.
func (m *Module) firewalldMutate(ctx context.Context, op string, r rule, zoneArgs []string) error {
	var spec []string
	if r.source == "" && r.action == "allow" {
		spec = []string{op + "-port=" + fmt.Sprintf("%d/%s", r.port, r.proto)}
	} else {
		spec = []string{op + "-rich-rule=" + richRule(r)}
	}
	args := append([]string{"--permanent"}, zoneArgs...)
	args = append(args, spec...)
	res := m.Runner.Run(ctx, "firewall-cmd", args...)
	if rerr := cmdError("firewall-cmd", args, res); rerr != nil {
		return rerr
	}
	reload := m.Runner.Run(ctx, "firewall-cmd", "--reload")
	return cmdError("firewall-cmd", []string{"--reload"}, reload)
}

// richRule собирает firewalld rich-rule. Источник через `source address=`,
// действие через accept/reject. IPv4 предполагается (family ipv4); для MVP
// этого достаточно.
//
//	rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept
func richRule(r rule) string {
	verb := "accept"
	if r.action == "deny" {
		verb = "reject"
	}
	var b strings.Builder
	b.WriteString(`rule family="ipv4"`)
	if r.source != "" {
		fmt.Fprintf(&b, ` source address="%s"`, r.source)
	}
	fmt.Fprintf(&b, ` port port="%d" protocol="%s" %s`, r.port, r.proto, verb)
	return b.String()
}

// firewalldPortPresent ищет "5432/tcp" в выводе `firewall-cmd --list-ports`
// (одна строка, токены через пробел):
//
//	22/tcp 80/tcp 443/tcp 5432/tcp
func firewalldPortPresent(out string, port int, proto string) bool {
	want := fmt.Sprintf("%d/%s", port, proto)
	for _, tok := range strings.Fields(out) {
		if tok == want {
			return true
		}
	}
	return false
}

// firewalldRichRulePresent ищет совпадающее rich-rule в выводе
// `firewall-cmd --list-rich-rules`. Сопоставление по нормализованным
// компонентам (port/protocol/source/verb), а не по точной строке: firewalld
// может переупорядочить/переформатировать атрибуты.
//
//	rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept
func firewalldRichRulePresent(out string, r rule) bool {
	wantVerb := "accept"
	if r.action == "deny" {
		wantVerb = "reject"
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		got, ok := parseRichRule(line)
		if !ok {
			continue
		}
		if got.port == r.port && got.proto == r.proto && got.source == r.source && got.verb == wantVerb {
			return true
		}
	}
	return false
}

type richRuleParsed struct {
	port   int
	proto  string
	source string
	verb   string // accept | reject | drop
}

// parseRichRule извлекает из строки rich-rule компоненты port/protocol/source/
// verb через поиск ключевых токенов. Толерантен к порядку и лишним атрибутам.
func parseRichRule(line string) (richRuleParsed, bool) {
	var p richRuleParsed
	p.source = extractQuoted(line, "address=")
	portStr := extractQuoted(line, "port port=")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return p, false
	}
	p.port = port
	p.proto = extractQuoted(line, "protocol=")
	if p.proto != "tcp" && p.proto != "udp" {
		return p, false
	}
	switch {
	case strings.Contains(line, " accept"):
		p.verb = "accept"
	case strings.Contains(line, " reject"):
		p.verb = "reject"
	case strings.Contains(line, " drop"):
		p.verb = "drop"
	default:
		return p, false
	}
	return p, true
}

// extractQuoted находит key (например `address=`) и возвращает значение в
// кавычках сразу после него. Пусто, если ключ не найден.
func extractQuoted(line, key string) string {
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key):]
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
