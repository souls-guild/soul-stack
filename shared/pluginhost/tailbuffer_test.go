package pluginhost

import "testing"

func TestTailBuffer(t *testing.T) {
	tb := newTailBuffer(8)
	_, _ = tb.Write([]byte("12345"))
	if tb.String() != "12345" {
		t.Errorf("after 5 bytes: %q", tb.String())
	}
	_, _ = tb.Write([]byte("6789AB"))
	// Total written: 11 bytes; max=8 → last 8 = "456789AB".
	if got := tb.String(); got != "456789AB" {
		t.Errorf("tail = %q, want 456789AB", got)
	}
	// Большой write — должен обрезаться до последних max байт.
	tb2 := newTailBuffer(4)
	_, _ = tb2.Write([]byte("0123456789"))
	if got := tb2.String(); got != "6789" {
		t.Errorf("large write tail = %q, want 6789", got)
	}
}
