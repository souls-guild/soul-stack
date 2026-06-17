package api

import (
	"errors"
	"net/url"
	"strconv"
	"testing"
)

func TestParsePage_Defaults(t *testing.T) {
	p, err := ParsePage(url.Values{})
	if err != nil {
		t.Fatalf("ParsePage empty: %v", err)
	}
	if p.Offset != 0 {
		t.Errorf("Offset = %d, want 0", p.Offset)
	}
	if p.Limit != DefaultPageLimit {
		t.Errorf("Limit = %d, want %d", p.Limit, DefaultPageLimit)
	}
}

func TestParsePage_EmptyStringsTreatedAsAbsent(t *testing.T) {
	// Распространённый случай: клиент шлёт `?offset=&limit=` (например,
	// из формы). Должны взять defaults, не отказывать.
	q := url.Values{"offset": []string{""}, "limit": []string{""}}
	p, err := ParsePage(q)
	if err != nil {
		t.Fatalf("ParsePage empty strings: %v", err)
	}
	if p.Offset != 0 || p.Limit != DefaultPageLimit {
		t.Errorf("got %+v", p)
	}
}

func TestParsePage_ValidValues(t *testing.T) {
	q := url.Values{"offset": []string{"100"}, "limit": []string{"25"}}
	p, err := ParsePage(q)
	if err != nil {
		t.Fatalf("ParsePage: %v", err)
	}
	if p.Offset != 100 || p.Limit != 25 {
		t.Errorf("got %+v, want {100 25}", p)
	}
}

func TestParsePage_RejectsInvalidOffset(t *testing.T) {
	cases := []string{"abc", "-1", "1.5"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			q := url.Values{"offset": []string{raw}}
			_, err := ParsePage(q)
			if err == nil {
				t.Fatalf("offset=%q expected error", raw)
			}
			var pe *PaginationError
			if !errors.As(err, &pe) {
				t.Errorf("err type = %T, want *PaginationError", err)
			}
		})
	}
}

func TestParsePage_RejectsInvalidLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"abc", "integer"},
		{"0", ">= 1"},
		{"-5", ">= 1"},
		{strconv.Itoa(MaxPageLimit + 1), "<="},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			q := url.Values{"limit": []string{c.raw}}
			_, err := ParsePage(q)
			if err == nil {
				t.Fatalf("limit=%q expected error", c.raw)
			}
		})
	}
}

func TestParsePage_BoundaryLimit(t *testing.T) {
	// Граница MaxPageLimit — допустима.
	q := url.Values{"limit": []string{strconv.Itoa(MaxPageLimit)}}
	p, err := ParsePage(q)
	if err != nil {
		t.Fatalf("limit=MaxPageLimit should be ok: %v", err)
	}
	if p.Limit != MaxPageLimit {
		t.Errorf("Limit = %d, want %d", p.Limit, MaxPageLimit)
	}
}

func TestPagedResponse_Generic(t *testing.T) {
	// Sanity-check, что generic-typing работает (compile-time + JSON-форма).
	type item struct{ Name string }
	resp := PagedResponse[item]{
		Items:  []item{{Name: "a"}, {Name: "b"}},
		Offset: 0, Limit: 50, Total: 2,
	}
	if len(resp.Items) != 2 || resp.Items[0].Name != "a" {
		t.Errorf("Items = %+v", resp.Items)
	}
}
