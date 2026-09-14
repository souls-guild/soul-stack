// The four actions.
//
// Each opens a hypervisor from its own `endpoint`, does its work, and ends with
// exactly one final event carrying changed/failed and the output that becomes
// `register.<task>.*` (ADR-012).
package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// readyTimeout bounds the wait for a batch to take its DHCP leases. Not a param:
// a scenario has no information a hypervisor does not about how long a lease
// takes, so the knob would only ever be tuned to paper over a broken network.
// Tests shorten it; nothing at runtime writes it.
var readyTimeout = 5 * time.Minute

// pollInterval paces both the readiness wait and the teardown confirmation.
var pollInterval = 2 * time.Second

func progress(stream eventStream, format string, args ...any) {
	_ = stream.Send(&pluginv1.ApplyEvent{Message: fmt.Sprintf(format, args...)})
}

// finish sends the one final event. An output that cannot be marshalled is a
// failure rather than a success with a missing register — a downstream step
// reading register.provision.hosts would otherwise see nothing and not know why.
func finish(stream eventStream, changed, failed bool, msg string, out map[string]any) error {
	ev := &pluginv1.ApplyEvent{Message: msg, Changed: changed, Failed: failed}
	if out != nil {
		s, err := structpb.NewStruct(out)
		if err != nil {
			return sendFailure(stream, fmt.Sprintf("%s, but the output could not be encoded: %v", msg, err))
		}
		ev.Output = s
	}
	return stream.Send(ev)
}

// failWithHosts is the refusal that still hands back the machines this run made.
//
// ★ A create that fails on the third of five leaves two machines running, and a
// failure event with no output loses them: the caller's cleanup step has nothing
// to work from. Every id this run is responsible for goes out even on the way to
// reporting that the run failed.
func failWithHosts(stream eventStream, msg string, ids []string, changed bool) error {
	hosts := make([]any, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, hostStub(id))
	}
	return finish(stream, changed, true, msg, map[string]any{"hosts": hosts})
}

// ★★ THE DECLARED OUTPUT SHAPE.
//
// A module document declares its INPUTS and nothing else, so the platform
// type-checks no part of what comes back out. `register.<task>.*` is therefore
// the one dimension of this contract that can change under a consumer with every
// test in the tree still green — and it did: the state descriptions promised
// `external_ip` this artifact has never emitted and omitted `state`, which it
// always has.
//
// These lists are the artifact's own statement of that shape. Each is quoted in
// the state description an operator reads, and [TestOutputShapeIsDeclared] holds
// BOTH the emitters and those descriptions to them — so a key added to a map
// below without a line here is a red test rather than a scenario that quietly
// starts reading nothing.
var (
	hostEntryKeys = []string{"vm_id", "sid", "primary_ip", "state", "attributes"}
	hostAttrKeys  = []string{"namespace", "name", "cpu_size", "ram_size", "image_id", "network_id", "created_at", "run_label"}
	hostStubKeys  = []string{"vm_id"}
	resizeKeys    = []string{"vm_id", "changed", "caused_downtime"} // plus "error" on a failure
)

// hostEntry is the shape `core.soul.registered` and the bootstrap consumers read,
// for a machine that IS ready.
//
// ★ primary_ip is FLAT. Nesting it under `network:` — which is the soulprint's
// shape and a plausible thing to reach for — makes it invisible to the consumer
// while every test that only checks sid stays green.
//
// bootstrap_token is absent, and that is a known red downstream rather than an
// oversight: minting one needs the Keeper's token store, which a plugin has no
// access to.
func hostEntry(d domainInfo, ip, hostname string) map[string]any {
	h := map[string]any{
		"vm_id":      d.UUID,
		"sid":        sidFor(d, hostname),
		"primary_ip": ip,
	}
	h["attributes"] = map[string]any{
		"namespace":  d.Namespace,
		"name":       d.Name,
		"cpu_size":   d.VCPUs,
		"ram_size":   d.MemoryBytes,
		"image_id":   d.ImageID,
		"network_id": d.NetworkID,
		"created_at": d.CreatedAt,
		// The batch identity is echoed back so a scenario can read the identity it
		// filtered `probed` by, which `run_label` as an input filter alone would
		// not let it do.
		"run_label": d.runLabel(),
	}
	h["state"] = powerState(d)
	return h
}

// hostStub is what a machine that did NOT come up gets: its id and nothing else.
//
// ★ Deliberately not a hostEntry with blank fields. `sidFor` would fall back to
// `<name>.<namespace>` and manufacture a plausible sid for a machine that never
// announced one — and a manufactured sid is exactly what the downstream guard
// keys on, so filling it in disables the check that would have caught this.
func hostStub(vmID string) map[string]any {
	return map[string]any{"vm_id": vmID}
}

// resizeResult is one machine's entry in `output.results`. `error` is present
// only on a failure: a key holding "" would make `has(r.error)` true for every
// machine in the batch, and that is the predicate a scenario writes.
func resizeResult(vmID string, changed, downtime bool, err error) map[string]any {
	r := map[string]any{"vm_id": vmID, "changed": changed, "caused_downtime": downtime}
	if err != nil {
		r["error"] = err.Error()
	}
	return r
}

func powerState(d domainInfo) string {
	if d.Running {
		return "RUNNING"
	}
	return "STOPPED"
}

// sidFor prefers the machine's own account of itself — the hostname it announced
// over DHCP — and falls back to the domain name qualified by the namespace.
func sidFor(d domainInfo, hostname string) string {
	if hostname != "" {
		return hostname
	}
	if d.Name != "" && d.Namespace != "" {
		return d.Name + "." + d.Namespace
	}
	return d.Name
}

// applyCreated provisions a batch and waits for it.
//
// Idempotent on the batch identity: it adopts the machines it already made
// (matched by the run label, or by the `<name>-<seq>` naming for machines made
// before a label existed) and tops up only what is missing, at the first free
// indexes.
func (m *VMLocal) applyCreated(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	p := params.AsMap()
	f := newFields(p, "")
	rawProfile, ok := p["profile"].(map[string]any)
	if !ok {
		// Unreachable through the action table — validateCreated runs first and
		// refuses this — but a bare type assertion here would turn a future
		// direct caller's mistake into a panic in a Keeper process.
		return sendFailure(stream, "profile is required and must be an object")
	}
	prof, _ := parseProfile(rawProfile)

	namespace := prof.scope(f.str(connNamespace))
	name := f.str("name")
	count := f.integer("count")
	if count == 0 {
		count = 1
	}
	runLabel := prof.runLabel
	if runLabel == "" {
		runLabel = name
	}

	h, err := m.connect(f.str(connEndpoint))
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	defer h.Close()

	networkName, networkID, err := h.LookupNetwork(prof.networkID)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("profile.network_id: %v", err))
	}
	imageID, imagePath, err := h.ResolveImage(prof)
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	existing, err := h.ListDomains(namespace)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("scan namespace %q: %v", namespace, err))
	}
	adopted := adoptBatch(existing, runLabel, name)
	progress(stream, "namespace %q holds %d VM of batch %q; target is %d", namespace, len(adopted), runLabel, count)

	// Names held by machines in this namespace that are NOT part of this batch.
	// Creating over one would collide inside libvirt with an error about a
	// storage volume, which says nothing about the actual problem.
	foreign := map[string]bool{}
	for _, d := range existing {
		if _, mine := adopted[d.Name]; !mine {
			foreign[d.Name] = true
		}
	}

	// ★ ids is every machine this run is responsible for, and it is complete
	// BEFORE anything is written. Appending as we go means a failure half-way
	// hands back only the machines the loop happened to have reached, and the rest
	// — already existing, already running — are invisible to the cleanup step.
	adoptedNames := make([]string, 0, len(adopted))
	for n := range adopted {
		adoptedNames = append(adoptedNames, n)
	}
	sort.Strings(adoptedNames)
	ids := make([]string, 0, count)
	for _, n := range adoptedNames {
		ids = append(ids, adopted[n].UUID)
	}

	created := 0
	for _, n := range adoptedNames {
		d := adopted[n]
		// A member that is powered down cannot take a lease. Bringing it back is
		// the convergence `created` promises; leaving it would hang the wait and
		// then report the batch unusable.
		if d.Running {
			continue
		}
		if err := h.StartDomain(d.UUID); err != nil {
			return failWithHosts(stream, fmt.Sprintf("%s is stopped and would not start: %v", n, err), ids, created > 0)
		}
		d.Running = true
		adopted[n] = d
		created++
		progress(stream, "%s was stopped and has been started", n)
	}

	for seq := 0; len(adopted) < int(count); seq++ {
		vmName := batchMemberName(name, runLabel, seq)
		if _, taken := adopted[vmName]; taken {
			continue
		}
		if foreign[vmName] {
			return failWithHosts(stream, fmt.Sprintf(
				"%q already exists in namespace %q and is not part of batch %q — "+
					"pick another name, or adopt that machine by labelling it %s=%s",
				vmName, namespace, runLabel, runLabelKey, runLabel), ids, created > 0)
		}

		labels := map[string]string{}
		for k, v := range prof.labels {
			labels[k] = v
		}
		labels[runLabelKey] = runLabel

		seed, serr := buildSeed(vmName, namespace, f.str("userdata"))
		if serr != nil {
			return failWithHosts(stream, fmt.Sprintf("build cloud-init seed for %s: %v", vmName, serr), ids, created > 0)
		}
		d, cerr := h.CreateDomain(ctx, domainSpec{
			Name:        vmName,
			Namespace:   namespace,
			Labels:      labels,
			Profile:     prof,
			ImageID:     imageID,
			ImagePath:   imagePath,
			NetworkID:   networkID,
			NetworkName: networkName,
			Seed:        seed,
		})
		if cerr != nil {
			return failWithHosts(stream, fmt.Sprintf("create %s: %v", vmName, cerr), ids, created > 0)
		}
		adopted[vmName] = d
		ids = append(ids, d.UUID)
		created++
		progress(stream, "created %s (%s)", vmName, d.UUID)
	}

	hosts, notReady, err := m.awaitBatch(ctx, stream, h, networkID, adopted)
	if err != nil {
		return failWithHosts(stream, err.Error(), ids, created > 0)
	}
	out := map[string]any{"hosts": hosts}
	if len(notReady) > 0 {
		// ★ FAILED, not a success with a warning. A machine with no address is
		// not provisioned, and reporting the step complete hands the scenario a
		// roster it cannot use — two steps later, with somebody else's message.
		return finish(stream, created > 0, true,
			fmt.Sprintf("some VMs did not become usable (%s); every vm_id is in output.hosts for cleanup",
				strings.Join(notReady, ", ")), out)
	}
	return finish(stream, created > 0, false,
		fmt.Sprintf("batch %q holds %d ready VM (%d created this run)", runLabel, len(hosts), created), out)
}

// batchMemberName is the deterministic name of the seq-th machine in a batch.
//
// `<name>-<seq>` when the step named the batch, `soul-<runLabel>-<seq>` when the
// identity came only from the run label.
//
// ★ It matters beyond tidiness: the machine name is what `sid` falls back to when
// the guest announced none, so changing the rule silently renames every host in
// an existing batch and orphans the registry rows keyed on the old sid.
func batchMemberName(name, runLabel string, seq int) string {
	if name != "" {
		return fmt.Sprintf("%s-%d", name, seq)
	}
	return fmt.Sprintf("soul-%s-%d", runLabel, seq)
}

// batchNameRe matches the `<name>-<seq>` form, ANCHORED.
//
// ★ An unanchored prefix test adopts a machine from another batch: with
// `name: web`, `strings.HasPrefix("web-db-0", "web-")` is true, so an unrelated
// `web-db` machine is counted towards the batch, reported in output.hosts, handed
// to the scenario to bootstrap, and torn down by a later `destroyed`.
func batchNameRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[0-9]+$`)
}

// adoptBatch picks out the machines that belong to this batch: by run label, or
// by the `<name>-<seq>` naming for machines made before the label existed.
//
// A machine that is STOPPED is still ours and is still adopted. `created` is a
// state, not an imperative — "count machines of this batch exist and are ready" —
// so a member that is powered down is something to start, which applyCreated
// does. The alternatives are both worse: refusing to adopt it makes every rerun
// after a host reboot build a sibling and leave the dead one behind for the next
// rerun to ignore as well, and treating it as a stranger fails the run on a name
// collision with itself.
func adoptBatch(domains []domainInfo, runLabel, name string) map[string]domainInfo {
	out := map[string]domainInfo{}
	var re *regexp.Regexp
	if name != "" {
		re = batchNameRe(name)
	}
	for _, d := range domains {
		switch {
		case runLabel != "" && d.runLabel() == runLabel:
		case d.runLabel() == "" && re != nil && re.MatchString(d.Name):
		default:
			continue
		}
		out[d.Name] = d
	}
	return out
}

// awaitBatch blocks until every machine has a lease, or readyTimeout passes.
//
// Readiness is "the guest took a DHCP lease and reported a hostname", and both
// halves are load-bearing: an address without a hostname is a machine that booted
// but has not run cloud-init, which is exactly the state that later surfaces as an
// SSH timeout three steps away.
func (m *VMLocal) awaitBatch(ctx context.Context, stream eventStream, h hypervisor, networkID string, batch map[string]domainInfo) ([]any, []string, error) {
	names := make([]string, 0, len(batch))
	for n := range batch {
		names = append(names, n)
	}
	sort.Strings(names)

	ready := map[string]map[string]any{}
	deadline := time.Now().Add(readyTimeout)

	for {
		for _, n := range names {
			if _, done := ready[n]; done {
				continue
			}
			d := batch[n]
			ip, hostname, ok, err := h.Lease(ctx, networkID, d.MAC)
			if err != nil {
				return nil, nil, fmt.Errorf("read lease for %s: %w", n, err)
			}
			if !ok || ip == "" || hostname == "" {
				continue
			}
			ready[n] = hostEntry(d, ip, hostname)
			progress(stream, "%s is up at %s as %s", n, ip, hostname)
		}
		if len(ready) == len(names) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	hosts := make([]any, 0, len(names))
	var notReady []string
	for _, n := range names {
		if e, ok := ready[n]; ok {
			hosts = append(hosts, e)
			continue
		}
		notReady = append(notReady, n)
		hosts = append(hosts, hostStub(batch[n].UUID))
	}
	return hosts, notReady, nil
}

// applyDestroyed tears machines down and CONFIRMS it.
//
// A machine that is already gone is an idempotent success. One carrying
// deletion_protection is refused — the flag is honoured rather than recorded,
// because a protection that does not protect is worse than none.
func (m *VMLocal) applyDestroyed(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	p := params.AsMap()
	f := newFields(p, "")
	ids := f.strList("vm_ids")

	h, err := m.connect(f.str(connEndpoint))
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	defer h.Close()

	namespace := f.str(connNamespace)

	torn := 0
	var failed []string
	hosts := make([]any, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, hostStub(id))

		d, lerr := h.LookupDomain(id)
		if errors.Is(lerr, errNoDomain) {
			progress(stream, "%s is already gone", id)
			continue
		}
		if lerr != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", id, lerr))
			continue
		}
		// The namespace is the scope this artifact may touch. Refusing here is
		// what keeps a stray vm_id from reaching an unrelated VM on the same box.
		if d.Namespace != namespace {
			failed = append(failed, fmt.Sprintf("%s lives in namespace %q, not %q", id, d.Namespace, namespace))
			continue
		}
		if d.DeletionProtection {
			failed = append(failed, fmt.Sprintf("%s carries deletion_protection", id))
			continue
		}
		if derr := m.confirmDestroy(ctx, h, id); derr != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", id, derr))
			continue
		}
		torn++
		progress(stream, "%s torn down and confirmed gone", id)
	}

	out := map[string]any{"hosts": hosts}
	if len(failed) > 0 {
		return finish(stream, torn > 0, true,
			"teardown could not be confirmed for: "+strings.Join(failed, "; "), out)
	}
	return finish(stream, torn > 0, false,
		fmt.Sprintf("%d of %d VM torn down and confirmed (the rest were already gone)", torn, len(ids)), out)
}

// confirmDestroy re-issues the delete until the machine is actually gone. A
// domain caught mid-definition can refuse to go, and "accepted" is not "gone".
func (m *VMLocal) confirmDestroy(ctx context.Context, h hypervisor, id string) error {
	deadline := time.Now().Add(readyTimeout)
	// last is the most recent reason the teardown is not yet confirmed. It is
	// cleared whenever a round makes progress, so a transient blip on one
	// iteration is not still being reported after a later round succeeded.
	var last error
	for {
		switch err := h.DestroyDomain(id); {
		case errors.Is(err, errVolumeSurvived):
			// ★ TERMINAL, and it must not go round again. The domain is already
			// gone, so nothing names its disks any more and no retry can re-derive
			// this; the next round would answer "already gone", clear the error and
			// report a confirmed teardown over a disk that is still on the host.
			return err
		case err == nil, errors.Is(err, errNoDomain):
			last = nil
		default:
			last = err
		}
		if _, err := h.LookupDomain(id); errors.Is(err, errNoDomain) {
			return last
		} else if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("still present after %s: %w", readyTimeout, last)
			}
			return fmt.Errorf("still present after %s", readyTimeout)
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w (last failure: %v)", ctx.Err(), last)
			}
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// applyProbed reads the inventory. Read-only, changed=false by design.
func (m *VMLocal) applyProbed(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	p := params.AsMap()
	f := newFields(p, "")

	h, err := m.connect(f.str(connEndpoint))
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	defer h.Close()

	namespace := f.str(connNamespace)
	domains, err := h.ListDomains(namespace)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("list namespace %q: %v", namespace, err))
	}

	wanted := map[string]bool{}
	for _, id := range f.strList("vm_ids") {
		wanted[id] = true
	}
	runLabel := f.str("run_label")

	sort.Slice(domains, func(i, j int) bool { return domains[i].Name < domains[j].Name })
	hosts := make([]any, 0, len(domains))
	for _, d := range domains {
		if len(wanted) > 0 {
			if !wanted[d.UUID] {
				continue
			}
		} else if runLabel != "" && d.runLabel() != runLabel {
			continue
		}
		// ★ A lease that cannot be read leaves this machine without an address,
		// not the whole probe without an answer. A domain whose metadata lost its
		// network id would otherwise make the entire namespace unlistable — and a
		// probe exists precisely to look at machines that are in a bad way.
		ip, hostname, _, lerr := h.Lease(ctx, d.NetworkID, d.MAC)
		if lerr != nil {
			progress(stream, "%s: no address read (%v)", d.Name, lerr)
			ip, hostname = "", ""
		}
		hosts = append(hosts, hostEntry(d, ip, hostname))
	}
	return finish(stream, false, false, fmt.Sprintf("probed %d VM", len(hosts)), map[string]any{"hosts": hosts})
}

// applyResized moves every machine in the batch to an ABSOLUTE target.
//
// The precheck is the host's free memory and the disk pool's free space against the
// batch's summed POSITIVE delta. It fails closed, before the first machine is
// touched, so a batch never ends up half-resized because the box ran out on VM four
// of six.
//
// ★ Past the precheck the batch is NOT abandoned on the first per-VM error. A
// machine is stopped to have its cpu changed, and returning there leaves it
// stopped with nobody told which one it was. Every machine is attempted, each one
// reports its own outcome in `output.results`, and a machine stopped for an update
// that then failed is started again before moving on.
func (m *VMLocal) applyResized(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	p := params.AsMap()
	f := newFields(p, "")
	ids := f.strList("vm_ids")
	cpu, ramMB, diskGB := f.integer("cpu_cores"), f.integer("ram_mb"), f.integer("disk_gb")
	targetRAM := ramMB * 1024 * 1024
	targetDisk := diskGB * 1024 * 1024 * 1024

	h, err := m.connect(f.str(connEndpoint))
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	defer h.Close()

	namespace := f.str(connNamespace)

	targets := make([]domainInfo, 0, len(ids))
	for _, id := range ids {
		d, lerr := h.LookupDomain(id)
		if lerr != nil {
			return sendFailure(stream, fmt.Sprintf("%s: %v", id, lerr))
		}
		if d.Namespace != namespace {
			return sendFailure(stream, fmt.Sprintf("%s lives in namespace %q, not %q", id, d.Namespace, namespace))
		}
		targets = append(targets, d)
	}

	var ramDelta, diskDelta int64
	for _, d := range targets {
		if targetRAM > d.MemoryBytes {
			ramDelta += targetRAM - d.MemoryBytes
		}
		if targetDisk > d.DiskBytes {
			diskDelta += targetDisk - d.DiskBytes
		}
	}
	if ramDelta > 0 {
		free, ferr := h.FreeMemoryBytes()
		if ferr != nil {
			return sendFailure(stream, fmt.Sprintf("read host free memory: %v", ferr))
		}
		if free < ramDelta {
			return sendFailure(stream, fmt.Sprintf("batch needs %d more bytes of RAM, host has %d free — refused before any VM was touched", ramDelta, free))
		}
	}
	if diskDelta > 0 {
		free, ferr := h.PoolFreeBytes()
		if ferr != nil {
			return sendFailure(stream, fmt.Sprintf("read pool free space: %v", ferr))
		}
		if free < diskDelta {
			return sendFailure(stream, fmt.Sprintf("batch needs %d more bytes of disk, pool has %d free — refused before any VM was touched", diskDelta, free))
		}
	}
	progress(stream, "quota: host has room for the batch's growth")

	results := make([]any, 0, len(targets))
	anyChanged := false
	var problems []string
	for _, d := range targets {
		changed, downtime, rerr := m.resizeOne(ctx, stream, h, d, cpu, targetRAM, targetDisk)
		if rerr != nil {
			problems = append(problems, d.UUID+": "+rerr.Error())
		}
		anyChanged = anyChanged || changed
		results = append(results, resizeResult(d.UUID, changed, downtime, rerr))
	}

	// `output.results` rather than `hosts`: a resize answers about the operation,
	// not about the inventory, and a per-VM outcome is the only honest report for
	// a batch that is attempted machine by machine. The keys are [resizeKeys].
	out := map[string]any{"results": results}
	if len(problems) > 0 {
		return finish(stream, anyChanged, true, strings.Join(problems, "; "), out)
	}
	return finish(stream, anyChanged, false, fmt.Sprintf("%d VM at the requested target", len(targets)), out)
}

// resizeOne applies the target to one machine. Returns whether anything changed,
// whether that cost downtime, and this machine's own error.
func (m *VMLocal) resizeOne(ctx context.Context, stream eventStream, h hypervisor, d domainInfo, cpu, targetRAM, targetDisk int64) (bool, bool, error) {
	changed := false

	// Disk first, and online: it is the one dimension that needs no downtime, so
	// a failure here leaves the machine running and otherwise untouched.
	if targetDisk > 0 {
		if targetDisk <= d.DiskBytes {
			progress(stream, "%s disk already %d bytes, target %d — skipped (shrinking is refused)", d.UUID, d.DiskBytes, targetDisk)
		} else {
			if err := h.ResizeDiskBytes(d.UUID, targetDisk); err != nil {
				return changed, false, fmt.Errorf("resize disk: %w", err)
			}
			changed = true
			progress(stream, "%s disk grown to %d bytes (the guest filesystem follows only if it runs growpart)", d.UUID, targetDisk)
		}
	}

	needCPU := cpu > 0 && cpu != d.VCPUs
	needRAM := targetRAM > 0 && targetRAM != d.MemoryBytes
	if !needCPU && !needRAM {
		return changed, false, nil
	}

	// stop → update → start. libvirt could hot-plug some of this; the artifact does
	// not, and `allow_downtime` is the operator's consent to the stop this actually
	// performs.
	forced, err := h.StopDomain(ctx, d.UUID)
	if err != nil {
		return changed, false, fmt.Errorf("stop: %w", err)
	}
	if forced {
		progress(stream, "%s ignored the ACPI shutdown and was stopped the hard way — an unclean shutdown, "+
			"which stock cloud images make the normal case because they do not act on the power button", d.UUID)
	}

	if needCPU {
		if err := h.SetVCPUs(d.UUID, cpu); err != nil {
			// The machine is stopped and its config unchanged. Bring it back, and
			// report the original error — that is the one that matters.
			m.startBack(stream, h, d.UUID)
			return changed, true, fmt.Errorf("set vcpus: %w", err)
		}
		changed = true
	}
	if needRAM {
		if err := h.SetMemoryBytes(d.UUID, targetRAM); err != nil {
			m.startBack(stream, h, d.UUID)
			return changed, true, fmt.Errorf("set memory: %w", err)
		}
		changed = true
	}
	if err := h.StartDomain(d.UUID); err != nil {
		return changed, true, fmt.Errorf("start after resize: %w", err)
	}
	progress(stream, "%s resized through stop/start", d.UUID)
	return changed, true, nil
}

// startBack returns a machine that was stopped for an update that then failed. A
// failure to start it back is reported and not returned: the update error is the
// one the operator needs first.
func (m *VMLocal) startBack(stream eventStream, h hypervisor, id string) {
	if err := h.StartDomain(id); err != nil {
		progress(stream, "%s could not be started again after the failed update (%v) — it is STOPPED", id, err)
	}
}
