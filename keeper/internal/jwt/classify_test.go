package jwt

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyVerifyErr — covers every branch of the switch in
// ClassifyVerifyErr. Source of truth is verifier.go: the classifier only
// distinguishes expired and invalid-issuer; everything else (malformed,
// bad-signature, not-yet-valid, arbitrary error) collapses by design into
// one generic detail "invalid token" — a defense against oracle attacks via
// distinguishing 401 causes.
//
// Sentinel errors are checked both directly and wrapped
// (fmt.Errorf("%w: …")), because Verify returns them wrapped.
func TestClassifyVerifyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil returns empty string",
			err:  nil,
			want: "",
		},
		{
			name: "expired token",
			err:  ErrExpiredToken,
			want: publicDetailExpiredToken,
		},
		{
			name: "expired token wrapped",
			err:  fmt.Errorf("%w: extra context", ErrExpiredToken),
			want: publicDetailExpiredToken,
		},
		{
			name: "invalid issuer",
			err:  ErrInvalidIssuer,
			want: publicDetailInvalidIssuer,
		},
		{
			name: "invalid issuer wrapped (as Verify returns it)",
			err:  fmt.Errorf("%w: got %q, want %q", ErrInvalidIssuer, "evil", "keeper.test"),
			want: publicDetailInvalidIssuer,
		},
		{
			name: "ErrInvalidToken → generic-detail",
			err:  ErrInvalidToken,
			want: publicDetailInvalidToken,
		},
		{
			name: "ErrInvalidToken wrapped (malformed/bad-sig Verify path)",
			err:  fmt.Errorf("%w: token is malformed", ErrInvalidToken),
			want: publicDetailInvalidToken,
		},
		{
			name: "arbitrary unwrapped error -> default branch",
			err:  errors.New("something entirely different"),
			want: publicDetailInvalidToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyVerifyErr(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyVerifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyVerifyErr_NeverLeaksRawError — the guarantee from
// ClassifyVerifyErr's docstring: detail NEVER contains the raw err.Error()
// (golang-jwt's internal message is an oracle surface). Checked against
// an error with a recognizable marker in its wrapper.
func TestClassifyVerifyErr_NeverLeaksRawError(t *testing.T) {
	marker := "INTERNAL-PARSER-PATH-MUST-NOT-LEAK"
	err := fmt.Errorf("%w: %s", ErrInvalidToken, marker)
	got := ClassifyVerifyErr(err)
	if got == "" {
		t.Fatalf("ClassifyVerifyErr returned an empty string for a non-nil error")
	}
	if got != publicDetailInvalidToken {
		t.Fatalf("detail = %q, want %q", got, publicDetailInvalidToken)
	}
	// Extra invariant check: the marker must not surface in detail.
	if got == err.Error() {
		t.Errorf("detail matched raw err.Error() - internal message leak")
	}
}
