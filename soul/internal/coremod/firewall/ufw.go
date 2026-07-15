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

// applyUFW adds/removes a single ufw rule. NEVER calls `ufw enable` and never
// touches `ufw default` — only `ufw allow/deny` and `ufw delete allow/deny`
// for a specific rule (security invariant, see package doc).
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

// ufwAddArgs builds arguments for `ufw allow/deny ...`. ufw format:
//
//	ufw allow proto tcp from 10.0.0.0/8 to any port 5432
//	ufw allow 80/tcp                                    (no source)
//
// We always use the explicit expanded form (proto/from/to any port) so add
// and parse stay symmetric: the short form `80/tcp` shows up differently in
// status than `5432/tcp` with a source.
func ufwAddArgs(r rule) []string {
	if r.source == "" {
		// No source — short form, matching what ufw status prints: "80/tcp".
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

// ufwStatusRule is one parsed line of `ufw status`.
type ufwStatusRule struct {
	port   int
	proto  string
	action string // allow | deny
	source string // "" for Anywhere
}

// ufwRulePresent reports whether rule r is present in `ufw status` output.
func ufwRulePresent(statusOut string, r rule) bool {
	for _, sr := range parseUFWStatus(statusOut) {
		if sr.port == r.port && sr.proto == r.proto && sr.action == r.action && sourceMatch(sr.source, r.source) {
			return true
		}
	}
	return false
}

// sourceMatch: an empty r.source (any) matches only an empty sr.source
// (Anywhere); otherwise exact string match. The desired source is already
// normalized (normalizeSource), and ufw status prints the canonical form,
// so the strings are directly comparable.
func sourceMatch(got, want string) bool {
	return got == want
}

// parseUFWStatus parses the tabular output of `ufw status`. Data line format:
//
//	To                         Action      From
//	5432/tcp                   ALLOW       10.0.0.0/8
//	80/tcp                     ALLOW       Anywhere
//	22                         DENY        Anywhere
//
// Some ufw builds print a direction token in the Action column (`ALLOW IN`,
// `DENY IN`, `ALLOW OUT`) even in non-verbose mode:
//
//	9100/tcp                   ALLOW IN    Anywhere
//
// In that case the direction token between action and source is ignored,
// otherwise `IN` would land in source and break idempotency for no-source rules.
//
// Ignored: the header ("To ... Action ... From"), the separator ("--"), lines
// with "(v6)" (IPv6 mirror), and the "Status: active" line.
//
// Parsing is fragile across ufw versions — covered by unit tests on samples.
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
		// IPv6 mirror rules (e.g. "80/tcp (v6)") aren't matched in MVP —
		// idempotency is tracked against the IPv4 line.
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
		// source is the first field after action; an optional direction token
		// (`IN`/`OUT`) between action and source is skipped.
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

// parseUFWPortProto parses "5432/tcp" → (5432,"tcp"). A bare port "22" with no
// protocol is treated as tcp (ufw's default when proto is absent).
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

// isUFWDirection recognizes the Action column's direction token (`IN`/`OUT`),
// which some ufw builds print after the action (`ALLOW IN`).
func isUFWDirection(s string) bool {
	switch strings.ToUpper(s) {
	case "IN", "OUT":
		return true
	default:
		return false
	}
}
