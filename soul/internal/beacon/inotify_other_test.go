//go:build !linux

package beacon

import (
	"context"
	"strings"
	"testing"
)

// L0 unit test for the non-Linux stub variant (darwin/windows): Check always
// returns "platform not supported"; Default() still assembles the registry
// without panicking (the address constant is available everywhere, see beaconaddr.All).
func TestInotifyStub_NotSupported(t *testing.T) {
	b := NewInotify()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path": "/tmp",
	}))
	if err == nil {
		t.Fatal("на non-Linux stub обязан возвращать ошибку")
	}
	if !strings.Contains(err.Error(), "platform not supported") {
		t.Errorf("ошибка stub-а должна упоминать 'platform not supported', got %q", err.Error())
	}
	if state != "" || data != nil {
		t.Errorf("stub не должен возвращать state/data, got state=%q data=%v", state, data)
	}
}

func TestInotifyStub_RegistryDefault(t *testing.T) {
	reg := Default()
	if _, ok := reg.Lookup(InotifyName); !ok {
		t.Fatalf("InotifyName=%q отсутствует в Default() даже на non-Linux", InotifyName)
	}
}
