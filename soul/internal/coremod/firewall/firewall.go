// Package firewall implements the `core.firewall` core module ([ADR-015]) —
// manages a SINGLE firewall rule (an ansible ufw/firewalld-inspired idea,
// reworked into a safe declarative MVP).
//
// States:
//   - present: the rule exists (port/protocol/source/action).
//   - absent:  the rule is removed.
//
// Backend is selected via util.DetectFirewall (by which control binary is
// installed, NOT by Soulprint). MVP: ufw and firewalld. iptables is deferred.
//
// Idempotency: parses `ufw status` / `firewall-cmd --list-...` output. If the
// rule is already present (present) or absent (absent) — changed=false.
//
// CRITICAL SECURITY INVARIANT ([ADR-016] "security first"): the module NEVER
// touches the default policy and NEVER runs `ufw enable` /
// `systemctl start firewalld` / never enables the firewall wholesale.
// Enabling a firewall with a default-deny policy on a remote host instantly
// cuts off SSH and loses control. The module works ONLY with a specific rule
// (add/delete). Enforced by a unit test (Apply must not generate a single
// enable/default command).
//
// CLI output parsing is fragile across tool versions — covered by strict unit
// tests against pinned output samples.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
// [ADR-016]: docs/adr/0016-parity-license.md
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

// Name is the canonical address prefix.
const Name = "core.firewall"

// Module implements sdk/module.SoulModule for core.firewall.
//
// Runner is the seam for substituting exec calls in unit tests (detect +
// status parsing + add/delete). All ufw/firewall-cmd calls go through it.
type Module struct {
	Runner util.Runner
}

func New() *Module {
	return &Module{Runner: util.OSRunner{}}
}

// rule is a normalized firewall rule.
type rule struct {
	port   int
	proto  string // tcp | udp
	source string // CIDR/IP or "" (any)
	action string // allow | deny
	zone   string // firewalld zone or "" (default zone)
}

// Validate is NOT fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): `port` accepts int OR string (${...} interpolation), range
// 1..65535 is checked, proto/action are enums (tcp|udp / allow|deny), source
// is a CIDR/IPv4 form that rejects IPv6. Neither enums, numeric bounds, nor
// dual-type-required are expressible in the manifest DSL — kept as manual
// validation.
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

// PlanReadSafe declares core.firewall.Plan as pure-read (ADR-031 Scry): reads
// `ufw status` / `firewall-cmd --list-...`, does NOT mutate firewall state
// (marker for the host, default-deny).
//
// CRITICAL: this marker is consistent with the module's security invariant —
// Plan doesn't and can't do enable/default/start (see the file's doc
// comment). All of Plan's runner calls are read-only (`ufw status` /
// `firewall-cmd --list-ports` / `firewall-cmd --list-rich-rules`).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads the current firewall
// rule state (the same status/list Apply starts with) and sends
// PlanEvent.changed — "would Apply change the rule?" Does NOT mutate: no
// rule add/delete, no reload.
//
// Backend is selected via util.DetectFirewall (read-only).
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

// planFromPresence maps "is the rule present?" and the desired state to
// drift. present + present-state → clean; absent + absent-state → clean;
// otherwise drift.
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

// readRuleFromPlan mirrors readRule for PlanRequest.
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
		// Normalize to the canonical form (as ufw/firewalld print it) so the
		// written and read rule match → idempotency.
		r.source = normalizeSource(r.source)
	}
	if r.zone, err = util.OptStringParam(req.Params, "zone"); err != nil {
		return r, err
	}
	return r, nil
}

// parsePort accepts port as a number (proto-json marshals numbers as
// float64) or a string (for ${...} interpolation, which yields a string).
// Range 1..65535.
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

// validateSource accepts an IPv4 CIDR (192.168.0.0/24) or a single IPv4
// (10.0.0.1). IPv6 sources are rejected: both MVP backends only work with
// IPv4 (the ufw parser skips v6 strings, firewalld hardcodes family="ipv4"),
// and silently accepting IPv6 would cause a looping add (drift). Honest
// rejection at Validate.
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

// normalizeSource brings source to the canonical form mirroring
// ufw/firewalld's output: a CIDR with host bits collapses to the network
// address (10.0.0.1/8 → 10.0.0.0/8), a single IP gets /32
// (10.0.0.1 → 10.0.0.1/32). Call only after validateSource (input is already
// valid and IPv4).
func normalizeSource(src string) string {
	if _, ipnet, err := net.ParseCIDR(src); err == nil {
		return ipnet.String()
	}
	if ip := net.ParseIP(src); ip != nil {
		return ip.String() + "/32"
	}
	return src
}

// cmdError turns a Result into an error on launch failure or non-zero exit.
// Returns nil if the command exited 0.
func cmdError(name string, args []string, res util.Result) error {
	if res.Err != nil {
		return fmt.Errorf("%s %v: %v", name, args, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s %v: exit %d: %s", name, args, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// finalRule assembles the final ApplyEvent with changed and rule echo fields.
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
