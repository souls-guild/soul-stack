package voyage

import "testing"

// ---- ResolveBatchMode: NULL/пустой → barrier (forward-compat), иначе как есть ----

func TestResolveBatchMode(t *testing.T) {
	t.Parallel()
	window := BatchModeWindow
	barrier := BatchModeBarrier
	empty := BatchMode("")
	cases := []struct {
		name string
		in   *BatchMode
		want BatchMode
	}{
		{"nil → barrier", nil, BatchModeBarrier},
		{"empty → barrier", &empty, BatchModeBarrier},
		{"barrier → barrier", &barrier, BatchModeBarrier},
		{"window → window", &window, BatchModeWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBatchMode(tc.in); got != tc.want {
				t.Errorf("ResolveBatchMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- ValidBatchMode: только closed-enum значения валидны ----

func TestValidBatchMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   BatchMode
		want bool
	}{
		{BatchModeBarrier, true},
		{BatchModeWindow, true},
		{BatchMode(""), false},
		{BatchMode("garbage"), false},
		{BatchMode("WINDOW"), false}, // регистр значим
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			if got := ValidBatchMode(tc.in); got != tc.want {
				t.Errorf("ValidBatchMode(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---- ResolveFailThreshold: обобщённый abort-gate (S-W3) ----

func TestResolveFailThreshold(t *testing.T) {
	t.Parallel()
	abort := OnFailureAbort
	cont := OnFailureContinue
	n3 := 3
	cases := []struct {
		name      string
		threshold *int
		policy    *OnFailure
		want      int
	}{
		{"nil+nil → 0 (без порога)", nil, nil, 0},
		{"nil+continue → 0", nil, &cont, 0},
		{"nil+abort → 1 (backcompat)", nil, &abort, 1},
		{"explicit 3 → 3", &n3, nil, 3},
		{"explicit 3 побеждает abort", &n3, &abort, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveFailThreshold(tc.threshold, tc.policy); got != tc.want {
				t.Errorf("ResolveFailThreshold = %d, want %d", got, tc.want)
			}
		})
	}
}

// ---- ResolveRequireAlive: nil → false (forward-compat) ----

func TestResolveRequireAlive(t *testing.T) {
	t.Parallel()
	tru, fls := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil → false", nil, false},
		{"false → false", &fls, false},
		{"true → true", &tru, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveRequireAlive(tc.in); got != tc.want {
				t.Errorf("ResolveRequireAlive = %v, want %v", got, tc.want)
			}
		})
	}
}
