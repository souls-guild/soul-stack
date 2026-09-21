package schema

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// sampleDocument is a document that exercises every corner the serializer has to be
// deterministic about: several modules, several states, several parameters, a nested
// items descriptor, an enum of mixed literals, and an arbitrary JSON-Schema map.
func sampleDocument() Document {
	return Document{
		Kind:            KindSoulModule,
		ProtocolVersion: 1,
		Compat:          Compat{Keeper: ">=0.9 <2.0"},
		Modules: []Module{
			{
				Name:         "acl",
				Description:  "Redis ACL users",
				Capabilities: []Capability{NetworkOutbound, VaultAccess},
				SideEffects:  []SideEffect{{User: "redis_acl_user"}, {Service: "redis"}},
				States: map[string]State{
					"present": {
						Description: "The ACL user exists",
						Input: Input{
							"host":  {Type: String, Required: true, Description: "Redis host"},
							"port":  {Type: Int, Default: 6379},
							"rules": {Type: List, Items: &Param{Type: String}},
							"login_password": {
								Type: String, Secret: true, Pattern: `^vault:.*`,
								Deprecated: &Deprecated{Since: "0.4.0", RemovedIn: "0.6.0", Use: "host"},
							},
							"mode": {Type: String, Enum: []any{"acl", "legacy"}, Format: "hostname"},
							"tls":  {Type: Bool, Default: false},
						},
						Output: Output{"users": {Type: List, Items: &Param{Type: String}}},
					},
					"absent": {Description: "The ACL user is gone", Input: Input{
						"host": {Type: String, Required: true},
					}},
				},
			},
			{
				Name:        "info",
				Description: "Read-only facts",
				States: map[string]State{
					"reported": {Description: "Facts are reported"},
				},
			},
		},
	}
}

func TestMarshal_IsDeterministic(t *testing.T) {
	doc := sampleDocument()
	first, err := Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Go randomizes map iteration order per range statement, so repeating the
	// marshal in one process is what actually exercises it.
	for i := range 200 {
		got, err := Marshal(doc)
		if err != nil {
			t.Fatalf("Marshal #%d: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("Marshal #%d differs from the first run:\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

func TestMarshal_IgnoresMapInsertionOrder(t *testing.T) {
	names := []string{"zeta", "alpha", "middle", "beta"}

	build := func(order []string) Document {
		states := make(map[string]State, len(order))
		for _, n := range order {
			input := make(Input, len(order))
			for _, p := range order {
				input[p] = Param{Type: String, Description: "param " + p}
			}
			states[n] = State{Description: "state " + n, Input: input}
		}
		return Document{
			Kind:            KindSoulModule,
			ProtocolVersion: 1,
			Modules:         []Module{{Name: "acl", States: states}},
		}
	}

	forward, err := Marshal(build(names))
	if err != nil {
		t.Fatalf("Marshal forward: %v", err)
	}
	reversed := make([]string, len(names))
	for i, n := range names {
		reversed[len(names)-1-i] = n
	}
	backward, err := Marshal(build(reversed))
	if err != nil {
		t.Fatalf("Marshal backward: %v", err)
	}
	if !bytes.Equal(forward, backward) {
		t.Fatalf("insertion order changed the bytes:\nforward:  %s\nbackward: %s", forward, backward)
	}
}

func TestMarshal_SortsEveryObjectKey(t *testing.T) {
	// A struct field declared out of alphabetical order would slip past a
	// round-trip test but not past this one.
	raw, err := Marshal(sampleDocument())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	assertSortedKeys(t, raw)
}

// assertSortedKeys walks the token stream, which preserves the order keys were
// written in, and fails on the first pair that is out of order.
func assertSortedKeys(t *testing.T, raw []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	// stack holds the last key seen at each open object depth; "" for arrays.
	var stack []string
	var inObject []bool
	expectKey := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, "")
				inObject = append(inObject, true)
				expectKey = true
			case '[':
				inObject = append(inObject, false)
				expectKey = false
			case '}', ']':
				if inObject[len(inObject)-1] {
					stack = stack[:len(stack)-1]
				}
				inObject = inObject[:len(inObject)-1]
				expectKey = len(inObject) > 0 && inObject[len(inObject)-1]
			}
			continue
		}
		if expectKey {
			key, ok := tok.(string)
			if !ok {
				t.Fatalf("expected an object key, got %T %v", tok, tok)
			}
			if prev := stack[len(stack)-1]; prev != "" && prev >= key {
				t.Fatalf("object keys out of order: %q then %q", prev, key)
			}
			stack[len(stack)-1] = key
			expectKey = false
			continue
		}
		// A scalar value inside an object is followed by the next key.
		expectKey = len(inObject) > 0 && inObject[len(inObject)-1]
	}
}

func TestMarshal_Idempotent(t *testing.T) {
	first, err := Marshal(sampleDocument())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := Unmarshal(first)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	second, err := Marshal(parsed)
	if err != nil {
		t.Fatalf("Marshal round two: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round trip changed the bytes:\nfirst:  %s\nsecond: %s", first, second)
	}
	ok, err := IsCanonical(first)
	if err != nil {
		t.Fatalf("IsCanonical: %v", err)
	}
	if !ok {
		t.Fatal("Marshal output is not reported as canonical")
	}
}

func TestMarshal_NoWhitespaceAndNoHTMLEscaping(t *testing.T) {
	doc := Document{
		Kind:            KindSoulModule,
		ProtocolVersion: 1,
		Modules: []Module{{
			Name:        "acl",
			Description: `a < b && c > d, "quoted", back\slash`,
			States:      map[string]State{"present": {Description: "line1\nline2\ttab"}},
		}},
	}
	raw, err := Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(string(raw), esc) {
			t.Fatalf("encoding/json HTML escaping (%s) leaked into the document: %s", esc, raw)
		}
	}
	if !strings.Contains(string(raw), `a < b && c > d`) {
		t.Fatalf("expected the literal text, got: %s", raw)
	}
	if !strings.Contains(string(raw), `\"quoted\"`) || !strings.Contains(string(raw), `back\\slash`) {
		t.Fatalf("expected quote and backslash escapes, got: %s", raw)
	}
	if !strings.Contains(string(raw), `line1\nline2\ttab`) {
		t.Fatalf("expected control-character escapes, got: %s", raw)
	}
	if bytes.ContainsAny(raw[:len(raw)-1], "\n\t") {
		t.Fatalf("insignificant whitespace in the document: %q", raw)
	}
	// Whatever we escape must survive the trip back.
	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Modules[0].Description != doc.Modules[0].Description {
		t.Fatalf("description round trip: got %q want %q", back.Modules[0].Description, doc.Modules[0].Description)
	}
}

func TestMarshal_OmitsEmptyCompat(t *testing.T) {
	raw, err := Marshal(Document{
		Kind: KindSoulModule, ProtocolVersion: 1,
		Modules: []Module{{Name: "acl", States: map[string]State{"present": {Description: "d"}}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "compat") {
		t.Fatalf("an undeclared compat window should not appear in the document: %s", raw)
	}
}

func TestUnmarshal_RejectsUnknownKey(t *testing.T) {
	// Strict decoding is what makes adding a Param field a forward-compat event
	// (ADR-0076(i)/(q)) instead of a silent one.
	_, err := Unmarshal([]byte(`{"kind":"soul_module","protocol_version":1,"future_field":true}`))
	if err == nil {
		t.Fatal("expected an unknown key to be rejected")
	}
	if !strings.Contains(err.Error(), "future_field") {
		t.Fatalf("error should name the unknown key, got: %v", err)
	}
}

func TestUnmarshal_RejectsTrailingContent(t *testing.T) {
	_, err := Unmarshal([]byte(`{"kind":"soul_module","protocol_version":1}{"kind":"soul_module"}`))
	if err == nil {
		t.Fatal("expected trailing content to be rejected")
	}
}

func TestIsCanonical_RejectsPrettyPrinted(t *testing.T) {
	doc := sampleDocument()
	pretty, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	ok, err := IsCanonical(pretty)
	if err != nil {
		t.Fatalf("IsCanonical: %v", err)
	}
	if ok {
		t.Fatal("pretty-printed JSON must not be accepted as canonical")
	}
}

func TestParseVersion(t *testing.T) {
	valid := map[string]version{
		"0.0.0":     {0, 0, 0},
		"1.2.3":     {1, 2, 3},
		"10.20.30":  {10, 20, 30},
		"0.10.0":    {0, 10, 0},
		"123.0.456": {123, 0, 456},
	}
	for in, want := range valid {
		got, ok := parseVersion(in)
		if !ok || got != want {
			t.Fatalf("parseVersion(%q) = %+v,%v; want %+v,true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.02.3",
		"1.2.3-rc1", "+1.2.3", "1.-2.3", "a.b.c", "1..3", "1.2."} {
		if _, ok := parseVersion(in); ok {
			t.Fatalf("parseVersion(%q) accepted an invalid version", in)
		}
	}
}
