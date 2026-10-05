package ssh

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// StateApply applies a destiny to each host of a list over SSH: the agent binary
// is delivered, and `soul apply` runs the destiny the Keeper rendered for that
// host (NIM-905, docs/adr/draft-ssh-apply-unregistered-hosts.md).
//
// The list, not the registry, says who the hosts are — a machine created seconds
// ago has no `souls` row that could route it, and this state neither reads nor
// writes one. Everything after the dial is [push.ApplyOverSession], the same code
// the registry's push branch runs.
const StateApply = "apply"

// auditWriteTimeout caps the audit write, which runs detached from the run's
// context and so would otherwise be bounded by nothing.
const auditWriteTimeout = 5 * time.Second

func validateApplyParams(params *structpb.Struct) []string {
	var errs []string
	if _, err := util.StringParam(params, "destiny"); err != nil {
		errs = append(errs, err.Error())
	}
	input, err := util.OptStructParam(params, "input")
	if err != nil {
		errs = append(errs, err.Error())
	}
	inputFrom, err := parseInputFrom(params)
	if err != nil {
		errs = append(errs, err.Error())
	}
	if err := inputOverlap(input, inputFrom); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

// parseInputFrom reads `input_from:` — destiny input name → the name of a string
// field of the host entry. It is how a per-host value (a host's own bootstrap
// token) reaches the destiny: this step renders once for the whole run, so
// `input:` cannot say "this host's".
func parseInputFrom(params *structpb.Struct) (map[string]string, error) {
	s, err := util.OptStructParam(params, "input_from")
	if err != nil || s == nil {
		return nil, err
	}
	out := make(map[string]string, len(s.GetFields()))
	for name, v := range s.GetFields() {
		field, ok := v.GetKind().(*structpb.Value_StringValue)
		if !ok || field.StringValue == "" {
			return nil, fmt.Errorf("param \"input_from\".%s: expected the name of a host field", name)
		}
		out[name] = field.StringValue
	}
	return out, nil
}

// inputOverlap refuses a name given both a value and a host field: which one the
// destiny would get is a guess, and either answer silently drops the other.
func inputOverlap(input *structpb.Struct, inputFrom map[string]string) error {
	var both []string
	for name := range inputFrom {
		if _, ok := input.GetFields()[name]; ok {
			both = append(both, name)
		}
	}
	if len(both) == 0 {
		return nil
	}
	sort.Strings(both)
	return fmt.Errorf("param \"input\" and \"input_from\" both set %s — give each destiny input one source", strings.Join(both, ", "))
}

// preparedHost is one entry with its destiny already rendered. A skipped entry
// carries no render.
type preparedHost struct {
	host     hostInput
	rendered *util.RenderedDestiny
}

// hostOutcome is what one host's run came to.
type hostOutcome struct {
	sid     string
	skipped bool
	ran     bool
	changed bool
	tasks   []map[string]any
	err     error
}

func (m *Module) applyDestiny(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if errs := validateApplyParams(req.Params); len(errs) > 0 {
		return util.SendFailed(stream, "ssh apply: "+strings.Join(errs, "; "))
	}
	providerName, err := util.StringParam(req.Params, "ssh_provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	destiny, _ := util.StringParam(req.Params, "destiny")
	inputStruct, _ := util.OptStructParam(req.Params, "input")
	inputFrom, _ := parseInputFrom(req.Params)
	hosts, err := parseHosts(req.Params, !m.teleport())
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Hosts run at once, so a SID listed twice would be two deliveries and two
	// `soul apply` runs racing on one machine — and one token redeemed twice.
	seen := make(map[string]int, len(hosts))
	for i, h := range hosts {
		if j, dup := seen[h.sid]; dup {
			return util.SendFailed(stream, fmt.Sprintf("ssh apply: hosts[%d] and hosts[%d] are both %q — a host is applied to once", j, i, h.sid))
		}
		seen[h.sid] = i
	}
	sshUser, sshPort, joinWait, err := parseConn(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// A bare host has no agent; without delivery there is nothing to exec.
	if m.Deliverer == nil {
		if m.DeliveryErr != nil {
			return util.SendFailed(stream, "ssh apply: agent delivery is unavailable: "+maskErr(m.DeliveryErr))
		}
		return util.SendFailed(stream, "ssh apply: keeper.yml::push.soul_binary_path is unset — a host with no agent has no `soul` to apply the destiny with")
	}
	if m.Dial == nil {
		return util.SendFailed(stream, "ssh apply: dialer not configured (wire push.Dial / push.NewTeleportDialer in main)")
	}
	prov, hostCAs, refusal := m.directAuth("ssh apply", providerName)
	if refusal != "" {
		return util.SendFailed(stream, refusal)
	}
	renderDestiny := util.DestinyRendererFrom(ctx)
	if renderDestiny == nil {
		return util.SendFailed(stream, "ssh apply: no destiny render on the module context — this state runs inside a scenario run")
	}

	// The step's own sealed params too: a `${ vault(…) }` handed to a destiny
	// input the destiny does not declare secret is still a secret, and the audit
	// record's per-host error is masked by nothing downstream.
	secrets := append(hostSecrets(hosts), render.SealedValues(req.Params.AsMap(), util.SealedPathsFrom(ctx))...)

	// ★ Every host is rendered before any host is dialed: a missing field or a
	// render error must stop the step with nothing executed anywhere.
	prepared := make([]preparedHost, len(hosts))
	for i, h := range hosts {
		prepared[i].host = h
		if h.onboarded {
			continue
		}
		input := map[string]any{}
		if inputStruct != nil {
			input = inputStruct.AsMap()
		}
		for name, field := range inputFrom {
			v := h.fields[field]
			if v == "" {
				return util.SendFailed(stream, fmt.Sprintf("ssh apply: hosts[%d] %q carries no string field %q for input_from.%s", i, h.sid, field, name))
			}
			input[name] = v
		}
		rendered, rerr := renderDestiny(ctx, destiny, h.sid, input)
		if rerr != nil {
			return util.SendFailed(stream, maskValues(fmt.Sprintf("ssh apply: render destiny %q for %q: %v", destiny, h.sid, rerr), secrets))
		}
		prepared[i].rendered = rendered
		secrets = append(secrets, rendered.Secrets...)
	}

	applyID := ""
	if scope, ok := util.RunScopeFrom(ctx); ok {
		applyID = scope.ApplyID
	}

	outcomes := make([]hostOutcome, len(prepared))
	var wg sync.WaitGroup
	for i, p := range prepared {
		if p.rendered == nil {
			outcomes[i] = hostOutcome{sid: p.host.sid, skipped: true}
			continue
		}
		wg.Add(1)
		go func(i int, p preparedHost) {
			defer wg.Done()
			outcomes[i] = m.applyHost(ctx, prov, hostCAs, p, sshUser, sshPort, joinWait, applyID)
		}(i, p)
	}
	wg.Wait()

	ref := ""
	for _, p := range prepared {
		if p.rendered != nil {
			ref = p.rendered.Ref
			break
		}
	}
	var auditErr error
	if m.Audit != nil {
		// Detached from the run's context: the record of what ran matters most
		// when the run is being cut off, and a write on the cancelled context
		// would be lost exactly then.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
		ev := m.applyAuditEvent(providerName, destiny, ref, outcomes, secrets)
		ev.CorrelationID = applyID
		auditErr = m.Audit.Write(actx, ev)
		cancel()
	}

	var failed []string
	results := make([]any, 0, len(outcomes))
	skipped, changed := 0, 0
	for _, o := range outcomes {
		results = append(results, map[string]any{"sid": o.sid, "ran": o.ran, "skipped": o.skipped, "changed": o.changed})
		if o.skipped {
			skipped++
		}
		if o.changed {
			changed++
		}
		if o.err != nil {
			failed = append(failed, fmt.Sprintf("%q: %s", o.sid, maskValues(o.err.Error(), secrets)))
		}
	}
	if len(failed) > 0 {
		msg := fmt.Sprintf("ssh apply: destiny %q failed on %d of %d host(s): %s", destiny, len(failed), len(outcomes), strings.Join(failed, "; "))
		if auditErr != nil {
			msg += "; audit write: " + maskErr(auditErr)
		}
		return util.SendFailed(stream, msg)
	}
	if auditErr != nil {
		return util.SendFailed(stream, "ssh apply: audit write: "+maskErr(auditErr))
	}
	return util.SendFinal(stream, changed > 0, map[string]any{
		"hosts":   results,
		"count":   float64(len(outcomes)),
		"skipped": float64(skipped),
		"changed": float64(changed),
	})
}

// applyHost dials one host, delivers the agent and runs the destiny on it. Only
// the dial is this module's; the rest is the push executor.
func (m *Module) applyHost(ctx context.Context, prov SshProviderHost, hostCAs []push.NamedHostKeyAuthority, p preparedHost, user string, port int, joinWait time.Duration, applyID string) hostOutcome {
	out := hostOutcome{sid: p.host.sid}
	var (
		sess push.Session
		err  error
	)
	if m.teleport() {
		sess, err = m.dialTeleport(ctx, p.host, user, port, joinWait)
	} else {
		sess, err = m.dialDirect(ctx, prov, hostCAs, p.host, user, port, joinWait)
	}
	if err != nil {
		// The SID already heads this host's line; only a different dial address
		// (direct's primary_ip) adds anything.
		if target := p.host.connectTarget(m.teleport()); target != p.host.sid {
			err = fmt.Errorf("%s: %w", target, err)
		}
		out.err = err
		return out
	}
	defer func() { _ = sess.Close() }()

	tasks := p.rendered.Tasks
	statuses := make([]keeperv1.TaskStatus, len(tasks))
	var (
		firstFailure  *keeperv1.TaskEvent
		agentAnswered bool
	)
	rr, err := push.ApplyOverSession(ctx, sess, m.Deliverer, m.SoulSpec, push.HostSoulBinaryPath, p.host.sid,
		&keeperv1.ApplyRequest{ApplyId: applyID, Tasks: tasks},
		func(ev *keeperv1.TaskEvent) {
			agentAnswered = true
			idx := int(ev.GetTaskIdx())
			if idx < 0 || idx >= len(statuses) {
				return
			}
			statuses[idx] = ev.GetStatus()
			switch ev.GetStatus() {
			case keeperv1.TaskStatus_TASK_STATUS_CHANGED:
				out.changed = true
			case keeperv1.TaskStatus_TASK_STATUS_FAILED, keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT:
				if firstFailure == nil {
					firstFailure = ev
				}
			}
		})
	// A host whose delivery failed, or whose agent printed nothing, ran no task;
	// listing every task as not_run would read as a destiny that started.
	if agentAnswered || rr != nil {
		out.tasks = taskRecords(tasks, statuses)
	}
	if err != nil {
		out.err = err
		return out
	}
	out.ran = true
	if rr.GetStatus() == keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		return out
	}
	if firstFailure != nil {
		idx := int(firstFailure.GetTaskIdx())
		out.err = fmt.Errorf("task %q (%s) %s: %s", tasks[idx].GetName(), tasks[idx].GetModule(),
			statusLabel(firstFailure.GetStatus()), firstFailure.GetError().GetMessage())
		return out
	}
	out.err = errors.New("run ended " + strings.ToLower(strings.TrimPrefix(rr.GetStatus().String(), "RUN_STATUS_")))
	return out
}

// taskRecords is the audit's account of what ran: each task by name and module
// with its outcome, and nothing from its params or output.
func taskRecords(tasks []*keeperv1.RenderedTask, statuses []keeperv1.TaskStatus) []map[string]any {
	out := make([]map[string]any, 0, len(tasks))
	for i, t := range tasks {
		status := "not_run"
		if statuses[i] != keeperv1.TaskStatus_TASK_STATUS_UNSPECIFIED {
			status = statusLabel(statuses[i])
		}
		out = append(out, map[string]any{"task": t.GetName(), "module": t.GetModule(), "status": status})
	}
	return out
}

func statusLabel(s keeperv1.TaskStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "TASK_STATUS_"))
}

func (m *Module) applyAuditEvent(providerName, destiny, ref string, outcomes []hostOutcome, secrets []string) *audit.Event {
	hosts := make([]any, 0, len(outcomes))
	sids := make([]any, 0, len(outcomes))
	skipped := 0
	for _, o := range outcomes {
		sids = append(sids, o.sid)
		entry := map[string]any{"sid": o.sid}
		switch {
		case o.skipped:
			skipped++
			entry["skipped"] = true
		case o.err != nil:
			entry["status"] = "failed"
			entry["error"] = maskValues(o.err.Error(), secrets)
		default:
			entry["status"] = "success"
		}
		if o.tasks != nil {
			tasks := make([]any, 0, len(o.tasks))
			for _, t := range o.tasks {
				tasks = append(tasks, t)
			}
			entry["tasks"] = tasks
		}
		hosts = append(hosts, entry)
	}
	named := destiny
	if ref != "" {
		named = destiny + "@" + ref
	}
	return &audit.Event{
		EventType: audit.EventSSHRun,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"action":       StateApply,
			"destiny":      named,
			"ssh_provider": providerName,
			"transport":    m.transportName(),
			"count":        float64(len(outcomes)),
			"skipped":      float64(skipped),
			"sids":         sids,
			"hosts":        hosts,
		},
	}
}

// hostSecrets is every non-empty value a host entry carries under a name the
// platform treats as sensitive — the token `input_from:` hands the destiny.
func hostSecrets(hosts []hostInput) []string {
	var out []string
	for _, h := range hosts {
		for k, v := range h.fields {
			if v != "" && audit.IsSensitiveKey(k) {
				out = append(out, v)
			}
		}
	}
	return out
}

// maskValues replaces every known secret value in text, then applies the
// value-shape filter of [maskErrText]. Longest first: a short secret that is a
// substring of a longer one must not consume its prefix and leave the tail.
func maskValues(text string, secrets []string) string {
	ordered := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			ordered = append(ordered, s)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	for _, s := range ordered {
		text = strings.ReplaceAll(text, s, audit.MaskedValue)
	}
	return maskErrText(text)
}
