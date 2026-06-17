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

// applyUFW добавляет/удаляет одно ufw-правило. НИКОГДА не вызывает `ufw enable`
// и не трогает `ufw default` — только `ufw allow/deny` и `ufw delete allow/deny`
// для конкретного правила (инвариант безопасности, см. doc пакета).
func (m *Module) applyUFW(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], state string, r rule) error {
	status := m.Runner.Run(ctx, "ufw", "status")
	if status.Err != nil {
		return util.SendFailed(stream, fmt.Sprintf("ufw status: %v", status.Err))
	}
	if status.ExitCode != 0 {
		return util.SendFailed(stream, fmt.Sprintf("ufw status: exit %d: %s", status.ExitCode, strings.TrimSpace(status.Stderr)))
	}
	present := ufwRulePresent(status.Stdout, r)

	switch state {
	case "present":
		if present {
			return finalRule(stream, false, "ufw", r)
		}
		args := ufwAddArgs(r)
		res := m.Runner.Run(ctx, "ufw", args...)
		if rerr := cmdError("ufw", args, res); rerr != nil {
			return util.SendFailed(stream, rerr.Error())
		}
		return finalRule(stream, true, "ufw", r)
	case "absent":
		if !present {
			return finalRule(stream, false, "ufw", r)
		}
		args := append([]string{"delete"}, ufwAddArgs(r)...)
		res := m.Runner.Run(ctx, "ufw", args...)
		if rerr := cmdError("ufw", args, res); rerr != nil {
			return util.SendFailed(stream, rerr.Error())
		}
		return finalRule(stream, true, "ufw", r)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", state))
	}
}

// ufwAddArgs строит аргументы для `ufw allow/deny ...`. ufw-формат:
//
//	ufw allow proto tcp from 10.0.0.0/8 to any port 5432
//	ufw allow 80/tcp                                    (без source)
//
// Мы всегда используем явную развёрнутую форму (proto/from/to any port), чтобы
// add и parse были симметричны: краткая форма `80/tcp` в status показывается
// иначе, чем `5432/tcp` с source.
func ufwAddArgs(r rule) []string {
	if r.source == "" {
		// Без source — краткая форма, как её и печатает ufw status: "80/tcp".
		return []string{r.action, fmt.Sprintf("%d/%s", r.port, r.proto)}
	}
	return []string{
		r.action,
		"proto", r.proto,
		"from", r.source,
		"to", "any",
		"port", strconv.Itoa(r.port),
	}
}

// ufwStatusRule — распарсенная строка `ufw status`.
type ufwStatusRule struct {
	port   int
	proto  string
	action string // allow | deny
	source string // "" для Anywhere
}

// ufwRulePresent сообщает, присутствует ли правило r в выводе `ufw status`.
func ufwRulePresent(statusOut string, r rule) bool {
	for _, sr := range parseUFWStatus(statusOut) {
		if sr.port == r.port && sr.proto == r.proto && sr.action == r.action && sourceMatch(sr.source, r.source) {
			return true
		}
	}
	return false
}

// sourceMatch: пустой r.source (any) совпадает только с пустым sr.source
// (Anywhere); иначе — точное строковое совпадение. Желаемый source уже
// нормализован (normalizeSource), а ufw status печатает каноническую форму,
// поэтому строки сравнимы напрямую.
func sourceMatch(got, want string) bool {
	return got == want
}

// parseUFWStatus разбирает табличный вывод `ufw status`. Формат строки данных:
//
//	To                         Action      From
//	5432/tcp                   ALLOW       10.0.0.0/8
//	80/tcp                     ALLOW       Anywhere
//	22                         DENY        Anywhere
//
// Некоторые сборки ufw печатают в колонке Action направление-токен (`ALLOW IN`,
// `DENY IN`, `ALLOW OUT`) даже в non-verbose-режиме:
//
//	9100/tcp                   ALLOW IN    Anywhere
//
// В этом случае направление между action и source игнорируется, иначе `IN`
// попало бы в source и сломало бы идемпотентность no-source-правил.
//
// Игнорируются: заголовок ("To ... Action ... From"), разделитель ("--"),
// строки про "(v6)" (IPv6-зеркало), служебная строка "Status: active".
//
// Парсинг хрупок между версиями ufw — покрыт unit-тестами на образцах.
func parseUFWStatus(out string) []ufwStatusRule {
	var rules []ufwStatusRule
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Status:") {
			continue
		}
		if strings.HasPrefix(line, "To") && strings.Contains(line, "Action") {
			continue
		}
		if strings.HasPrefix(line, "--") {
			continue
		}
		// IPv6-зеркало правила (например "80/tcp (v6)") в MVP не сопоставляем —
		// идемпотентность ведём по IPv4-строке.
		if strings.Contains(line, "(v6)") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		action := actionFromUFW(fields[1])
		if action == "" {
			continue
		}
		port, proto, ok := parseUFWPortProto(fields[0])
		if !ok {
			continue
		}
		// source — первое поле после action; опциональный direction-токен
		// (`IN`/`OUT`) между action и source пропускается.
		fromIdx := 2
		if len(fields) > fromIdx && isUFWDirection(fields[fromIdx]) {
			fromIdx++
		}
		source := ""
		if len(fields) > fromIdx {
			from := fields[fromIdx]
			if from != "Anywhere" {
				source = from
			}
		}
		rules = append(rules, ufwStatusRule{port: port, proto: proto, action: action, source: source})
	}
	return rules
}

// parseUFWPortProto разбирает "5432/tcp" → (5432,"tcp"). Голый порт "22" без
// протокола трактуется как tcp (ufw default при отсутствии proto).
func parseUFWPortProto(s string) (int, string, bool) {
	proto := "tcp"
	portStr := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		portStr = s[:i]
		proto = s[i+1:]
	}
	if proto != "tcp" && proto != "udp" {
		return 0, "", false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, "", false
	}
	return port, proto, true
}

func actionFromUFW(s string) string {
	switch strings.ToUpper(s) {
	case "ALLOW":
		return "allow"
	case "DENY":
		return "deny"
	default:
		return ""
	}
}

// isUFWDirection распознаёт direction-токен колонки Action (`IN`/`OUT`),
// который некоторые сборки ufw печатают после действия (`ALLOW IN`).
func isUFWDirection(s string) bool {
	switch strings.ToUpper(s) {
	case "IN", "OUT":
		return true
	default:
		return false
	}
}
