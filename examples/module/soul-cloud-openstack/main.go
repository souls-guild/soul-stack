// soul-cloud-openstack is a real Soul Stack CloudDriver plugin for OpenStack
// (ADR-016 Phase 4 cloud parity; rollout by the soul-cloud-aws pattern).
//
// Builds into the static binary `soul-cloud-openstack`. The Keeper-side
// `core.cloud.provisioned` module (ADR-017) starts it as a sub-process, performs
// the gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPCs.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the secret from
// Vault and places plain values in CreateRequest.credentials /
// DestroyRequest.credentials; the driver does NOT call Vault. Auth form is
// Keystone v3 password-scoped (auth_url + username + password + project +
// user/project domain), region is optional (private clouds without regions).
// Cloud-init userdata is passed into servers.CreateOpts.UserData (gophercloud
// base64-encodes it itself, unlike the EC2 driver).
//
// Shared framework (error taxonomy / wait-until-ready / retry-backoff) comes
// from sdk/clouddriver and is common to all rollout drivers. Provider-specific
// pieces here are only compute/v2/servers calls and the OpenStack error
// classifier (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded next
// to the binary. Same technique as in soul-cloud-aws - a separate file, not
// hardcoded in Go (easier to keep in sync with manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runMetaKey is the server.metadata idempotency key: value = run identifier
// (from profile.labels or CreateRequest.name as fallback). The name has no colon
// (some OpenStack installations trim `:` in metadata keys). A repeated Create
// with the same value reuses live VMs (BUILD/ACTIVE/REBUILD) instead of creating
// duplicates (NIM-16).
const runMetaKey = "soulstack-run"

// Active/transitional OpenStack instance statuses where a VM is considered
// "live" for idempotency and list filtering. Terminal statuses (ERROR/DELETED/
// SHUTOFF) are excluded: the driver either marks them failed (probe Err) or
// ignores them when searching for an idempotency conflict.
const (
	statusActive  = "ACTIVE"
	statusBuild   = "BUILD"
	statusRebuild = "REBUILD"
)

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry
// phases. It is a variable so L0 tests can replace it (fast MaxAttempts, short
// delays) without raising the 1s->2s->4s timer. Same technique as `newOsClient`
// (see osapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// OpenstackDriver implements CloudDriver for OpenStack.
type OpenstackDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema.
func (o *OpenstackDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	var raw map[string]any
	if err := json.Unmarshal(profileSchemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse embedded schema.json: %w", err)
	}
	s, err := structpb.NewStruct(raw)
	if err != nil {
		return nil, fmt.Errorf("encode profile_schema: %w", err)
	}
	return &pluginv1.SchemaReply{ProfileSchema: s}, nil
}

// Validate carries no credentials (ValidateProfileRequest), so structural checks
// are here; auth happens in Create. region is NOT required - private clouds
// without regions run Keystone without `RegionName` in the catalog.
func (o *OpenstackDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "image_id") == "" {
		errs = append(errs, "profile.image_id is required")
	}
	if stringField(p, "flavor_id") == "" {
		errs = append(errs, "profile.flavor_id is required")
	}
	if stringField(p, "network_id") == "" {
		errs = append(errs, "profile.network_id is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile params parsed for servers.Create.
type vmProfile struct {
	region           string
	availabilityZone string
	imageID          string
	flavorID         string
	networkID        string
	keyName          string
	securityGroups   []string
	labels           map[string]string
	runLabel         string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		region:           stringField(p, "region"),
		availabilityZone: stringField(p, "availability_zone"),
		imageID:          stringField(p, "image_id"),
		flavorID:         stringField(p, "flavor_id"),
		networkID:        stringField(p, "network_id"),
		keyName:          stringField(p, "key_name"),
		labels:           map[string]string{},
	}
	if sgs, ok := p["security_groups"].([]any); ok {
		for _, sg := range sgs {
			if s, ok := sg.(string); ok {
				prof.securityGroups = append(prof.securityGroups, s)
			}
		}
	}
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runLabel = prof.labels[runMetaKey]
	return prof
}

// Create: fail-closed without identity (no name and no run label - otherwise
// rerun creates orphan VMs, NIM-16) -> idempotency scan of live VMs by
// metadata[runMetaKey] -> fill missing instances with first free indexes ->
// wait-until-ready -> final event with VmInfo (fqdn = server.AccessIPv4/floating
// IP / first address from server.Addresses; if none exists, Fqdn is empty and
// attributes carry no_address - Keeper-side will see it and decide). name without
// a label becomes the run label (metadata stamp).
func (o *OpenstackDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	if creds.Region == "" {
		creds.Region = prof.region
	}

	// Fail-closed: without name and run label the run is indistinguishable from
	// previous ones -> idempotency scan is impossible, rerun would create orphan
	// VMs (NIM-16). Guard is before newOsClient - Keystone auth is already an
	// OpenStack API call.
	nameBase := req.GetName()
	if nameBase == "" && prof.runLabel == "" {
		return sendCreateFailed(stream, clouddriver.FailInvalidParams, "identity",
			fmt.Errorf("no run identity: set step param `name` or profile.labels[%q]", runMetaKey))
	}
	// name without an explicit label becomes the run label -> created VMs always
	// carry metadata[runMetaKey], future runs match on it.
	if prof.runLabel == "" {
		prof.runLabel = nameBase
	}

	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "os-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency (NIM-16): always scan (runLabel is non-empty after the guard) -
	// live VMs from the run are reused, and only missing ones are added.
	existing, err := o.findByRunLabel(ctx, cli, backoff, prof.runLabel)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyOS, err), "list-existing", err)
	}
	if int32(len(existing)) >= count {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runLabel),
		})
		return o.finalizeCreate(ctx, cli, stream, backoff, serverIDs(existing))
	}

	need := count - int32(len(existing))
	names := gapFillNames(existing, nameBase, prof.runLabel, need)
	if len(existing) > 0 {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: reusing %d existing VM for run %q, creating %d more", len(existing), prof.runLabel, need),
		})
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("servers.Create count=%d flavor=%s image=%s", need, prof.flavorID, prof.imageID),
	}); err != nil {
		return err
	}

	newServers, err := o.createServers(ctx, cli, backoff, prof, req.GetUserdata(), names)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyOS, err), "servers.Create", err)
	}

	allIDs := append(serverIDs(existing), serverIDs(newServers)...)
	return o.finalizeCreate(ctx, cli, stream, backoff, allIDs)
}

// createServers calls servers.Create once for each name (OpenStack creates one
// server per call; there is no batch Create like EC2.RunInstances). Names arrive
// ready from gap-fill - deterministic and without collisions with existing VMs
// (NIM-16).
func (o *OpenstackDriver) createServers(ctx context.Context, cli osAPI, backoff clouddriver.BackoffConfig, prof vmProfile, userdata string, names []string) ([]servers.Server, error) {
	out := make([]servers.Server, 0, len(names))
	for _, name := range names {
		opts := buildCreateOpts(prof, userdata, name)
		var srv *servers.Server
		err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
			var rerr error
			srv, rerr = cli.CreateServer(ctx, opts)
			return rerr
		})
		if err != nil {
			return out, err
		}
		if srv != nil {
			out = append(out, *srv)
		}
	}
	return out, nil
}

// buildCreateOpts forms servers.CreateOpts with the ready VM name
// (deterministic `<nameBase>-<seq>` / `soul-<runLabel>-<seq>` from gap-fill).
//
// Userdata: gophercloud servers.CreateOpts encodes UserData in base64 ITSELF
// (see servers.CreateOpts.ToServerCreateMap -> base64.StdEncoding.EncodeToString).
// The driver puts a plain []byte cloud-init blob - unlike the EC2 driver, where
// the SDK does NOT encode and author does base64.
func buildCreateOpts(prof vmProfile, userdata, name string) servers.CreateOpts {
	metadata := make(map[string]string, len(prof.labels)+1)
	for k, v := range prof.labels {
		metadata[k] = v
	}
	if prof.runLabel != "" {
		metadata[runMetaKey] = prof.runLabel
	}
	opts := servers.CreateOpts{
		Name:             name,
		FlavorRef:        prof.flavorID,
		ImageRef:         prof.imageID,
		AvailabilityZone: prof.availabilityZone,
		Networks:         []servers.Network{{UUID: prof.networkID}},
		SecurityGroups:   prof.securityGroups,
		Metadata:         metadata,
	}
	if userdata != "" {
		opts.UserData = []byte(userdata)
	}
	return opts
}

// vmName is the deterministic name of the i-th VM in the batch (NIM-16, a pure
// function without a time component; otherwise idempotency scan would not find
// its VMs): nameBase (CreateRequest.name, self-onboard "Variant T" ADR-017(h))
// -> `<nameBase>-<seq>` (keeper predicted FQDN and baked a per-VM token under it
// - the name MUST match); otherwise `soul-<runLabel>-<seq>`.
func vmName(nameBase, runLabel string, seq int32) string {
	if nameBase != "" {
		return fmt.Sprintf("%s-%d", nameBase, seq)
	}
	return fmt.Sprintf("soul-%s-%d", runLabel, seq)
}

// gapFillNames returns need names for missing VMs by taking the first free
// indexes seq=0,1,2,... (occupied = existing names) -> on partial rerun, new VMs
// do not collide by name with existing VMs (NIM-16). Occupancy is computed from
// live existing VMs: an index of a VM in DELETING can be reused - nova requiring
// unique names will answer Conflict loudly and without orphans (default nova
// allows duplicate names; distinction is still by metadata[runMetaKey]).
func gapFillNames(existing []servers.Server, nameBase, runLabel string, need int32) []string {
	occupied := make(map[string]bool, len(existing))
	for _, s := range existing {
		occupied[s.Name] = true
	}
	names := make([]string, 0, need)
	for seq := int32(0); int32(len(names)) < need; seq++ {
		n := vmName(nameBase, runLabel, seq)
		if occupied[n] {
			continue
		}
		names = append(names, n)
	}
	return names
}

// finalizeCreate waits for VM readiness (ACTIVE + IP) and sends the final event.
// Anti-orphan: on ctx-cancel/timeout, unfinished VMs go into the final event with
// failed=true but populated vm_id - Keeper can Destroy them.
func (o *OpenstackDriver) finalizeCreate(ctx context.Context, cli osAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, vmIDs []string) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		srv, perr := cli.GetServer(pctx, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyOS, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		switch srv.Status {
		case statusActive:
			if primaryAddress(srv) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			// ACTIVE without an address - keep waiting (Neutron may not have bound
			// the port yet). Not Ready and not Err: poller will retry.
			return clouddriver.ProbeResult{}
		case statusBuild, statusRebuild:
			return clouddriver.ProbeResult{}
		default:
			// ERROR / SHUTOFF / DELETED / PAUSED / SUSPENDED / … — terminal.
			return clouddriver.ProbeResult{Err: fmt.Errorf("server %s entered terminal status %q", vmID, srv.Status)}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if srv, derr := cli.GetServer(ctx, wr.VMID); derr == nil {
				vi.Fqdn = preferredFqdn(srv)
				vi.PrimaryIp = primaryAddress(srv)
				vi.Attributes = serverAttributes(srv)
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		// ctx-cancel / deadline: final event CARRIES vm_id of all created VMs with
		// failed=true (anti-orphan) - Keeper sees instance-id and can Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyOS, waitErr), "wait-until-ready", waitErr),
			Vms:     vms,
			Failed:  true,
		})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: "completed",
		Vms:     vms,
		Failed:  anyFailed,
	})
}

// findByRunLabel lists live (ACTIVE/BUILD/REBUILD) VMs with the given runLabel
// in the current project. servers.List supports metadata filtering through
// ListOpts.Metadata in newer APIs; as a portable fallback, fetch everything and
// filter afterward on the Python side.
//
// Unlike the wb driver, there is no name-match branch: openstack has no pre-fix
// unlabeled VMs with deterministic names (anon names were random).
//
// PageSize is not set - gophercloud Pager pages by itself. AllTenants=false: the
// driver lives in the scope of one project (by credentials).
func (o *OpenstackDriver) findByRunLabel(ctx context.Context, cli osAPI, backoff clouddriver.BackoffConfig, runLabel string) ([]servers.Server, error) {
	var all []servers.Server
	err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
		var rerr error
		all, rerr = cli.ListServers(ctx, servers.ListOpts{})
		return rerr
	})
	if err != nil {
		return nil, err
	}
	live := make([]servers.Server, 0, len(all))
	for _, s := range all {
		if !isLiveStatus(s.Status) {
			continue
		}
		if s.Metadata[runMetaKey] != runLabel {
			continue
		}
		live = append(live, s)
	}
	return live, nil
}

func isLiveStatus(status string) bool {
	switch status {
	case statusActive, statusBuild, statusRebuild:
		return true
	}
	return false
}

// Destroy: per-VM servers.Delete, stream per-VM events. OpenStack DeleteServer
// is not batched, so we iterate the list; 404 (not_found) is idempotent success.
func (o *OpenstackDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "os-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, id := range req.GetVmIds() {
		vmID := id
		err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
			return cli.DeleteServer(ctx, vmID)
		})
		if err == nil {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "deleted"})
			continue
		}
		class := clouddriver.Classify(classifyOS, err)
		if class == clouddriver.FailNotFound {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "already absent"})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{
			VmId:    vmID,
			Message: clouddriver.FailMessage(class, "servers.Delete", err),
			Failed:  true,
		})
	}
	return nil
}

// Status polls one VM (servers.Get). credentials come in a separate
// StatusRequest.credentials field (A-flow, symmetrical with Create/Destroy).
func (o *OpenstackDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("os-client: %w", err)
	}
	srv, err := cli.GetServer(ctx, req.GetVmId())
	if err != nil {
		return nil, fmt.Errorf("servers.Get %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      srv.Status,
		Attributes: serverAttributes(srv),
	}, nil
}

// List streams VM inventory in the project (optionally filtered by runLabel).
// credentials come in a separate ListRequest.credentials field (A-flow,
// symmetrical with Create/Destroy/Status).
func (o *OpenstackDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("os-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	runLabel := stringField(filter, runMetaKey)

	all, err := cli.ListServers(ctx, servers.ListOpts{})
	if err != nil {
		return fmt.Errorf("servers.List: %w", err)
	}
	for _, s := range all {
		if runLabel != "" && s.Metadata[runMetaKey] != runLabel {
			continue
		}
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       s.ID,
			Fqdn:       preferredFqdn(&s),
			PrimaryIp:  primaryAddress(&s),
			Attributes: serverAttributes(&s),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&OpenstackDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-openstack:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

func serverIDs(ss []servers.Server) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

// preferredFqdn decides what to use as Fqdn for a Soul host. OpenStack does not
// guarantee FQDN in the API (server.Name is a short name, not DNS name), so the
// fqdn canon is:
//  1. AccessIPv4 (if floating IP is set) - externally addressable name/IP;
//  2. primaryAddress() - first IPv4 from server.Addresses, internal network;
//  3. empty - Keeper-side decides (provisioned error is possible if IP did not
//     appear; documented in manifest profile_schema).
func preferredFqdn(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if s.AccessIPv4 != "" {
		return s.AccessIPv4
	}
	return primaryAddress(s)
}

// primaryAddress returns the first IPv4 from server.Addresses. Addresses shape in
// gophercloud is map[net-name][]Address; here plain IPv4 is needed, not floating
// (AccessIPv4 is used for floating). Network name is chosen deterministically by
// sorting keys (stable inventory output).
func primaryAddress(s *servers.Server) string {
	if s == nil || len(s.Addresses) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.Addresses))
	for k := range s.Addresses {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, net := range keys {
		addrs, ok := s.Addresses[net].([]any)
		if !ok {
			continue
		}
		for _, a := range addrs {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			ver, _ := am["version"].(float64)
			if ver != 4 {
				continue
			}
			if addr, ok := am["addr"].(string); ok && addr != "" {
				return addr
			}
		}
	}
	return ""
}

// sortStrings is compact sorting (without depending on sort.Strings so module
// helpers stay flat; n here is the number of networks for one VM, usually 1-3,
// O(n^2) is safe).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func serverAttributes(s *servers.Server) *structpb.Struct {
	if s == nil {
		return nil
	}
	m := map[string]any{
		"status":   s.Status,
		"flavor":   flavorRef(s),
		"image":    imageRef(s),
		"hostname": s.Name,
	}
	if !s.Created.IsZero() {
		m["created_at"] = s.Created.UTC().Format(time.RFC3339)
	}
	if s.AccessIPv4 != "" {
		m["access_ipv4"] = s.AccessIPv4
	}
	if primaryAddress(s) == "" {
		m["no_address"] = true
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return st
}

// flavorRef / imageRef: gophercloud parses Flavor/Image as map[string]any (Nova
// returns either a string ID or an object with href). The driver reduces this to
// flat "id" for attributes.
func flavorRef(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if id, ok := s.Flavor["id"].(string); ok {
		return id
	}
	return ""
}

func imageRef(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if id, ok := s.Image["id"].(string); ok {
		return id
	}
	return ""
}
