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
			name:    "валидный Bearer",
			header:  "Bearer abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "scheme в нижнем регистре",
			header:  "bearer abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "scheme в верхнем регистре",
			header:  "BEARER abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "scheme смешанный регистр",
			header:  "BeArEr abc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "разделитель — табуляция",
			header:  "Bearer\tabc.def.ghi",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "лишние пробелы вокруг токена режутся TrimSpace",
			header:  "Bearer    abc.def.ghi   ",
			wantTok: "abc.def.ghi",
			wantOK:  true,
		},
		{
			name:    "пустая строка",
			header:  "",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "нет разделителя — только scheme",
			header:  "Bearer",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "scheme с разделителем, но без токена",
			header:  "Bearer ",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "scheme и только пробелы вместо токена",
			header:  "Bearer      ",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "не-Bearer scheme (Basic)",
			header:  "Basic dXNlcjpwYXNz",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "header начинается с пробела (idx == 0)",
			header:  " Bearer abc.def.ghi",
			wantTok: "",
			wantOK:  false,
		},
		{
			name:    "голый токен без scheme",
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
		t.Errorf("ParseBearerToken token = %q, want %q (внутренний пробел сохраняется)", gotTok, "abc def")
	}
}
