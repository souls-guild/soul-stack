package plugin

// Guard for the two proto numbers the CloudDriver contract left behind (NIM-761).
//
// Removing a kind from a wire enum is not the same as freeing its number. An
// already-built third-party plugin still sends `KIND_CLOUD_DRIVER = 2` in its
// handshake, and a `PluginManifest` written before the removal still carries
// `cloud_driver` in field 8. Reusing either number would make one of those old
// messages decode as something else entirely — the failure ADR-012's "never reuse a
// field number" rule exists to prevent, and the reason both are `reserved` rather than
// deleted.
//
// `reserved` is a compile-time promise inside protoc, so nothing in Go can break it —
// which is exactly why it needs a test: deleting the `reserved` line is a one-character
// edit that no build, no vet and no other test would notice, and the number would then
// be quietly available to the next kind somebody adds.
//
// The subject is the `.proto` source, not the generated Go: the generated file has no
// trace of a reserved number at all.
//
// The search is scoped to the DECLARING BLOCK, not the file. `common.proto` holds three
// enums and `manifest.proto` five messages, so a file-wide regex would go on passing
// after somebody moved `reserved 2;` out of `enum Kind` and into `enum Capability` —
// which frees the number in the one place it mattered while still reading as protected.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// reservedCase — one number that must stay unusable, and where it is written down.
type reservedCase struct {
	file   string
	block  string // the `enum X {` / `message X {` line the reservation must sit inside
	number string
	name   string
	was    string
}

var reservedCases = []reservedCase{
	{
		file:   "../../proto/plugin/v1/common.proto",
		block:  "enum Kind {",
		number: "2",
		name:   "KIND_CLOUD_DRIVER",
		was:    "enum Kind",
	},
	{
		file:   "../../proto/plugin/v1/manifest.proto",
		block:  "message Manifest {",
		number: "8",
		name:   "cloud_driver",
		was:    "the Manifest.spec oneof",
	},
}

func TestRemovedCloudDriverNumbersStayReserved(t *testing.T) {
	for _, c := range reservedCases {
		t.Run(c.name, func(t *testing.T) {
			b, err := os.ReadFile(c.file)
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			src := blockBody(t, string(b), c)

			numRe := regexp.MustCompile(`(?m)^\s*reserved\s+` + regexp.QuoteMeta(c.number) + `\s*;`)
			if !numRe.MatchString(src) {
				t.Errorf("%s: `reserved %s;` is gone. %s freed the number that %s used to hold, so a "+
					"plugin built before NIM-761 would decode its own handshake as whatever takes it next.",
					c.file, c.number, c.was, c.name)
			}

			nameRe := regexp.MustCompile(`(?m)^\s*reserved\s+"` + regexp.QuoteMeta(c.name) + `"\s*;`)
			if !nameRe.MatchString(src) {
				t.Errorf("%s: `reserved \"%s\";` is gone. The number alone does not stop the NAME coming "+
					"back on a different number, which is the other half of the same confusion.",
					c.file, c.name)
			}

			// The removal itself, not only its bookkeeping: a `reserved` line beside a live
			// declaration of the same name is worse than neither, because it reads as done.
			liveRe := regexp.MustCompile(`(?m)^\s*(` + regexp.QuoteMeta(c.name) + `\s*=|\w+\s+` + regexp.QuoteMeta(c.name) + `\s*=)`)
			if liveRe.MatchString(src) {
				t.Errorf("%s: %s is declared again despite being reserved — the CloudDriver contract is "+
					"back in the wire format (NIM-761 removed it).", c.file, c.name)
			}
		})
	}
}

// blockBody returns the source of the one enum/message the reservation belongs to,
// from its opening line to the matching close at the same indentation. Everything the
// test asserts is then about that block alone.
func blockBody(t *testing.T, src string, c reservedCase) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == c.block {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no `%s` in the file — the declaration this guard is about was renamed or "+
			"removed, and every assertion below would be about the wrong block", c.file, c.block)
	}
	indent := strings.Repeat(" ", len(lines[start])-len(strings.TrimLeft(lines[start], " ")))
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == indent+"}" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatalf("%s: `%s` is never closed at its own indentation; the block cannot be delimited", c.file, c.block)
	return ""
}
