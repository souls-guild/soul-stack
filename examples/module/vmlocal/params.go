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
	"slices"
	"sort"
	"strings"
)

// runLabelKey is the domain-metadata label carrying the batch identity. It is
// what `created` matches its own machines by on a rerun, and what `probed`
// filters on.
const runLabelKey = "soulstack-run"

// userdataMaxBytes bounds what goes onto the NoCloud seed. The ISO would hold
// megabytes; a cloud-init document that large is a rendering accident rather
// than an intention, and it is cheaper to refuse it than to boot a machine that
// then fails to configure itself.
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

// strList reads a list of strings, refusing a bare string. `vm_ids` is declared
// a list; accepting one id unwrapped would make the single-machine case take a
// different code path from every other case, which is where such a path rots.
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
// it: dropping would lose the batch identity when someone writes
// `soulstack-run: 12345`, and a lost identity spawns orphans.
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

// vmProfile is the machine spec: one libvirt domain's worth of decisions, and
// nothing that a libvirt host cannot answer for.
type vmProfile struct {
	namespace    string
	imageID      string
	imageName    string
	networkID    string
	cpuSize      int64
	ramSize      int64 // bytes
	bootDiskSize int64 // bytes
	bootDiskName string

	deletionProtection bool
	labels             map[string]string
	runLabel           string
}

// scope is the namespace a profile places its machines in; the profile wins over
// the step param, so one step can provision into a namespace of the spec's
// choosing without the caller restating it.
func (p vmProfile) scope(stepNS string) string {
	if p.namespace != "" {
		return p.namespace
	}
	return stepNS
}

// profileVocabulary is the CLOSED set of profile keys, and closing it is this
// artifact's own guard on its parameter surface.
//
// ★ It has to live here rather than in the schema document: `profile` is
// declared [module.Map], so param-level strictness (ADR-0076) type-checks
// nothing inside it and an unknown key there is invisible to the platform. An
// ignored key in a machine spec is the worst kind of silence — the operator
// asked for a property, was not refused, and gets a machine without it.
//
// Adding a field means adding it here, which is the point: the set drifts by an
// edit that a reviewer sees, not by a reader quietly starting to read one more
// key.
var profileVocabulary = []string{
	"namespace",
	"image_id", "image_name",
	"network_id",
	"cpu_size", "ram_size",
	"boot_disk_size", "boot_disk_name",
	"deletion_protection",
	"labels",
}

// parseProfile builds the spec and reports everything wrong with it. One
// function, called by both Validate and Apply, so what Validate refuses Apply
// refuses (NIM-786) by construction rather than by two lists agreeing.
func parseProfile(raw map[string]any) (vmProfile, []string) {
	f := newFields(raw, "profile")

	p := vmProfile{
		namespace:          f.str("namespace"),
		imageID:            f.str("image_id"),
		imageName:          f.str("image_name"),
		networkID:          f.str("network_id"),
		cpuSize:            f.integer("cpu_size"),
		ramSize:            f.integer("ram_size"),
		bootDiskSize:       f.integer("boot_disk_size"),
		bootDiskName:       f.str("boot_disk_name"),
		deletionProtection: f.boolean("deletion_protection"),
		labels:             f.labels("labels"),
	}
	p.runLabel = p.labels[runLabelKey]

	unknown := make([]string, 0)
	for k := range raw {
		if !slices.Contains(profileVocabulary, k) {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		f.errs = append(f.errs, fmt.Sprintf("profile.%s is not a vmlocal profile field. A key this artifact does not read would be silently ignored, "+
			"so it is refused instead. The fields are: %s.", k, strings.Join(profileVocabulary, ", ")))
	}

	return p, f.errs
}
