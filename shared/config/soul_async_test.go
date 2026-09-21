package config

import "testing"

// The `async:` block of soul.yml — the host-side ceiling on intra-host task
// concurrency (ADR-0075(e), destiny/tasks.md §6). The plan never names a number;
// the machine that has to tolerate the fan-out does.

func soulBaseWithAsync(asyncBlock string) []byte {
	return []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
` + asyncBlock)
}

// No block at all is the common case and means unlimited — real fan-out is a
// handful of renders, so a mandatory cap would be ceremony around a non-problem.
func TestSoulAsync_OmittedBlockIsUnlimited(t *testing.T) {
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", soulBaseWithAsync(""), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("a soul.yml without an async: block must be valid")
	}
	if cfg.Async != nil {
		t.Fatalf("omitted async: block must decode as nil, got %+v", cfg.Async)
	}
	if got := cfg.AsyncMaxConcurrent(); got != 0 {
		t.Errorf("AsyncMaxConcurrent() = %d, want 0 (unlimited)", got)
	}
}

func TestSoulAsync_MaxConcurrentRoundTrip(t *testing.T) {
	src := soulBaseWithAsync(`async:
  max_concurrent: 4
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("async.max_concurrent: 4 must be valid")
	}
	if got := cfg.AsyncMaxConcurrent(); got != 4 {
		t.Errorf("AsyncMaxConcurrent() = %d, want 4", got)
	}
}

// 0 is "unlimited", not "forbid async": a task above the ceiling waits for a
// slot, so a ceiling of zero slots would wedge every run using async:.
func TestSoulAsync_ExplicitZeroIsUnlimited(t *testing.T) {
	src := soulBaseWithAsync(`async:
  max_concurrent: 0
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("async.max_concurrent: 0 must be valid")
	}
	if got := cfg.AsyncMaxConcurrent(); got != 0 {
		t.Errorf("AsyncMaxConcurrent() = %d, want 0", got)
	}
}

// A negative value is a typo, not a policy — and it must be caught here rather
// than silently resolving to unlimited on the host.
func TestSoulAsync_NegativeRejected(t *testing.T) {
	src := soulBaseWithAsync(`async:
  max_concurrent: -1
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if !hasErrorDiag(diags) {
		t.Fatal("async.max_concurrent: -1 must be an error")
	}
}

// An unknown key inside the block is rejected fail-closed like everywhere else:
// a misspelled ceiling that silently means "unlimited" is the failure mode this
// prevents.
func TestSoulAsync_UnknownKeyRejected(t *testing.T) {
	src := soulBaseWithAsync(`async:
  max_parallel: 4
`)
	_, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if !hasErrorDiag(diags) {
		t.Fatal("async.max_parallel must be rejected as an unknown key")
	}
}
