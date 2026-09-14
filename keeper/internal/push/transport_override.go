package push

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Route is the per-host transport decision one push dispatch runs under: which
// SshProvider plugin serves the host, and what the TASK said should override
// the registry (`transport:`, NIM-870).
//
// Provider is required — an empty one is a programming error, not "pick a
// default": the three routing levels already end in ErrProviderNotRouted rather
// than a fallback, because different providers have different auth perimeters.
type Route struct {
	Provider string
	Override TransportOverride
}

// TransportOverride is a task's `transport: { ssh: … }` reduced to the fields
// the push flow resolves per host. A zero field means the task said nothing
// about it and the registry keeps its answer.
//
// ★ It deliberately carries no ADDRESS. `souls.ssh_target` has no address
// column — its `Host` is the SID — so a task that could name one would be a
// task that reaches a machine the registry has never heard of, and this key
// does not do that. `core.bootstrap.issued` still writes `transport='agent'`
// as a literal and refuses `ssh`. Bootstrapping a bare VM is separate scope.
type TransportOverride struct {
	// Provider overrides `souls.ssh_target.ssh_provider` (Level 1 of the 3-tier
	// resolve) and, with it, the coven and cluster defaults below it.
	Provider string
	// User overrides `souls.ssh_target.ssh_user`.
	User string
	// Port overrides `souls.ssh_target.ssh_port`. 0 = unset.
	Port int
}

// Empty reports whether the task overrode nothing.
func (o TransportOverride) Empty() bool {
	return o.Provider == "" && o.User == "" && o.Port == 0
}

// TransportOverrideFrom decodes a rendered plan's transport pair
// ([render.DispatchPlan.TransportName] / TransportParams, already through
// [config.TransportSpecOf]) into an override.
//
// ok=false for a task that named no transport and for `transport: agent` —
// the agent transport is the pull stream and has nothing for the push flow to
// override. An `ssh` transport with an unusable param is an ERROR rather than a
// silent drop: the whole point of the key is that it beats the registry, so a
// value that did not survive the decode must fail the run instead of letting
// the registry answer as though the task had been silent.
func TransportOverrideFrom(name string, params map[string]any) (TransportOverride, bool, error) {
	if name != config.TransportSSH {
		return TransportOverride{}, false, nil
	}
	var o TransportOverride
	for key, raw := range params {
		switch key {
		case config.TransportParamSSHProvider:
			s, err := transportParamString(key, raw)
			if err != nil {
				return TransportOverride{}, false, err
			}
			o.Provider = s
		case config.TransportParamUser:
			s, err := transportParamString(key, raw)
			if err != nil {
				return TransportOverride{}, false, err
			}
			o.User = s
		case config.TransportParamPort:
			n, err := transportParamInt(key, raw)
			if err != nil {
				return TransportOverride{}, false, err
			}
			o.Port = n
		default:
			return TransportOverride{}, false, fmt.Errorf("push: transport.ssh: unknown param %q", key)
		}
	}
	return o, true, nil
}

// Apply lays the override over a registry-resolved target, field by field. The
// override is sparse on purpose: a task that named only a user must not blank
// the port the registry (or its default) resolved.
//
// It reports no per-field source. An earlier version did, and the labels were
// wrong in the one case that matters — [PGFallbackTargetResolver] fills an unset
// `ssh_user`/`ssh_port` from defaultSSHUser/defaultSSHPort, which is not the
// `souls.ssh_target` row a "soul" label would send a reader to. The run summary
// says which fields the TASK set (pushorch.summarize reads the override
// directly); anything it does not name came from the resolver, and the resolver
// is the one place that knows whether that was the row or its default.
func (o TransportOverride) Apply(t SSHTarget) SSHTarget {
	if o.User != "" {
		t.User = o.User
	}
	if o.Port != 0 {
		t.Port = o.Port
	}
	return t
}

func transportParamString(key string, raw any) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("push: transport.ssh.%s: expected a string, got %T", key, raw)
	}
	return s, nil
}

// transportParamInt accepts the integer shapes a YAML decode can produce. goccy
// hands back uint64 for a positive literal and int64 for a negative one; a value
// that travelled through JSON (the push API body) arrives as float64.
func transportParamInt(key string, raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case uint64:
		return int(v), nil
	case float64:
		if v != float64(int64(v)) {
			return 0, fmt.Errorf("push: transport.ssh.%s: expected an integer, got %v", key, v)
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("push: transport.ssh.%s: expected an integer, got %T", key, raw)
	}
}
