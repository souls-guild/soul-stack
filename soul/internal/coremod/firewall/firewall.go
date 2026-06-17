// Package firewall реализует core-модуль `core.firewall` ([ADR-015]) —
// управление ОДНИМ правилом файрвола (аналог идеи ansible ufw/firewalld,
// переработанный под безопасный декларативный MVP).
//
// Состояния:
//   - present: правило существует (порт/протокол/источник/действие).
//   - absent:  правило удалено.
//
// Backend выбирается по util.DetectFirewall (по установленному управляющему
// бинарю, НЕ по Soulprint). MVP: ufw и firewalld. iptables отложен.
//
// Идемпотентность: парсим вывод `ufw status` / `firewall-cmd --list-...`. Если
// правило уже присутствует (present) или отсутствует (absent) — changed=false.
//
// КРИТИЧЕСКИЙ ИНВАРИАНТ БЕЗОПАСНОСТИ ([ADR-016] «безопасность на первом
// месте»): модуль НИКОГДА не трогает default policy и НИКОГДА не делает
// `ufw enable` / `systemctl start firewalld` / не включает файрвол целиком.
// Включение файрвола с дефолтной deny-политикой на удалённом хосте мгновенно
// отрезает SSH и теряет управление. Модуль работает ТОЛЬКО с конкретным
// правилом (add/delete). Это проверяется unit-тестом (Apply не должен
// генерировать ни одной enable/default-команды).
//
// Парсинг вывода CLI хрупок между версиями инструментов — покрыт строгими
// unit-тестами на зафиксированных образцах вывода.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
// [ADR-016]: docs/adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack
package firewall

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — каноническая верхушка адреса.
const Name = "core.firewall"

// Module — реализация sdk/module.SoulModule для core.firewall.
//
// Runner — точка подмены exec-вызовов в unit-тестах (детект + status-парсинг +
// add/delete). Все обращения к ufw/firewall-cmd идут через него.
type Module struct {
	Runner util.Runner
}

func New() *Module {
	return &Module{Runner: util.OSRunner{}}
}

// rule — нормализованное правило файрвола.
type rule struct {
	port   int
	proto  string // tcp | udp
	source string // CIDR/IP или "" (any)
	action string // allow | deny
	zone   string // firewalld-зона или "" (default zone)
}

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): `port` принимается как int ИЛИ string (${...}-интерполяция),
// проверяется диапазон 1..65535, proto/action — enum (tcp|udp / allow|deny),
// source — CIDR/IPv4-форма с отказом на IPv6. Ни enum, ни числовые границы, ни
// dual-type-required не выражаются manifest-DSL — оставляем ручную форму.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "present", "absent":
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want present|absent)", req.State))
	}

	if _, err := parsePort(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := parseProto(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := parseAction(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	if src, err := util.OptStringParam(req.Params, "source"); err != nil {
		errs = append(errs, err.Error())
	} else if src != "" {
		if verr := validateSource(src); verr != nil {
			errs = append(errs, verr.Error())
		}
	}
	if _, err := util.OptStringParam(req.Params, "zone"); err != nil {
		errs = append(errs, err.Error())
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.firewall.Plan — pure-read (ADR-031 Scry):
// читает `ufw status` / `firewall-cmd --list-...`, НЕ мутирует state файрвола
// (маркер для host-а, default-deny).
//
// КРИТИЧНО: маркер совместим с инвариантом безопасности модуля — Plan не делает
// и не может делать enable/default/start (см. doc файла). Все runner-вызовы
// Plan-а — read-only (`ufw status` / `firewall-cmd --list-ports` /
// `firewall-cmd --list-rich-rules`).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее состояние правил
// файрвола (тот же status/list, что в начале Apply) и шлёт PlanEvent.changed —
// «Apply изменил бы правило?». НЕ мутирует: ни add/delete правил, ни reload.
//
// Backend выбирается через util.DetectFirewall (read-only).
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	r, err := readRuleFromPlan(req)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	fw := util.DetectFirewall(ctx, m.Runner)
	if fw == util.FirewallUnknown {
		return util.PlanFailed("core.firewall: no supported firewall detected (ufw/firewalld)")
	}

	switch fw {
	case util.FirewallUFW:
		status := m.Runner.Run(ctx, "ufw", "status")
		if status.Err != nil {
			return util.PlanFailed(fmt.Sprintf("ufw status: %v", status.Err))
		}
		if status.ExitCode != 0 {
			return util.PlanFailed(fmt.Sprintf("ufw status: exit %d: %s", status.ExitCode, strings.TrimSpace(status.Stderr)))
		}
		present := ufwRulePresent(status.Stdout, r)
		return planFromPresence(stream, req.State, present)
	case util.FirewallFirewalld:
		zoneArgs := zoneArgs(r.zone)
		present, perr := m.firewalldRulePresent(ctx, r, zoneArgs)
		if perr != nil {
			return util.PlanFailed(perr.Error())
		}
		return planFromPresence(stream, req.State, present)
	default:
		return util.PlanFailed(fmt.Sprintf("core.firewall: unsupported firewall %q", fw))
	}
}

// planFromPresence маппит «правило присутствует?» и желаемый state в drift.
// present + present-state → clean; absent + absent-state → clean; иначе drift.
func planFromPresence(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], state string, present bool) error {
	switch state {
	case "present":
		return util.SendPlanFinal(stream, !present)
	case "absent":
		return util.SendPlanFinal(stream, present)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", state))
	}
}

// readRuleFromPlan — параллель readRule для PlanRequest.
func readRuleFromPlan(req *pluginv1.PlanRequest) (rule, error) {
	var r rule
	var err error
	if r.port, err = parsePort(req.Params); err != nil {
		return r, err
	}
	if r.proto, err = parseProto(req.Params); err != nil {
		return r, err
	}
	if r.action, err = parseAction(req.Params); err != nil {
		return r, err
	}
	if r.source, err = util.OptStringParam(req.Params, "source"); err != nil {
		return r, err
	}
	if r.source != "" {
		if verr := validateSource(r.source); verr != nil {
			return r, verr
		}
		r.source = normalizeSource(r.source)
	}
	if r.zone, err = util.OptStringParam(req.Params, "zone"); err != nil {
		return r, err
	}
	return r, nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	r, err := readRule(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	fw := util.DetectFirewall(ctx, m.Runner)
	if fw == util.FirewallUnknown {
		return util.SendFailed(stream, "core.firewall: no supported firewall detected (ufw/firewalld)")
	}

	switch fw {
	case util.FirewallUFW:
		return m.applyUFW(ctx, stream, req.State, r)
	case util.FirewallFirewalld:
		return m.applyFirewalld(ctx, stream, req.State, r)
	default:
		return util.SendFailed(stream, fmt.Sprintf("core.firewall: unsupported firewall %q", fw))
	}
}

func readRule(req *pluginv1.ApplyRequest) (rule, error) {
	var r rule
	var err error
	if r.port, err = parsePort(req.Params); err != nil {
		return r, err
	}
	if r.proto, err = parseProto(req.Params); err != nil {
		return r, err
	}
	if r.action, err = parseAction(req.Params); err != nil {
		return r, err
	}
	if r.source, err = util.OptStringParam(req.Params, "source"); err != nil {
		return r, err
	}
	if r.source != "" {
		if verr := validateSource(r.source); verr != nil {
			return r, verr
		}
		// Нормализуем к канонической форме (как её печатают ufw/firewalld), чтобы
		// записываемое и читаемое правило совпадали → идемпотентность.
		r.source = normalizeSource(r.source)
	}
	if r.zone, err = util.OptStringParam(req.Params, "zone"); err != nil {
		return r, err
	}
	return r, nil
}

// parsePort принимает port как число (proto-json маршалит числа во float64) или
// строку (на случай ${...}-интерполяции, дающей строку). Диапазон 1..65535.
func parsePort(params *structpb.Struct) (int, error) {
	if n, ok, err := util.OptIntParam(params, "port"); err == nil && ok {
		if n < 1 || n > 65535 {
			return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
		}
		return int(n), nil
	}
	s, serr := util.OptStringParam(params, "port")
	if serr != nil {
		return 0, fmt.Errorf("param %q: must be an integer", "port")
	}
	if s == "" {
		return 0, fmt.Errorf("param %q: required", "port")
	}
	n, cerr := strconv.Atoi(s)
	if cerr != nil {
		return 0, fmt.Errorf("param %q: invalid integer %q", "port", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
	}
	return n, nil
}

func parseProto(params *structpb.Struct) (string, error) {
	p, err := util.OptStringParam(params, "proto")
	if err != nil {
		return "", err
	}
	if p == "" {
		return "tcp", nil
	}
	switch p {
	case "tcp", "udp":
		return p, nil
	default:
		return "", fmt.Errorf("param %q: must be tcp|udp, got %q", "proto", p)
	}
}

func parseAction(params *structpb.Struct) (string, error) {
	a, err := util.OptStringParam(params, "action")
	if err != nil {
		return "", err
	}
	if a == "" {
		return "allow", nil
	}
	switch a {
	case "allow", "deny":
		return a, nil
	default:
		return "", fmt.Errorf("param %q: must be allow|deny, got %q", "action", a)
	}
}

// validateSource принимает IPv4 CIDR (192.168.0.0/24) или одиночный IPv4
// (10.0.0.1). IPv6-source отвергается: оба бэкенда MVP работают только с IPv4
// (ufw-парсер пропускает v6-строки, firewalld жёстко family="ipv4"), и тихий
// приём IPv6 приводит к зацикленному add (drift). Честный отказ на Validate.
func validateSource(src string) error {
	if ip, _, err := net.ParseCIDR(src); err == nil {
		if ip.To4() == nil {
			return fmt.Errorf("param %q: IPv6 source not supported in MVP (got %q)", "source", src)
		}
		return nil
	}
	if ip := net.ParseIP(src); ip != nil {
		if ip.To4() == nil {
			return fmt.Errorf("param %q: IPv6 source not supported in MVP (got %q)", "source", src)
		}
		return nil
	}
	return fmt.Errorf("param %q: invalid CIDR or IP %q", "source", src)
}

// normalizeSource приводит source к канонической форме, симметричной выводу
// ufw/firewalld: CIDR с host-битами схлопывается к адресу сети
// (10.0.0.1/8 → 10.0.0.0/8), одиночный IP получает /32 (10.0.0.1 → 10.0.0.1/32).
// Вызывать только после validateSource (вход уже валиден и IPv4).
func normalizeSource(src string) string {
	if _, ipnet, err := net.ParseCIDR(src); err == nil {
		return ipnet.String()
	}
	if ip := net.ParseIP(src); ip != nil {
		return ip.String() + "/32"
	}
	return src
}

// cmdError превращает Result в ошибку при сбое запуска или non-zero exit.
// Возвращает nil, если команда отработала с exit 0.
func cmdError(name string, args []string, res util.Result) error {
	if res.Err != nil {
		return fmt.Errorf("%s %v: %v", name, args, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s %v: exit %d: %s", name, args, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// finalRule собирает финальный ApplyEvent с changed и эхо-полями правила.
func finalRule(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, backend string, r rule) error {
	out := map[string]any{
		"changed": changed,
		"backend": backend,
		"port":    r.port,
		"proto":   r.proto,
		"action":  r.action,
	}
	if r.source != "" {
		out["source"] = r.source
	}
	if r.zone != "" {
		out["zone"] = r.zone
	}
	return util.SendFinal(stream, changed, out)
}
