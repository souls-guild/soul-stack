package firewall_test

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/firewall"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func apply(t *testing.T, r *internaltest.Runner, state string, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	m := &firewall.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream
}

// --- Fixed CLI output samples (parsing is fragile across versions) ---

const ufwStatusSample = `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
5432/tcp                   ALLOW       10.0.0.0/8
53                         DENY        Anywhere
443/tcp                    ALLOW       Anywhere (v6)
`

// ufwStatusInSample is a variant of `ufw status` output where the Action
// column carries a direction token (`ALLOW IN` / `DENY IN`) even in
// non-verbose mode (some ufw builds). The parser must ignore the direction,
// or `IN` leaks into source and breaks idempotency of no-source rules.
const ufwStatusInSample = `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
9100/tcp                   ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.0.0.0/8
53                         DENY IN     Anywhere
443/tcp                    ALLOW IN    Anywhere (v6)
`

const firewalldListPortsSample = "22/tcp 80/tcp 443/tcp 5432/tcp\n"

const firewalldRichRulesSample = `rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept
rule family="ipv4" source address="192.168.0.0/16" port port="53" protocol="udp" reject
`

// --- Validate ---

func TestValidate_UnknownState(t *testing.T) {
	reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"port": 22}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для неизвестного state")
	}
}

func TestValidate_PortRequired(t *testing.T) {
	reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true без port")
	}
}

func TestValidate_PortRange(t *testing.T) {
	for _, p := range []any{0, 70000, -1} {
		reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"port": p}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true для порта вне диапазона %v", p)
		}
	}
}

func TestValidate_BadProtoAndAction(t *testing.T) {
	reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"port": 22, "proto": "sctp"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для proto=sctp")
	}
	reply, _ = firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"port": 22, "action": "reject"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для action=reject")
	}
}

func TestValidate_BadSource(t *testing.T) {
	reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"port": 22, "source": "not-a-cidr"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для битого source")
	}
}

func TestValidate_IPv6Source_Rejected(t *testing.T) {
	for _, src := range []string{"2001:db8::1", "2001:db8::/32", "::1"} {
		reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"port": 22, "source": src}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true для IPv6-source %q (ожидался отказ)", src)
		}
	}
}

func TestValidate_Happy(t *testing.T) {
	reply, _ := firewall.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8", "action": "allow"}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для валидного правила: %v", reply.Errors)
	}
}

// --- ufw runner helpers ---

func ufwRunner() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.On("ufw --version", util.Result{ExitCode: 0}) // DetectFirewall → ufw
	r.On("ufw status", util.Result{Stdout: ufwStatusSample})
	return r
}

func firewalldRunner() *internaltest.Runner {
	r := internaltest.NewRunner()
	// DetectFirewall: ufw absent (127), firewall-cmd present.
	r.On("firewall-cmd --version", util.Result{ExitCode: 0})
	r.On("firewall-cmd --list-ports", util.Result{Stdout: firewalldListPortsSample})
	r.On("firewall-cmd --list-rich-rules", util.Result{Stdout: firewalldRichRulesSample})
	return r
}

// --- ufw: idempotency via status parsing ---

func TestUFW_Present_AlreadyPresentSimplePort_NoOp(t *testing.T) {
	r := ufwRunner()
	stream := apply(t, r, "present", map[string]any{"port": 80, "proto": "tcp"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже существующего 80/tcp")
	}
	if calledWith(r, "ufw", "allow") {
		t.Fatalf("ufw allow вызван при идемпотентности: %v", r.Calls)
	}
}

func TestUFW_Present_AlreadyPresentWithSource_NoOp(t *testing.T) {
	r := ufwRunner()
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже существующего 5432/tcp from 10.0.0.0/8")
	}
}

// Source with host bits (10.0.0.1/8) normalizes to the network (10.0.0.0/8)
// as printed by ufw status → the rule is considered present → changed=false.
// Without normalization, string comparison would give present=false and a
// repeated add (drift).
func TestUFW_Present_SourceWithHostBits_Idempotent(t *testing.T) {
	r := ufwRunner()
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.1/8"})
	if stream.Last().Changed {
		t.Fatal("changed=true для 10.0.0.1/8 (нормализуется к 10.0.0.0/8, уже присутствует)")
	}
	if calledWith(r, "ufw", "allow") {
		t.Fatalf("ufw allow вызван при идемпотентности source-нормализации: %v", r.Calls)
	}
}

// TestUFW_Present_AllowInFormat_Idempotent: `ufw status` output with a
// direction token (`ALLOW IN`) for the no-source rule 9100/tcp. The parser
// must ignore `IN`, or it leaks into source, the rule won't match, and every
// run would repeat `ufw allow` (a false changed=true). Expected:
// present=true → changed=false → no add.
func TestUFW_Present_AllowInFormat_Idempotent(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("ufw --version", util.Result{ExitCode: 0})
	r.On("ufw status", util.Result{Stdout: ufwStatusInSample})
	stream := apply(t, r, "present", map[string]any{"port": 9100, "proto": "tcp"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже существующего 9100/tcp в формате 'ALLOW IN'")
	}
	if calledWith(r, "ufw", "allow") {
		t.Fatalf("ufw allow вызван при идемпотентности 'ALLOW IN'-формата: %v", r.Calls)
	}
}

// TestUFW_Present_AllowInFormat_WithSource_Idempotent: same direction format,
// but a rule with source (5432/tcp from 10.0.0.0/8) — `IN` must not shift
// source parsing.
func TestUFW_Present_AllowInFormat_WithSource_Idempotent(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("ufw --version", util.Result{ExitCode: 0})
	r.On("ufw status", util.Result{Stdout: ufwStatusInSample})
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8"})
	if stream.Last().Changed {
		t.Fatal("changed=true для 5432/tcp from 10.0.0.0/8 в формате 'ALLOW IN'")
	}
	if calledWith(r, "ufw", "allow") {
		t.Fatalf("ufw allow вызван при идемпотентности 'ALLOW IN' с source: %v", r.Calls)
	}
}

func TestUFW_Present_NewRule_Adds(t *testing.T) {
	r := ufwRunner()
	r.On("ufw allow 8080/tcp", util.Result{ExitCode: 0})
	stream := apply(t, r, "present", map[string]any{"port": 8080, "proto": "tcp"})
	if !stream.Last().Changed {
		t.Fatal("changed=false при добавлении нового правила")
	}
	if !calledExact(r, "ufw allow 8080/tcp") {
		t.Fatalf("ожидался вызов 'ufw allow 8080/tcp': %v", r.Calls)
	}
}

func TestUFW_Present_DenyAndAllowDistinct(t *testing.T) {
	// 53 in the sample is DENY; requesting allow 53 should be treated as absent.
	r := ufwRunner()
	r.On("ufw allow 53/tcp", util.Result{ExitCode: 0})
	stream := apply(t, r, "present", map[string]any{"port": 53, "proto": "tcp", "action": "allow"})
	if !stream.Last().Changed {
		t.Fatal("allow 53/tcp не должен совпасть с DENY 53 → ожидался changed=true")
	}
}

func TestUFW_Absent_RemovesExisting(t *testing.T) {
	r := ufwRunner()
	r.On("ufw delete allow 80/tcp", util.Result{ExitCode: 0})
	stream := apply(t, r, "absent", map[string]any{"port": 80, "proto": "tcp"})
	if !stream.Last().Changed {
		t.Fatal("changed=false при удалении существующего правила")
	}
	if !calledExact(r, "ufw delete allow 80/tcp") {
		t.Fatalf("ожидался 'ufw delete allow 80/tcp': %v", r.Calls)
	}
}

func TestUFW_Absent_NotPresent_NoOp(t *testing.T) {
	r := ufwRunner()
	stream := apply(t, r, "absent", map[string]any{"port": 9999, "proto": "tcp"})
	if stream.Last().Changed {
		t.Fatal("changed=true при удалении несуществующего правила")
	}
}

// --- firewalld: idempotency ---

func TestFirewalld_Present_SimplePortPresent_NoOp(t *testing.T) {
	r := firewalldRunner()
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже открытого 5432/tcp")
	}
}

func TestFirewalld_Present_RichRulePresent_NoOp(t *testing.T) {
	r := firewalldRunner()
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже существующего rich-rule 5432 from 10.0.0.0/8")
	}
}

// firewalld mirror of the UFW test: 10.0.0.1/8 normalizes to the network
// 10.0.0.0/8, which is already in rich-rules → changed=false, no add.
func TestFirewalld_Present_SourceWithHostBits_Idempotent(t *testing.T) {
	r := firewalldRunner()
	stream := apply(t, r, "present", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.1/8"})
	if stream.Last().Changed {
		t.Fatal("changed=true для 10.0.0.1/8 (нормализуется к 10.0.0.0/8, rich-rule уже есть)")
	}
	if calledWith(r, "firewall-cmd", "--add-rich-rule") {
		t.Fatalf("--add-rich-rule вызван при идемпотентности source-нормализации: %v", r.Calls)
	}
}

func TestFirewalld_Present_DenyRichRulePresent_NoOp(t *testing.T) {
	r := firewalldRunner()
	stream := apply(t, r, "present", map[string]any{"port": 53, "proto": "udp", "source": "192.168.0.0/16", "action": "deny"})
	if stream.Last().Changed {
		t.Fatal("changed=true для существующего reject rich-rule")
	}
}

func TestFirewalld_Present_NewSimplePort_Adds(t *testing.T) {
	r := firewalldRunner()
	r.On("firewall-cmd --permanent --add-port=8080/tcp", util.Result{ExitCode: 0})
	r.On("firewall-cmd --reload", util.Result{ExitCode: 0})
	stream := apply(t, r, "present", map[string]any{"port": 8080, "proto": "tcp"})
	if !stream.Last().Changed {
		t.Fatal("changed=false при добавлении нового порта")
	}
	if !calledExact(r, "firewall-cmd --permanent --add-port=8080/tcp") {
		t.Fatalf("ожидался --permanent --add-port=8080/tcp: %v", r.Calls)
	}
	if !calledExact(r, "firewall-cmd --reload") {
		t.Fatalf("ожидался --reload: %v", r.Calls)
	}
}

func TestFirewalld_Absent_RemovesPort(t *testing.T) {
	r := firewalldRunner()
	r.On("firewall-cmd --permanent --remove-port=80/tcp", util.Result{ExitCode: 0})
	r.On("firewall-cmd --reload", util.Result{ExitCode: 0})
	stream := apply(t, r, "absent", map[string]any{"port": 80, "proto": "tcp"})
	if !stream.Last().Changed {
		t.Fatal("changed=false при удалении существующего порта")
	}
	if !calledExact(r, "firewall-cmd --permanent --remove-port=80/tcp") {
		t.Fatalf("ожидался --remove-port=80/tcp: %v", r.Calls)
	}
}

func TestFirewalld_Zone_PassedThrough(t *testing.T) {
	r := firewalldRunner()
	r.On("firewall-cmd --list-ports --zone public", util.Result{Stdout: ""})
	r.On("firewall-cmd --permanent --zone public --add-port=8080/tcp", util.Result{ExitCode: 0})
	r.On("firewall-cmd --reload", util.Result{ExitCode: 0})
	stream := apply(t, r, "present", map[string]any{"port": 8080, "proto": "tcp", "zone": "public"})
	if !stream.Last().Changed {
		t.Fatal("changed=false при добавлении в зону public")
	}
	if !calledExact(r, "firewall-cmd --permanent --zone public --add-port=8080/tcp") {
		t.Fatalf("zone не проброшен в mutate: %v", r.Calls)
	}
}

// TestFirewalld_DenyWithoutSource_RichRule: action=deny without source →
// goes through a rich-rule with reject (plain --add-port is always accept).
// Covers add → idempotent → delete.
func TestFirewalld_DenyWithoutSource_RichRule(t *testing.T) {
	denyParams := map[string]any{"port": 7000, "proto": "tcp", "action": "deny"}
	wantRich := `rule family="ipv4" port port="7000" protocol="tcp" reject`

	// add: the target rich-rule isn't in the output yet → --add-rich-rule + --reload.
	add := firewalldRunner()
	add.On("firewall-cmd --permanent --add-rich-rule="+wantRich, util.Result{ExitCode: 0})
	add.On("firewall-cmd --reload", util.Result{ExitCode: 0})
	stream := apply(t, add, "present", denyParams)
	if !stream.Last().Changed {
		t.Fatal("changed=false при добавлении deny rich-rule")
	}
	if !calledExact(add, "firewall-cmd --permanent --add-rich-rule="+wantRich) {
		t.Fatalf("ожидался --add-rich-rule reject: %v", add.Calls)
	}
	if calledWith(add, "--add-port") {
		t.Fatalf("deny не должен идти через --add-port: %v", add.Calls)
	}

	// idempotent: the same reject is already in --list-rich-rules → changed=false, no add.
	idem := firewalldRunner()
	idem.On("firewall-cmd --list-rich-rules", util.Result{Stdout: wantRich + "\n"})
	stream = apply(t, idem, "present", denyParams)
	if stream.Last().Changed {
		t.Fatal("changed=true для уже существующего deny rich-rule")
	}
	if calledWith(idem, "--add-rich-rule") {
		t.Fatalf("--add-rich-rule вызван при идемпотентности deny: %v", idem.Calls)
	}

	// delete: rich-rule is present → --remove-rich-rule + --reload.
	del := firewalldRunner()
	del.On("firewall-cmd --list-rich-rules", util.Result{Stdout: wantRich + "\n"})
	del.On("firewall-cmd --permanent --remove-rich-rule="+wantRich, util.Result{ExitCode: 0})
	del.On("firewall-cmd --reload", util.Result{ExitCode: 0})
	stream = apply(t, del, "absent", denyParams)
	if !stream.Last().Changed {
		t.Fatal("changed=false при удалении существующего deny rich-rule")
	}
	if !calledExact(del, "firewall-cmd --permanent --remove-rich-rule="+wantRich) {
		t.Fatalf("ожидался --remove-rich-rule reject: %v", del.Calls)
	}
}

// TestFirewalld_Zone_Idempotent: the rule is already open in the given zone
// (the zone is also passed to --list-ports) → changed=false, no --add-port.
func TestFirewalld_Zone_Idempotent(t *testing.T) {
	r := firewalldRunner()
	r.On("firewall-cmd --list-ports --zone public", util.Result{Stdout: "8080/tcp\n"})
	stream := apply(t, r, "present", map[string]any{"port": 8080, "proto": "tcp", "zone": "public"})
	if stream.Last().Changed {
		t.Fatal("changed=true для уже открытого 8080/tcp в зоне public")
	}
	if calledWith(r, "--add-port") {
		t.Fatalf("--add-port вызван при идемпотентности зоны: %v", r.Calls)
	}
}

// TestUFW_UDPDistinctFromTCP: allow 53/udp isn't confused with bare 53 in
// status (parsed as 53/tcp DENY) → the rule is treated as absent, ufw allow
// 53/udp gets added.
func TestUFW_UDPDistinctFromTCP(t *testing.T) {
	r := ufwRunner()
	r.On("ufw allow 53/udp", util.Result{ExitCode: 0})
	stream := apply(t, r, "present", map[string]any{"port": 53, "proto": "udp", "action": "allow"})
	if !stream.Last().Changed {
		t.Fatal("allow 53/udp не должен совпасть с 53/tcp DENY → ожидался changed=true")
	}
	if !calledExact(r, "ufw allow 53/udp") {
		t.Fatalf("ожидался 'ufw allow 53/udp': %v", r.Calls)
	}
}

// --- backend not detected ---

func TestApply_NoFirewall_Fails(t *testing.T) {
	r := internaltest.NewRunner() // everything → 127
	stream := apply(t, r, "present", map[string]any{"port": 22})
	if !stream.Last().Failed {
		t.Fatal("без firewall-инструмента Apply должен зафейлиться")
	}
}

// --- CRITICAL SECURITY INVARIANT ---
// Apply must NEVER generate commands that enable the firewall wholesale or
// change the default policy: ufw enable / ufw default / firewall-cmd
// --set-default-zone / systemctl start firewalld. Otherwise SSH gets cut off
// on a remote deny-by-default host.

func TestUFW_NeverGeneratesEnableOrDefault(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		params map[string]any
	}{
		{"add", "present", map[string]any{"port": 8080, "proto": "tcp"}},
		{"add-source", "present", map[string]any{"port": 9090, "proto": "tcp", "source": "10.0.0.0/8"}},
		{"delete", "absent", map[string]any{"port": 80, "proto": "tcp"}},
		{"delete-source", "absent", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8"}},
		{"deny", "present", map[string]any{"port": 7000, "action": "deny"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ufwRunner()
			r.Fallback = util.Result{ExitCode: 0} // any add/delete "succeeds"
			r.On("ufw status", util.Result{Stdout: ufwStatusSample})
			apply(t, r, c.state, c.params)
			assertNoDangerousFirewallCmd(t, r)
		})
	}
}

func TestFirewalld_NeverGeneratesEnableOrDefault(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		params map[string]any
	}{
		{"add", "present", map[string]any{"port": 8080, "proto": "tcp"}},
		{"add-source", "present", map[string]any{"port": 9090, "proto": "tcp", "source": "10.0.0.0/8"}},
		{"delete-port", "absent", map[string]any{"port": 80, "proto": "tcp"}},
		{"deny", "present", map[string]any{"port": 7000, "action": "deny"}},
		{"delete-source", "absent", map[string]any{"port": 5432, "proto": "tcp", "source": "10.0.0.0/8"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := firewalldRunner()
			r.Fallback = util.Result{ExitCode: 0}
			r.On("firewall-cmd --list-ports", util.Result{Stdout: firewalldListPortsSample})
			r.On("firewall-cmd --list-rich-rules", util.Result{Stdout: firewalldRichRulesSample})
			apply(t, r, c.state, c.params)
			assertNoDangerousFirewallCmd(t, r)
		})
	}
}

// assertNoDangerousFirewallCmd checks that none of the called commands
// enable the firewall wholesale or change the default policy.
func assertNoDangerousFirewallCmd(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	banned := []string{
		"ufw enable",
		"ufw default",
		"ufw reset",
		"ufw disable",
		"--set-default-zone",
		"--set-target",
		"systemctl start firewalld",
		"systemctl enable firewalld",
		"--panic-on",
	}
	for _, call := range r.Calls {
		for _, b := range banned {
			if strings.Contains(call, b) {
				t.Fatalf("Apply сгенерировал запрещённую команду %q (содержит %q)", call, b)
			}
		}
	}
}

func calledExact(r *internaltest.Runner, want string) bool {
	for _, c := range r.Calls {
		if c == want {
			return true
		}
	}
	return false
}

func calledWith(r *internaltest.Runner, parts ...string) bool {
	for _, c := range r.Calls {
		all := true
		for _, p := range parts {
			if !strings.Contains(c, p) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
