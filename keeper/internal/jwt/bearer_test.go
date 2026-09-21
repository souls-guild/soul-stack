package jwt

import "testing"

// TestParseBearerToken — table of edge cases for the `Authorization`
// header parser. Assertions follow bearer.go's actual behavior:
// scheme is case-insensitive (strings.EqualFold), separator is SP or HTAB
// (strings.IndexAny(" \t")), the token part goes through TrimSpace.
func TestParseBearerToken(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		wantTok string
		wantOK  bool
	}{
		{
			name:    "valid Bearer",
			header:  "Bearer abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "lowercase scheme",
			header:  "bearer abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "uppercase scheme",
			header:  "BEARER abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "mixed-case scheme",
			header:  "BeArEr abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "tab separator",
			header:  "Bearer\tabc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "extra spaces around token are trimmed by TrimSpace",
			header:  "Bearer    abc.def.ghi   ",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "empty string",
			header:  "",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "no separator, scheme only",
			header:  "Bearer",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "scheme with separator but without token",
			header:  "Bearer ",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "scheme and only spaces instead of token",
			header:  "Bearer      ",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "non-Bearer scheme (Basic)",
			header:  "Basic dXNlcjpwYXNz",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "header starts with a space (idx == 0)",
			header:  " Bearer abc.def.ghi",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "bare token without scheme",
			header:  "abc.def.ghi",
			wantTok: "",
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTok, gotOK := ParseBearerToken(tc.header)
			if gotOK != tc.wantOK {
				t.Fatalf("ParseBearerToken(%q) ok = %v, want %v", tc.header, gotOK, tc.wantOK)
			}
			if gotTok != tc.wantTok {
				t.Errorf("ParseBearerToken(%q) token = %q, want %q", tc.header, gotTok, tc.wantTok)
			}
		})
	}
}

// TestParseBearerToken_InternalSpacesPreserved — TrimSpace only trims
// leading/trailing spaces of the token part; internal ones are kept. A JWT
// never contains internal spaces, but this pins the actual behavior:
// the parser doesn't "fix" such a token, it returns it as-is for Verify
// to reject downstream.
func TestParseBearerToken_InternalSpacesPreserved(t *testing.T) {
	gotTok, gotOK := ParseBearerToken("Bearer  abc def  ")
	if !gotOK {
		t.Fatalf("ParseBearerToken: ok = false, want true")
	}
	if gotTok != "abc def" {
		t.Errorf("ParseBearerToken token = %q, want %q (internal space is preserved)", gotTok, "abc def")
	}
}
