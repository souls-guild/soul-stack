package pluginhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Discovered is one ADDRESSABLE MODULE in the host cache — not one artifact.
//
// An artifact serves several modules (`acl`, `config`, `info`) and the host picks one
// per spawn, so the unit that can be looked up, capability-checked, disclosed and
// spawned is the module, not the file. Discovery therefore yields one entry per module
// of every slot it reads: the artifact fields (Doc, BinaryPath, Dir, Digest) repeat
// across the entries of one artifact, and [Discovered.Module] is what differs — and
// what the host passes as argv.
//
// Address level 1 is [Discovered.Alias], the slot directory's name, chosen by the
// operator at registration. The artifact does not know it and carries no self-name, so
// the same bytes registered as `redis` and as `redis-community` yield two address
// spaces that cannot collide.
type Discovered struct {
	// Alias is address level 1 — the registration alias, taken from the slot, never
	// from the artifact.
	Alias string
	// Module is address level 2 — the module this entry addresses, and the argv the
	// host passes the artifact at spawn. Empty for the kinds that serve a single
	// endpoint (ssh_provider / soul_beacon), which declare no modules.
	Module string
	// Doc is the schema document read from the artifact's trailer — the whole
	// artifact's, shared by every entry that came out of the same slot. Read THIS
	// entry's module through [Discovered.ModuleDef], never the document's other
	// modules.
	Doc *sharedplugin.Document
	// BinaryPath is the absolute path to the artifact.
	BinaryPath string
	// Dir is the slot directory holding the artifact (for logs and the digest
	// sidecar).
	Dir string
	// Digest is the artifact's SHA-256 (hex), computed during Discover. Used for
	// logs/OTel attributes; the authoritative integrity check happens in
	// [Host.Spawn] against the approved digest in the Sigil grant.
	Digest string
}

// Address is `<alias>.<module>` — the key a registry stores this entry under, and what
// a rendered task carries before its state suffix. A single-endpoint kind has no
// module, so its address is the bare alias.
func (d Discovered) Address() string {
	if d.Module == "" {
		return d.Alias
	}
	return d.Alias + "." + d.Module
}

// Kind is the contract the artifact implements. Every module in one artifact shares
// it — a bundle serves one contract, never a mix.
func (d Discovered) Kind() sharedplugin.Kind {
	if d.Doc == nil {
		return ""
	}
	return d.Doc.Kind
}

// ModuleDef is THIS entry's module declaration. ok is false for a single-endpoint kind
// and for a module the document does not declare.
func (d Discovered) ModuleDef() (sharedplugin.ModuleDef, bool) {
	if d.Doc == nil || d.Module == "" {
		return sharedplugin.ModuleDef{}, false
	}
	return d.Doc.Module(d.Module)
}

// Capabilities is what THIS module declares — never the union across the artifact.
// Spawning `acl` must not carry, or get approved for, what `config` touches.
//
// This is disclosure to the operator before approval, not a control: nothing confines
// the process afterwards (ADR-020(g), ADR-026(c) as corrected by NIM-377).
func (d Discovered) Capabilities() []sharedplugin.Capability {
	m, ok := d.ModuleDef()
	if !ok {
		return nil
	}
	return m.Capabilities
}

// SideEffects is what THIS module touches, under the same rule as
// [Discovered.Capabilities]: one module's footprint, never the artifact's.
func (d Discovered) SideEffects() []sharedplugin.SideEffect {
	m, ok := d.ModuleDef()
	if !ok {
		return nil
	}
	return m.SideEffects
}

// Discover reads every slot under cacheRoot.
//
// Host cache layout (docs/soul/modules.md, docs/keeper/plugins.md):
//
//	<cacheRoot>/
//	  <alias>/
//	    <artifact>        # exactly one executable, schema in its trailer
//	    .sha256           # digest sidecar, written on first spawn
//
// The slot directory's name is the registration alias. There is no filename convention
// for the artifact: it carries no self-name, so the host takes the slot's single
// executable and reads what it offers from the trailer.
//
// Discover **does not filter by kind** — that is the caller's job (the Soul host
// accepts soul_module and soul_beacon, the Keeper host ssh_provider and soul_module).
// See [FilterByKinds].
//
// A slot that cannot be read — no executable, several executables, no trailer, a
// malformed trailer, an invalid document — goes into warnings and is skipped, without
// stopping the walk. The only fatal error is failing to read cacheRoot itself.
func Discover(cacheRoot string) ([]Discovered, []string, error) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read plugin cache root %q: %w", cacheRoot, err)
	}
	var (
		out      []Discovered
		warnings []string
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found, warns := DiscoverSlot(e.Name(), filepath.Join(cacheRoot, e.Name()))
		out = append(out, found...)
		warnings = append(warnings, warns...)
	}
	return out, warnings, nil
}

// DiscoverSlot reads ONE slot directory and returns an entry per module the artifact
// serves (exactly one for a single-endpoint kind), plus warnings.
//
// alias is passed rather than derived from dir because the two need not match: the
// Keeper host reaches an active slot through a `current` symlink whose basename says
// nothing about the registration (A1-S1), and address level 1 must come from the
// registration either way.
//
// # Fail closed
//
// A missing or malformed trailer is a skip with a warning, never a fallback to a
// sibling `schema.json` and never an empty document. The schema is the disclosure an
// operator approved; an artifact whose disclosure cannot be read has not been
// approved, and a host that guessed would be running code nobody agreed to.
func DiscoverSlot(alias, dir string) ([]Discovered, []string) {
	skip := func(format string, args ...any) []string {
		return []string{fmt.Sprintf("skip %s: %s", dir, fmt.Sprintf(format, args...))}
	}

	binPath, err := artifactIn(dir)
	if err != nil {
		return nil, skip("%v", err)
	}
	digest, err := computeFileDigest(binPath)
	if err != nil {
		return nil, skip("digest %s: %v", filepath.Base(binPath), err)
	}
	doc, diags, err := sharedplugin.ReadArtifact(binPath)
	if err != nil {
		return nil, skip("%v", err)
	}
	if derr := sharedplugin.FirstError(diags); derr != nil {
		return nil, skip("%v", derr)
	}
	if doc == nil {
		// ReadArtifact reports this through diags as well; checking both channels is
		// what makes a caller that forgets one still fail closed.
		return nil, skip("artifact carries no readable schema document")
	}

	base := Discovered{Alias: alias, Doc: doc, BinaryPath: binPath, Dir: dir, Digest: digest}
	if doc.Kind != sharedplugin.KindSoulModule {
		return []Discovered{base}, nil
	}
	if len(doc.Modules) == 0 {
		// The validator already rejects this; the second check costs nothing and
		// keeps a soul_module slot from registering as an unaddressable entry.
		return nil, skip("kind=soul_module artifact declares no modules")
	}
	out := make([]Discovered, 0, len(doc.Modules))
	for _, m := range doc.Modules {
		entry := base
		entry.Module = m.Name
		out = append(out, entry)
	}
	return out, nil
}

// artifactIn returns the slot's single executable.
//
// There is no name to look up — the artifact declares none — so the rule is
// arithmetic: exactly one executable in the slot. None is an empty slot; more than one
// is ambiguous, and picking one would let directory listing order decide which code
// runs. Dot-files are ignored: that is the digest sidecar and the temp files its
// atomic write leaves behind.
func artifactIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// os.Stat, not the DirEntry: a slot may reach its artifact through a
		// symlink, and lstat would report the link rather than what it points at.
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		found = append(found, name)
	}
	switch len(found) {
	case 1:
		return filepath.Join(dir, found[0]), nil
	case 0:
		return "", errors.New("no executable artifact in the slot")
	default:
		sort.Strings(found)
		return "", fmt.Errorf("slot holds %d executables (%s), a slot holds exactly one artifact",
			len(found), strings.Join(found, ", "))
	}
}

// FilterByKinds keeps only entries whose kind is in allowedKinds. Rejected entries go
// into warnings with a human-readable message. Returns (filtered list, warnings).
//
// Convenient right after [Discover]:
//
//	found, w1, err := pluginhost.Discover(root)
//	found, w2 := pluginhost.FilterByKinds(found, []sharedplugin.Kind{sharedplugin.KindSoulModule})
//	warnings := append(w1, w2...)
func FilterByKinds(discovered []Discovered, allowedKinds []sharedplugin.Kind) ([]Discovered, []string) {
	if len(allowedKinds) == 0 {
		return discovered, nil
	}
	allowed := make(map[sharedplugin.Kind]struct{}, len(allowedKinds))
	for _, k := range allowedKinds {
		allowed[k] = struct{}{}
	}
	var (
		out      = make([]Discovered, 0, len(discovered))
		warnings []string
	)
	for _, d := range discovered {
		if _, ok := allowed[d.Kind()]; ok {
			out = append(out, d)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("skip %s: kind=%q not allowed on this host (want %v)",
			d.Dir, d.Kind(), allowedKinds))
	}
	return out, warnings
}
