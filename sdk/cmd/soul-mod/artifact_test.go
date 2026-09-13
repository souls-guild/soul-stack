package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// buildArtifact compiles testdata/bundle-artifact into dist/ under a temp directory and
// returns the path. Everything below runs against that real executable: the fake
// deriver in main_test.go proves the stamping logic, this proves the whole loop —
// `<artifact> schema` → trailer → verify — the way an author's Makefile runs it.
func buildArtifact(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	dist := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	out := filepath.Join(dist, "redis")

	cmd := exec.Command("go", "build", "-o", out, "./testdata/bundle-artifact")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build artifact: %v\n%s", err, combined)
	}
	return out
}

// runArtifact invokes the built artifact with the given arguments and environment.
func runArtifact(t *testing.T, path string, env []string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var so, se bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

func TestArtifact_StampVerifyLoop(t *testing.T) {
	artifact := buildArtifact(t)

	// What the artifact says about itself, straight from its own subcommand.
	printed, stderr, err := runArtifact(t, artifact, nil, schema.SchemaSubcommand)
	if err != nil {
		t.Fatalf("run schema: %v (stderr: %s)", err, stderr)
	}
	doc, err := schema.Unmarshal([]byte(printed))
	if err != nil {
		t.Fatalf("the artifact printed something unparseable: %v\n%s", err, printed)
	}
	if names := doc.ModuleNames(); len(names) != 2 || names[0] != "acl" || names[1] != "info" {
		t.Fatalf("modules: got %v", names)
	}

	var stdout, errOut bytes.Buffer
	if code := run([]string{"stamp", artifact}, &stdout, &errOut, execSchema); code != exitOK {
		t.Fatalf("stamp exit %d: %s", code, errOut.String())
	}

	stamped, err := schema.ReadTrailerFile(artifact)
	if err != nil {
		t.Fatalf("ReadTrailerFile: %v", err)
	}
	if string(stamped) != printed {
		t.Fatalf("trailer does not hold what the artifact printed:\ntrailer: %s\nprinted: %s", stamped, printed)
	}
	published, err := os.ReadFile(filepath.Join(filepath.Dir(artifact), schema.SchemaFileName))
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	if string(published) != printed {
		t.Fatal("schema.json and the trailer must carry identical bytes")
	}

	stdout.Reset()
	errOut.Reset()
	if code := run([]string{"verify", artifact}, &stdout, &errOut, execSchema); code != exitOK {
		t.Fatalf("verify exit %d: %s", code, errOut.String())
	}

	// A stamped artifact is still an artifact: appending after the image must not
	// disturb the loader.
	if _, stderr, err := runArtifact(t, artifact, nil, schema.SchemaSubcommand); err != nil {
		t.Fatalf("the stamped artifact no longer runs: %v (stderr: %s)", err, stderr)
	}
}

func TestArtifact_VerifyFailsWhenTheCodeChanges(t *testing.T) {
	artifact := buildArtifact(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stamp", artifact}, &stdout, &stderr, execSchema); code != exitOK {
		t.Fatalf("stamp exit %d: %s", code, stderr.String())
	}

	// SOUL_MOD_TEST_DESCRIPTION changes what the artifact declares, standing in for
	// an author editing a module.Def and not re-stamping.
	changed := func(artifact string) ([]byte, error) {
		t.Setenv("SOUL_MOD_TEST_DESCRIPTION", "Redis ACL users, rules and channels")
		return execSchema(artifact)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", artifact}, &stdout, &stderr, changed); code == exitOK {
		t.Fatal("verify accepted a stamp that no longer matches the code")
	}
	if !strings.Contains(stderr.String(), "does not match the code") {
		t.Fatalf("stderr should say what is wrong, got: %s", stderr.String())
	}

	// Re-stamping with the new description closes the gap again.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"stamp", artifact}, &stdout, &stderr, changed); code != exitOK {
		t.Fatalf("re-stamp exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", artifact}, &stdout, &stderr, changed); code != exitOK {
		t.Fatalf("verify after re-stamp: exit %d: %s", code, stderr.String())
	}
}

func TestArtifact_UnknownSubcommandExitsNonZero(t *testing.T) {
	artifact := buildArtifact(t)
	for _, args := range [][]string{{"acl-v2"}, {}, {"--version"}, {"ACL"}} {
		stdout, stderr, err := runArtifact(t, artifact, nil, args...)
		if err == nil {
			t.Fatalf("args %v: the artifact exited 0 (stdout: %s)", args, stdout)
		}
		if stderr == "" {
			t.Fatalf("args %v: nothing on stderr", args)
		}
		if stdout != "" {
			t.Fatalf("args %v: unexpected stdout: %s", args, stdout)
		}
	}
}

func TestArtifact_ServingAModuleNeedsASocket(t *testing.T) {
	// Naming a real module gets past dispatch and into the handshake, which fails
	// without SOUL_PLUGIN_SOCKET. That is the observable difference between "this
	// module exists" and "this module does not": an unknown name never reaches it.
	artifact := buildArtifact(t)
	_, stderr, err := runArtifact(t, artifact, []string{"SOUL_PLUGIN_SOCKET="}, "acl")
	if err == nil {
		t.Fatal("expected the handshake to fail with no socket")
	}
	if !strings.Contains(stderr, "SOUL_PLUGIN_SOCKET") {
		t.Fatalf("expected a handshake failure, got: %s", stderr)
	}
	if strings.Contains(stderr, "unknown module") {
		t.Fatalf("a declared module was treated as unknown: %s", stderr)
	}
}
