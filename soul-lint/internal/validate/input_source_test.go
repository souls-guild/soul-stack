package validate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Статвалидатор (через shared/config) обязан знать ключ source: и format: sid
// (ADR-044 S-T1) — иначе они поднимаются как unknown_key / input_format_invalid.

func TestInputSourceFormatSID_NotUnknown(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    format: sid
    source: { incarnation_hosts: true }
  hosts:
    type: array
    min_items: 1
    max_items: 3
    items:
      type: string
      format: sid
    source: { choir: redis_primary }
`
	tmp := t.TempDir()
	p := filepath.Join(tmp, "destiny.yml")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errOut bytes.Buffer
	code := Run(Options{Path: p, Kind: KindDestiny}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit: got %d want %d\nstdout: %s\nstderr: %s", code, ExitOK, out.String(), errOut.String())
	}
}
