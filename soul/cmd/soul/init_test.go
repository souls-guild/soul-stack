package main

import "testing"

// TestResolveInitToken verifies the bootstrap-token source precedence for
// `soul init`: an explicit --token wins over env SOUL_BOOTSTRAP_TOKEN; an
// empty flag falls back to env; both empty is an error.
func TestResolveInitToken(t *testing.T) {
	// An empty env string is equivalent to "unset": os.Getenv returns "", and
	// resolveInitToken treats that as no source. t.Setenv always sets a
	// deterministic value and unsets it after the test, isolating from the
	// CI's env.
	tests := []struct {
		name      string
		flagToken string
		env       string
		want      string
		wantErr   bool
	}{
		{
			name:      "flag wins over env",
			flagToken: "flag-tok",
			env:       "env-tok",
			want:      "flag-tok",
		},
		{
			name:      "flag wins even without env",
			flagToken: "flag-tok",
			env:       "",
			want:      "flag-tok",
		},
		{
			name:      "env is picked up when flag is empty",
			flagToken: "",
			env:       "env-tok",
			want:      "env-tok",
		},
		{
			name:      "both empty -> error",
			flagToken: "",
			env:       "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envBootstrapToken, tt.env)

			got, err := resolveInitToken(tt.flagToken)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveInitToken(%q) = %q, nil; want error", tt.flagToken, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInitToken(%q) unexpected error: %v", tt.flagToken, err)
			}
			if got != tt.want {
				t.Errorf("resolveInitToken(%q) = %q, want %q", tt.flagToken, got, tt.want)
			}
		})
	}
}
