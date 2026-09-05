package config

import "testing"

// `paths:` in soul.yml — the module cache and the SoulSeed directory (NIM-819).
//
// The rule is the keeper side's, verbatim: absolute or refused. A relative value is
// resolved against the process's working directory, and nothing in a soul.yml pins
// that — the same file under systemd without `WorkingDirectory=` and the same file run
// by hand from an operator's shell name two different directories. For `paths.seed`
// the directory holds the mTLS private key, the Keeper CA and the Sigil trust anchor,
// and [seed.Write]'s os.MkdirAll(0o700) leaves an already-existing directory's mode
// alone, so landing in a world-writable one is silent.

func soulBaseWithPaths(pathsBlock string) []byte {
	return []byte(pathsBlock + `keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
}

func TestSoulPaths_RelativeModulesIsRefused(t *testing.T) {
	src := soulBaseWithPaths(`paths:
  modules: modules
  seed: /var/lib/soul-stack/seed
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if !hasCode(diags, "path_not_absolute") {
		dump(t, diags)
		t.Fatal("a relative paths.modules must be refused with path_not_absolute")
	}
	if !hasCodeAt(diags, "path_not_absolute", "$.paths.modules") {
		dump(t, diags)
		t.Error("the diagnostic must point at $.paths.modules")
	}
}

func TestSoulPaths_RelativeSeedIsRefused(t *testing.T) {
	src := soulBaseWithPaths(`paths:
  modules: /var/lib/soul-stack/modules
  seed: seed
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if !hasCode(diags, "path_not_absolute") {
		dump(t, diags)
		t.Fatal("a relative paths.seed must be refused with path_not_absolute")
	}
	if !hasCodeAt(diags, "path_not_absolute", "$.paths.seed") {
		dump(t, diags)
		t.Error("the diagnostic must point at $.paths.seed")
	}
}

// The scenario from the ticket: both relative, both reported. One diagnostic per field
// and not one for the block — an operator fixing `modules` must not have to run the
// validator again to discover `seed`.
func TestSoulPaths_BothRelativeReportBoth(t *testing.T) {
	src := soulBaseWithPaths(`paths:
  modules: modules
  seed: seed
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	var n int
	for _, d := range diags {
		if d.Code == "path_not_absolute" {
			n++
		}
	}
	if n != 2 {
		dump(t, diags)
		t.Fatalf("path_not_absolute diagnostics = %d, want 2 (one per field)", n)
	}
}

// A dot-relative path is the same defect wearing a different spelling: `./modules` is
// still resolved against the working directory. Pinned because "does it start with a
// slash" is the check somebody reaches for when reimplementing this by hand.
func TestSoulPaths_DotRelativeIsRefused(t *testing.T) {
	src := soulBaseWithPaths(`paths:
  modules: ./modules
  seed: /var/lib/soul-stack/seed
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if !hasCode(diags, "path_not_absolute") {
		dump(t, diags)
		t.Fatal("./modules must be refused: it is relative to the working directory")
	}
}

// Absolute passes, and an omitted block stays valid — the empty value already refuses
// by name where it is used (`paths.modules is not set`, seed load → ErrIncomplete), so
// turning "unset" into a schema error here would break every config that relies on it.
func TestSoulPaths_AbsoluteAndOmittedAreValid(t *testing.T) {
	for name, block := range map[string]string{
		"absolute": `paths:
  modules: /var/lib/soul-stack/modules
  seed: /var/lib/soul-stack/seed
`,
		"omitted": "",
		"partial": `paths:
  seed: /var/lib/soul-stack/seed
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags, err := LoadSoulFromBytes("soul.yml", soulBaseWithPaths(block), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadSoulFromBytes: %v", err)
			}
			if hasCode(diags, "path_not_absolute") {
				dump(t, diags)
				t.Fatal("must not be reported as a relative path")
			}
		})
	}
}
