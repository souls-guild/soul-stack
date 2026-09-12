// Typed reads over the param maps, and the VM profile they build.
//
// ★ Every reader here REFUSES a value of the wrong type instead of coercing it
// (NIM-778). That matters more than it looks: `profile` is declared
// [module.Map], so the schema type-checks nothing inside it, and the ten fields
// that decide what machine gets built are exactly the ten the type system does
// not see. A reader that answered 0 for the string "2" would turn a typo into a
// machine with no CPU, and the operator would be told the field was missing.
package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// runLabelKey is the metadata label carrying the batch identity — the same key
// the cloud artifact stamps, because a scenario filtering on it must filter the
// same way here.
const runLabelKey = "soulstack-run"

// userdataMaxBytes mirrors the cloud's declared ci_user_data cap. A seed ISO
// would hold far more; accepting more would green a scenario the cloud refuses.
const userdataMaxBytes = 32 * 1024

// fields reads one param map, accumulating type errors rather than returning
// them one at a time: an operator with three mistyped fields should learn about
// all three, not be walked through them across three runs.
type fields struct {
	m    map[string]any
	path string // "" for step params, "profile" for the nested spec
	errs []string
}

func newFields(m map[string]any, path string) *fields {
	return &fields{m: m, path: path}
}

func (f *fields) key(name string) string {
	if f.path == "" {
		return name
	}
	return f.path + "." + name
}

func (f *fields) reject(name string, v any, want string) {
	f.errs = append(f.errs, fmt.Sprintf("%s must be %s, got %T", f.key(name), want, v))
}

// str returns "" both when the key is absent and when it holds an empty string;
// a caller that needs the difference asks has().
func (f *fields) str(name string) string {
	v, ok := f.m[name]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		f.reject(name, v, "a string")
		return ""
	}
	return s
}

// integer accepts the shapes a param can legitimately arrive in — structpb
// renders every number as float64, YAML gives int/int64/uint64 — and refuses
// everything else, including a float64 with a fractional part.
func (f *fields) integer(name string) int64 {
	v, ok := f.m[name]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			f.errs = append(f.errs, fmt.Sprintf("%s must be a whole number, got %v", f.key(name), n))
			return 0
		}
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case uint64:
		if n > math.MaxInt64 {
			f.errs = append(f.errs, fmt.Sprintf("%s is out of range: %d", f.key(name), n))
			return 0
		}
		return int64(n)
	default:
		f.reject(name, v, "an integer")
		return 0
	}
}

func (f *fields) boolean(name string) bool {
	v, ok := f.m[name]
	if !ok || v == nil {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		f.reject(name, v, "a boolean")
		return false
	}
	return b
}

func (f *fields) has(name string) bool {
	v, ok := f.m[name]
	return ok && v != nil
}

// strList reads a list of strings, refusing a bare string. The cloud contract
// declares vm_ids as a list and a scenario that passes one id unwrapped is a
// scenario that will behave differently in the cloud.
func (f *fields) strList(name string) []string {
	v, ok := f.m[name]
	if !ok || v == nil {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		f.reject(name, v, "a list of strings")
		return nil
	}
	out := make([]string, 0, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok {
			f.errs = append(f.errs, fmt.Sprintf("%s[%d] must be a string, got %T", f.key(name), i, e))
			continue
		}
		if s == "" {
			f.errs = append(f.errs, fmt.Sprintf("%s[%d] must not be empty", f.key(name), i))
			continue
		}
		out = append(out, s)
	}
	return out
}

// labels reads the label map, refusing a non-string value rather than dropping
// it. The cloud artifact drops silently, which loses the batch identity when
// someone writes `soulstack-run: 12345` — and a lost identity spawns orphans.
func (f *fields) labels(name string) map[string]string {
	v, ok := f.m[name]
	if !ok || v == nil {
		return map[string]string{}
	}
	raw, ok := v.(map[string]any)
	if !ok {
		f.reject(name, v, "a map of string to string")
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, ok := raw[k].(string)
		if !ok {
			f.errs = append(f.errs, fmt.Sprintf("%s[%q] must be a string, got %T", f.key(name), k, raw[k]))
			continue
		}
		out[k] = s
	}
	return out
}

// vmProfile is the machine spec, in the cloud artifact's field names.
type vmProfile struct {
	namespace    string
	namespaceID  string
	imageID      string
	imageName    string
	imageVersion int64
	networkID    string
	cpuSize      int64
	ramSize      int64 // bytes
	bootDiskSize int64 // bytes
	bootDiskName string

	deletionProtection bool
	rmExternalID       string
	labels             map[string]string
	runLabel           string
}

// profileScope is the namespace a profile places its machines in; profile wins
// over the step param, as in the cloud.
func (p vmProfile) scope(stepNS string) string {
	if p.namespace != "" {
		return p.namespace
	}
	if p.namespaceID != "" {
		return p.namespaceID
	}
	return stepNS
}

// refusedProfileFields are the fields the cloud accepts that a single libvirt
// host cannot honour. They are REFUSED rather than ignored: each one is a
// promise about the machine, and accepting a promise you do not keep is how a
// second implementation stops being a check on the contract and becomes a way
// around it. Fields that are merely inert here (rm_external_id) are accepted.
var refusedProfileFields = []struct {
	key    string
	reason string
}{
	{"set_external_ip", "vmlocal has no external-address pool; a VM is reachable on its NAT lease only"},
	{"external_ip_id", "vmlocal has no external-address pool; a VM is reachable on its NAT lease only"},
	{"anti_affinity", "vmlocal runs one hypervisor, so machines cannot be spread across hosts"},
	{"cluster", "vmlocal has no cluster placement"},
	// The local catalogue is a storage pool: one volume per image name, so there
	// is no set of versions for a version to pick from. Accepting it and
	// resolving the one volume anyway would be the silent no-op this list exists
	// to prevent — the operator would believe they had pinned something.
	{"image_version", "the local image catalogue is a storage pool with one volume per name, so there are no versions to pin"},
}

// parseProfile builds the spec and reports everything wrong with it. One
// function, called by both Validate and Apply, so what Validate refuses Apply
// refuses (NIM-786) by construction rather than by two lists agreeing.
func parseProfile(raw map[string]any) (vmProfile, []string) {
	f := newFields(raw, "profile")

	p := vmProfile{
		namespace:          f.str("namespace"),
		namespaceID:        f.str("namespace_id"),
		imageID:            f.str("image_id"),
		imageName:          f.str("image_name"),
		imageVersion:       f.integer("image_version"),
		networkID:          f.str("network_id"),
		cpuSize:            f.integer("cpu_size"),
		ramSize:            f.integer("ram_size"),
		bootDiskSize:       f.integer("boot_disk_size"),
		bootDiskName:       f.str("boot_disk_name"),
		deletionProtection: f.boolean("deletion_protection"),
		labels:             f.labels("labels"),
	}
	p.runLabel = p.labels[runLabelKey]

	// rm_external_id is inert here but required, because the cloud requires it:
	// a profile that validates locally and is then refused by the cloud would
	// make this artifact a worse gate than no gate.
	switch v := raw["rm_external_id"].(type) {
	case nil:
		if _, present := raw["rm_external_id"]; !present {
			f.errs = append(f.errs, "profile.rm_external_id is required (Resource Manager id of the owning system: cmdb_system_id as a string, or the legacy numeric id). It is inert on vmlocal and is required anyway, so that a profile accepted here is accepted by the cloud too.")
		} else {
			f.errs = append(f.errs, "profile.rm_external_id must be a non-empty string or a positive number")
		}
	case string:
		if strings.TrimSpace(v) == "" {
			f.errs = append(f.errs, "profile.rm_external_id must be a non-empty string or a positive number")
		}
		p.rmExternalID = v
	case float64:
		if v <= 0 || v != math.Trunc(v) {
			f.errs = append(f.errs, "profile.rm_external_id must be a non-empty string or a positive number")
		} else {
			p.rmExternalID = fmt.Sprintf("%d", int64(v))
		}
	case int, int64, uint64:
		p.rmExternalID = fmt.Sprintf("%d", v)
	default:
		f.errs = append(f.errs, fmt.Sprintf("profile.rm_external_id must be a non-empty string or a positive number, got %T", v))
	}

	for _, r := range refusedProfileFields {
		if !f.has(r.key) {
			continue
		}
		// A false, empty or zero value asks for nothing, so there is nothing to
		// refuse. A scenario that carries the key with its default still runs.
		if b, ok := raw[r.key].(bool); ok && !b {
			continue
		}
		if s, ok := raw[r.key].(string); ok && s == "" {
			continue
		}
		if n, ok := raw[r.key].(float64); ok && n == 0 {
			continue
		}
		f.errs = append(f.errs, fmt.Sprintf("profile.%s is not supported by vmlocal: %s. Remove it, or run this scenario against the cloud artifact.", r.key, r.reason))
	}

	return p, f.errs
}
