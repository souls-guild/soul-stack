package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// The registered transports — the closed enumeration `transport:` may name
// (orchestration.md §2.2.5, NIM-870). It is closed on purpose: `soul-lint` has
// to judge the key OFFLINE, with no keeper and no registry to ask, and an open
// namespace would leave a typo indistinguishable from a transport this build
// does not carry.
const (
	// TransportAgent is the gRPC EventStream to a Soul that is already running
	// on the host (ADR-012) — what a task without the key gets.
	//
	// ★ Writing it is NOT a no-op everywhere. On the ordinary (pull) path it says
	// out loud what would have happened anyway; inside a PUSH run it is a
	// contradiction — the run is the ssh transport — and `pushorch` fails the run
	// rather than dispatching over a transport the author explicitly did not
	// name. A silent fall-through there would be the one outcome this key exists
	// to prevent.
	TransportAgent = "agent"
	// TransportSSH is the agentless push flow (ADR-004, ADR-032): the Keeper
	// opens an SSH session, delivers the `soul` binary and execs it.
	TransportSSH = "ssh"
)

// Parameter names of the `ssh` transport. They are the three fields a task may
// take away from the `souls.ssh_target` row — deliberately NOT the address:
// `ssh_target` carries no address column at all (its `Host` is the SID), so a
// task that could name one would be a task that bootstraps a bare VM, and it
// does not. See the NOT-SOLVED note on [TransportSpecOf].
const (
	TransportParamSSHProvider = "ssh_provider"
	TransportParamUser        = "user"
	TransportParamPort        = "port"
)

// transportParamKind is how a transport param's value node is checked.
type transportParamKind int

const (
	// transportParamString — a string scalar.
	transportParamString transportParamKind = iota
	// transportParamProvider — a string scalar matching the SshProvider name
	// format.
	transportParamProvider
	// transportParamPort — an integer in 1..65535.
	transportParamPort
)

// Interpolation is refused in every kind — see [transportInterpolated].

// transportParams is the closed param set of every registered transport. An
// absent entry is not "no constraints" — [TransportRegistered] reads this map,
// so a transport missing from it does not exist.
var transportParams = map[string]map[string]transportParamKind{
	TransportAgent: {},
	TransportSSH: {
		TransportParamSSHProvider: transportParamProvider,
		TransportParamUser:        transportParamString,
		TransportParamPort:        transportParamPort,
	},
}

// reSSHProviderName is the SshProvider registration-alias format, the same one
// `push_providers.name` and the `souls.ssh_target` CHECK constraint carry.
var reSSHProviderName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// TransportRegistered reports whether name is a transport this build carries.
func TransportRegistered(name string) bool {
	_, ok := transportParams[name]
	return ok
}

// TransportNames lists the registered transports in sorted order, for a
// diagnostic that has to say what WAS allowed. Sorted so the message is
// byte-identical for identical input.
func TransportNames() []string {
	out := make([]string, 0, len(transportParams))
	for name := range transportParams {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TransportSpec decodes the polymorphic `transport:` into the transport's name
// and its params (orchestration.md §2.2.5). Two forms, and they decode to the
// SAME name — the scalar is exactly the dict with no params:
//
//	transport: ssh                            → ("ssh", nil, true)
//	transport: { ssh: { ssh_provider: "x" } } → ("ssh", {ssh_provider: x}, true)
//
// Task.Transport is `any` because the two forms share one key, the same reason
// [Task.RequireSpec] exists next to it.
func (t Task) TransportSpec() (name string, params map[string]any, ok bool) {
	return TransportSpecOf(t.Transport)
}

// TransportSpecOf is [Task.TransportSpec] over a bare `transport:` value, for
// callers holding the key without the task around it. One decoder for both, so
// the forms cannot drift.
//
// Unset/null → ("", nil, false). A shape the validator already rejects
// (validateTransportField runs at parse time) also yields false: this is a
// decode of validated input, not a second validator. In particular a dict with
// two keys decodes to false rather than picking one — map iteration has no
// order, and silently picking would make the run non-deterministic.
//
// ★ What the key does NOT do: it does not make a bare VM reachable. The host,
// its address and its provider still come from the registry —
// `core.bootstrap.issued` writes `transport='agent'` as a literal and refuses
// `ssh`, and `souls.ssh_target` carries no address column (`Host` is the SID,
// while a fresh VM has only a `primary_ip`). That is separate scope; do not
// read this key as bootstrap.
func TransportSpecOf(transport any) (name string, params map[string]any, ok bool) {
	switch v := transport.(type) {
	case nil:
		return "", nil, false
	case string:
		if !TransportRegistered(v) {
			return "", nil, false
		}
		return v, nil, true
	case map[string]any:
		return transportFromMap(v)
	case map[any]any:
		conv := make(map[string]any, len(v))
		for k, val := range v {
			s, isStr := k.(string)
			if !isStr {
				return "", nil, false
			}
			conv[s] = val
		}
		return transportFromMap(conv)
	default:
		return "", nil, false
	}
}

func transportFromMap(m map[string]any) (string, map[string]any, bool) {
	if len(m) != 1 {
		return "", nil, false
	}
	for name, raw := range m {
		if !TransportRegistered(name) {
			return "", nil, false
		}
		switch p := raw.(type) {
		case nil:
			return name, nil, true
		case map[string]any:
			return name, p, true
		case map[any]any:
			conv := make(map[string]any, len(p))
			for k, val := range p {
				s, isStr := k.(string)
				if !isStr {
					return "", nil, false
				}
				conv[s] = val
			}
			return name, conv, true
		default:
			return "", nil, false
		}
	}
	return "", nil, false
}

// validateTransportField — `transport:` allows two forms: the scalar name of a
// registered transport, or a mapping with EXACTLY ONE key, that key being the
// transport's name and its value the transport's params.
//
// The one-key rule is this validator's job (the decoder returns false on two
// rather than choosing), and it is what makes the dict a discriminator rather
// than a bag: two transports on one task have no defined order of preference,
// and inventing one would make the choice depend on map iteration.
func validateTransportField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	path := pathPrefix + ".transport"
	switch v := kv.Value.(type) {
	case *ast.NullNode:
		return nil
	case *ast.StringNode:
		if TransportRegistered(v.Value) {
			return nil
		}
		tok := v.GetToken()
		// An interpolated NAME gets the interpolation diagnostic, not
		// "unregistered": `${ vars.t }` is not a typo in a transport name and
		// "known: agent, ssh" sends the author looking for the wrong mistake.
		if d, interp := transportInterpolated("transport", v.Value, path); interp {
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, d)}
		}
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, transportUnknown(v.Value, path))}
	case *ast.MappingNode:
		return validateTransportMapping(v, path)
	default:
		tok := kv.Value.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "transport: must be a transport name or a mapping keyed by one transport name",
			Hint:     transportHint(),
			YAMLPath: path,
		})}
	}
}

func validateTransportMapping(mm *ast.MappingNode, path string) []diag.Diagnostic {
	if len(mm.Values) == 0 {
		tok := mm.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "transport_multiple",
			Message:  "transport: mapping is empty — it must carry exactly one transport name",
			Hint:     transportHint(),
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	// The diagnostic goes on the second+ key, as for the task discriminator: the
	// first is the "primary" and naming it as the offender reads backwards.
	for i := 1; i < len(mm.Values); i++ {
		tok := mm.Values[i].Key.GetToken()
		if tok == nil {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "transport_multiple",
			Message:  fmt.Sprintf("transport: declares %d transports (%s) — exactly one is allowed", len(mm.Values), strings.Join(transportMappingKeys(mm), ", ")),
			Hint:     "a task travels one way; split it into two tasks if two transports are really meant",
			YAMLPath: path + "." + tok.Value,
		}))
	}

	first := mm.Values[0]
	tok := first.Key.GetToken()
	if tok == nil {
		return out
	}
	name := tok.Value
	if !TransportRegistered(name) {
		if d, interp := transportInterpolated("transport", name, path+"."+name); interp {
			return append(out, diagAt(tok.Position.Line, tok.Position.Column, d))
		}
		return append(out, diagAt(tok.Position.Line, tok.Position.Column, transportUnknown(name, path+"."+name)))
	}
	return append(out, validateTransportParams(name, first.Value, path+"."+name)...)
}

func validateTransportParams(name string, value ast.Node, path string) []diag.Diagnostic {
	switch pm := value.(type) {
	case *ast.NullNode:
		// `transport: { ssh: }` — the dict form with no params, which is the
		// scalar form written long. Legal, and the decoder agrees.
		return nil
	case *ast.MappingNode:
		known := transportParams[name]
		var out []diag.Diagnostic
		for _, sub := range pm.Values {
			kt := sub.Key.GetToken()
			if kt == nil {
				continue
			}
			kind, ok := known[kt.Value]
			if !ok {
				out = append(out, diagAt(kt.Position.Line, kt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "unknown_key",
					Message:  fmt.Sprintf("unknown field %q in transport.%s", kt.Value, name),
					Hint:     transportParamHint(name),
					YAMLPath: path + "." + kt.Value,
				}))
				continue
			}
			out = append(out, validateTransportParamValue(kind, kt.Value, sub.Value, path+"."+kt.Value)...)
		}
		return out
	default:
		tok := value.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("transport.%s: must be a mapping of that transport's params", name),
			Hint:     transportParamHint(name),
			YAMLPath: path,
		})}
	}
}

func validateTransportParamValue(kind transportParamKind, key string, value ast.Node, path string) []diag.Diagnostic {
	tok := value.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	mismatch := func(want string) []diag.Diagnostic {
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be %s", key, want),
			YAMLPath: path,
		})}
	}

	if sn, isStr := value.(*ast.StringNode); isStr {
		if d, interp := transportInterpolated(key, sn.Value, path); interp {
			return []diag.Diagnostic{diagAt(line, col, d)}
		}
	}

	switch kind {
	case transportParamString, transportParamProvider:
		sn, isStr := value.(*ast.StringNode)
		if !isStr {
			return mismatch("a string")
		}
		if kind != transportParamProvider {
			return nil
		}
		if reSSHProviderName.MatchString(sn.Value) {
			return nil
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "name_invalid_format",
			Message:  fmt.Sprintf("%s %q does not match the SshProvider name format", key, sn.Value),
			Hint:     "kebab-case, starts with a letter: ^[a-z][a-z0-9-]{0,62}$",
			YAMLPath: path,
		})}
	case transportParamPort:
		if _, isStr := value.(*ast.StringNode); isStr {
			return mismatch("an integer in 1..65535")
		}
		in, isInt := value.(*ast.IntegerNode)
		if !isInt {
			return mismatch("an integer in 1..65535")
		}
		n, err := strconv.Atoi(in.GetToken().Value)
		if err != nil || n < 1 || n > 65535 {
			return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "value_out_of_range",
				Message:  fmt.Sprintf("%s must be in 1..65535, got %s", key, in.GetToken().Value),
				YAMLPath: path,
			})}
		}
		return nil
	default:
		return nil
	}
}

// validateTransportOnKeeper raises `transport_on_keeper_invalid` for
// `transport:` on a keeper-side task, joining `async_on_keeper_invalid` and
// `when_on_keeper_dynamic_unsupported` in the set of keys that are meaningless
// on that side.
//
// A keeper-side task runs INSIDE the Keeper (ADR-0087: the side is derived from
// the module address, not declared) — there is no host at the far end of it and
// therefore no transport to pick. This is the open question the owner left to
// the implementation (NIM-866 comment, 2026-09-14), and refusing is the answer
// the `core.ssh.run` case makes clearest: that module dials hosts itself, from
// its own `ssh_provider`/`hosts` params, and a task-level `transport:` beside
// them would be a second spelling for a different thing under the same word.
func validateTransportOnKeeper(present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	kv, ok := present["transport"]
	if !ok {
		return nil
	}
	addr, isKeeper := keeperSideTask(present)
	if !isKeeper {
		return nil
	}
	tok := kv.Key.GetToken()
	return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:     "transport_on_keeper_invalid",
		Message:  fmt.Sprintf("transport: on a keeper-side task (%s) names a way to reach a host, and a keeper task never leaves the Keeper", keeperSideBecause(addr)),
		Hint:     "drop the key; a keeper-side module that dials hosts (core.ssh.run) takes its own ssh_provider/hosts params",
		YAMLPath: pathPrefix + ".transport",
	})}
}

// transportInterpolated refuses a `${ … }` anywhere in the key — the transport's
// name as well as a param's value.
//
// ★ This is the one refusal in the key worth reading twice. `transport:` is
// decided ONCE PER TASK, before the hosts are resolved — it lands on the
// dispatch plan, not on a per-host rendered task — so there is no env to resolve
// against, and no answer that could differ per host. Rendering it is a real
// design (a host-independent root set, refused otherwise), not an omission;
// until then the alternative to refusing is shipping the literal `${ vars.x }`
// to the dialer as a provider name, which fails at connect time with a message
// about a provider nobody wrote.
func transportInterpolated(key, value, path string) (diag.Diagnostic, bool) {
	if !strings.Contains(value, "${") {
		return diag.Diagnostic{}, false
	}
	return diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "transport_interpolation_unsupported",
		Message:  fmt.Sprintf("%s: `${ … }` is not resolved in transport: — the key is decided once per task, before hosts are known", key),
		Hint:     "write the value literally; a per-host transport is not expressible today",
		YAMLPath: path,
	}, true
}

func transportUnknown(name, path string) diag.Diagnostic {
	return diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "transport_unknown",
		Message:  fmt.Sprintf("transport %q is not registered — known: %s", name, strings.Join(TransportNames(), ", ")),
		Hint:     transportHint(),
		YAMLPath: path,
	}
}

func transportHint() string {
	return "transport: " + TransportSSH + "  |  transport: { " + TransportSSH + ": { " + TransportParamSSHProvider + ": <name> } }"
}

func transportParamHint(name string) string {
	known := make([]string, 0, len(transportParams[name]))
	for k := range transportParams[name] {
		known = append(known, k)
	}
	sort.Strings(known)
	if len(known) == 0 {
		return fmt.Sprintf("transport.%s takes no params — write the scalar `transport: %s`", name, name)
	}
	return fmt.Sprintf("transport.%s accepts only %s", name, strings.Join(known, ", "))
}

func transportMappingKeys(mm *ast.MappingNode) []string {
	out := make([]string, 0, len(mm.Values))
	for _, kv := range mm.Values {
		if tok := kv.Key.GetToken(); tok != nil {
			out = append(out, tok.Value)
		}
	}
	return out
}
