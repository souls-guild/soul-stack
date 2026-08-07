package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

func testDocument(description string) schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Compat:          schema.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []schema.Module{{
			Name:        "acl",
			Description: description,
			States: map[string]schema.State{
				"present": {Description: "The ACL user exists", Input: schema.Input{
					"host": {Type: schema.String, Required: true},
				}},
			},
		}},
	}
}

func canonicalBytes(t *testing.T, doc schema.Document) []byte {
	t.Helper()
	raw, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

// fixedDeriver stands in for running the artifact: it returns whatever the test says
// the code currently declares.
func fixedDeriver(payload []byte) deriver {
	return func(string) ([]byte, error) { return payload, nil }
}

func writeArtifact(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "soul-mod-redis")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

func TestStamp_WritesTrailerAndSchemaFile(t *testing.T) {
	doc := canonicalBytes(t, testDocument("Redis ACL users"))
	artifact := writeArtifact(t, "pretend this is an ELF")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(doc)); code != exitOK {
		t.Fatalf("stamp exit %d, stderr: %s", code, stderr.String())
	}

	stamped, err := schema.ReadTrailerFile(artifact)
	if err != nil {
		t.Fatalf("ReadTrailerFile: %v", err)
	}
	if !bytes.Equal(stamped, doc) {
		t.Fatalf("trailer payload: got %s want %s", stamped, doc)
	}
	published, err := os.ReadFile(filepath.Join(filepath.Dir(artifact), schema.SchemaFileName))
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	if !bytes.Equal(published, doc) {
		t.Fatalf("schema.json must be byte-identical to the trailer:\ngot  %s\nwant %s", published, doc)
	}

	// And the round trip closes: verify accepts what stamp produced.
	if code := run([]string{"verify", artifact}, &stdout, &stderr, fixedDeriver(doc)); code != exitOK {
		t.Fatalf("verify after stamp: exit %d, stderr: %s", code, stderr.String())
	}
}

func TestVerify_FailsWhenTheCodeMovedOn(t *testing.T) {
	// The gate the whole scheme rests on: an author edits a module.Def, forgets to
	// re-stamp, and everything downstream would otherwise trust a description of a
	// module that no longer exists.
	stampedDoc := canonicalBytes(t, testDocument("Redis ACL users"))
	artifact := writeArtifact(t, "pretend this is an ELF")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(stampedDoc)); code != exitOK {
		t.Fatalf("stamp exit %d, stderr: %s", code, stderr.String())
	}

	changed := canonicalBytes(t, testDocument("Redis ACL users and rules"))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", artifact}, &stdout, &stderr, fixedDeriver(changed)); code == exitOK {
		t.Fatal("verify accepted a stamp that disagrees with the code")
	}
	if !strings.Contains(stderr.String(), "does not match the code") {
		t.Fatalf("stderr should say what is wrong, got: %s", stderr.String())
	}
}

func TestVerify_FailsWhenSchemaFileDisagrees(t *testing.T) {
	doc := canonicalBytes(t, testDocument("Redis ACL users"))
	artifact := writeArtifact(t, "pretend this is an ELF")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(doc)); code != exitOK {
		t.Fatalf("stamp exit %d, stderr: %s", code, stderr.String())
	}
	stale := canonicalBytes(t, testDocument("something else entirely"))
	jsonPath := filepath.Join(filepath.Dir(artifact), schema.SchemaFileName)
	if err := os.WriteFile(jsonPath, stale, 0o644); err != nil {
		t.Fatalf("write stale schema.json: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", artifact}, &stdout, &stderr, fixedDeriver(doc)); code == exitOK {
		t.Fatal("verify accepted a published schema.json that disagrees with the artifact")
	}
	if !strings.Contains(stderr.String(), schema.SchemaFileName) {
		t.Fatalf("stderr should name the file, got: %s", stderr.String())
	}
}

func TestVerify_FailsClosedOnMissingOrCorruptTrailer(t *testing.T) {
	doc := canonicalBytes(t, testDocument("Redis ACL users"))

	t.Run("never_stamped", func(t *testing.T) {
		artifact := writeArtifact(t, "an artifact nobody stamped")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"verify", artifact}, &stdout, &stderr, fixedDeriver(doc)); code == exitOK {
			t.Fatal("verify accepted an unstamped artifact")
		}
	})

	t.Run("trailer_truncated", func(t *testing.T) {
		artifact := writeArtifact(t, "an artifact")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(doc)); code != exitOK {
			t.Fatalf("stamp exit %d", code)
		}
		body, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatalf("read artifact: %v", err)
		}
		if err := os.WriteFile(artifact, body[:len(body)-4], 0o755); err != nil {
			t.Fatalf("truncate artifact: %v", err)
		}
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"verify", artifact}, &stdout, &stderr, fixedDeriver(doc)); code == exitOK {
			t.Fatal("verify accepted a truncated trailer")
		}
	})
}

func TestStamp_RefusesAnInvalidOrNonCanonicalSchema(t *testing.T) {
	tests := map[string]struct {
		payload []byte
		want    string
	}{
		"empty": {
			payload: nil,
			want:    "printed nothing",
		},
		"not_json": {
			payload: []byte("panic: something went wrong"),
			want:    "parse document",
		},
		"invalid_document": {
			payload: canonicalBytes(t, schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1}),
			want:    "modules_empty",
		},
		"not_canonical": {
			payload: []byte(`{"protocol_version": 1, "kind": "soul_module", ` +
				`"modules": [{"name": "acl", "states": {"present": {"description": "d"}}}]}`),
			want: "canonical",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := writeArtifact(t, "an artifact")
			var stdout, stderr bytes.Buffer
			if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(tc.payload)); code == exitOK {
				t.Fatal("stamp accepted a schema a reader would refuse")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr should mention %q, got: %s", tc.want, stderr.String())
			}
			if _, err := schema.ReadTrailerFile(artifact); err == nil {
				t.Fatal("a refused stamp must leave the artifact unstamped")
			}
		})
	}
}

func TestStamp_IsIdempotent(t *testing.T) {
	doc := canonicalBytes(t, testDocument("Redis ACL users"))
	artifact := writeArtifact(t, "an artifact")
	var stdout, stderr bytes.Buffer

	for i := range 3 {
		if code := run([]string{"stamp", artifact}, &stdout, &stderr, fixedDeriver(doc)); code != exitOK {
			t.Fatalf("stamp #%d: exit %d, stderr: %s", i, code, stderr.String())
		}
	}
	body, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if want := len("an artifact") + len(doc) + 8 + len(schema.TrailerMagic); len(body) != want {
		t.Fatalf("repeated stamping grew the artifact: %d bytes, want %d", len(body), want)
	}
}

func TestRun_UsageErrors(t *testing.T) {
	doc := canonicalBytes(t, testDocument("Redis ACL users"))
	cases := map[string][]string{
		"no_arguments":    {},
		"missing_path":    {"stamp"},
		"unknown_command": {"bless", "artifact"},
		"too_many":        {"stamp", "a", "b"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr, fixedDeriver(doc)); code != exitUsage {
				t.Fatalf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Fatalf("expected usage on stderr, got: %s", stderr.String())
			}
		})
	}
}
