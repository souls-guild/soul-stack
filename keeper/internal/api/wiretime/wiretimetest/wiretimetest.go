// Package wiretimetest runs a test inside a Keeper process whose zone is not
// UTC, which is the condition the NIM-1160 leak needs and CI does not have.
package wiretimetest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// Zone is the zone of the stand the leak was seen on. Brazil keeps no daylight
// saving time, so its offset is -03:00 all year.
const Zone = "America/Sao_Paulo"

const childEnv = "KEEPER_WIRETIME_CHILD"

// InSaoPaulo re-runs the calling top-level test in a child process with TZ set
// to [Zone], and fails t if the child fails. It returns true only in the child,
// which is where the test body runs:
//
//	if !wiretimetest.InSaoPaulo(t) {
//		return
//	}
func InSaoPaulo(t *testing.T) bool {
	t.Helper()
	if os.Getenv(childEnv) == t.Name() {
		if _, off := time.Now().Zone(); off != -3*60*60 {
			t.Fatalf("the child runs at offset %ds, want -03:00: TZ=%s did not load", off, Zone)
		}
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "TZ="+Zone, childEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("--- PASS: "+t.Name())) {
		t.Fatalf("under TZ=%s (err %v):\n%s", Zone, err, out)
	}
	return false
}

// Instants returns every string in a JSON document that parses as an RFC 3339
// instant, with its JSON path.
func Instants(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("answer is not JSON: %v\n%s", err, doc)
	}
	found := map[string]string{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		case string:
			if _, err := time.Parse(time.RFC3339Nano, x); err == nil {
				found[path] = x
			}
		}
	}
	walk("$", v)
	return found
}
