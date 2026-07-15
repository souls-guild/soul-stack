package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// stubFormPrepResolver — a fake [FormPrepSIDResolver]: captures the filter and
// returns a preset result.
type stubFormPrepResolver struct {
	gotFilter FormPrepFilter
	sids      []string
	truncated bool
	err       error
}

func (s *stubFormPrepResolver) ResolveSIDs(_ context.Context, f FormPrepFilter) ([]string, bool, error) {
	s.gotFilter = f
	return s.sids, s.truncated, s.err
}

// formPrepProblemType extracts problem.Type from a FormPrepTyped error (nil → "").
func formPrepProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %T %v", err, err)
	}
	return d.Type
}

func TestFormPrepTyped_IncarnationHosts_OK(t *testing.T) {
	res := &stubFormPrepResolver{sids: []string{"a.example", "b.example"}, truncated: true}
	h := NewModuleFormPrepHandler(res, nil)

	out, err := h.FormPrepTyped(context.Background(), FormPrepInput{
		Source: FormPrepSourceInput{IncarnationHosts: "web"},
		Prefix: "a",
	})
	if err != nil {
		t.Fatalf("FormPrepTyped: %v", err)
	}
	if res.gotFilter.IncarnationHosts != "web" || res.gotFilter.Prefix != "a" {
		t.Fatalf("filter = %+v, want incarnation_hosts=web prefix=a", res.gotFilter)
	}
	if len(out.Sids) != 2 || !out.Truncated {
		t.Fatalf("response = %+v, want 2 sids + truncated", out)
	}
}

func TestFormPrepTyped_Choir_OK(t *testing.T) {
	res := &stubFormPrepResolver{sids: []string{}}
	h := NewModuleFormPrepHandler(res, nil)

	out, err := h.FormPrepTyped(context.Background(), FormPrepInput{
		Source: FormPrepSourceInput{Choir: &FormPrepChoirSource{Incarnation: "web", Name: "primary"}},
	})
	if err != nil {
		t.Fatalf("FormPrepTyped: %v", err)
	}
	if res.gotFilter.Choir == nil || res.gotFilter.Choir.Incarnation != "web" || res.gotFilter.Choir.Name != "primary" {
		t.Fatalf("choir filter = %+v, want web/primary", res.gotFilter.Choir)
	}
	// Empty result → sids:[] (non-nil), not nil.
	if out.Sids == nil {
		t.Fatal("sids must be non-nil (json []), got null")
	}
}

// TestFormPrepTyped_SourceValidation — domain XOR validation of source (exactly one
// non-empty variant) → 422. Bind-phase cases (unknown field / malformed body → 400) are
// dropped — they're covered by huma-integration in the api package (handler-native: huma decodes the body).
func TestFormPrepTyped_SourceValidation(t *testing.T) {
	cases := []struct {
		name string
		in   FormPrepInput
	}{
		{"no source", FormPrepInput{Prefix: "a"}},
		{"both sources", FormPrepInput{Source: FormPrepSourceInput{
			IncarnationHosts: "web",
			Choir:            &FormPrepChoirSource{Incarnation: "web", Name: "p"},
		}}},
		{"choir missing name", FormPrepInput{Source: FormPrepSourceInput{
			Choir: &FormPrepChoirSource{Incarnation: "web"},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewModuleFormPrepHandler(&stubFormPrepResolver{}, nil)
			_, err := h.FormPrepTyped(context.Background(), c.in)
			if got := formPrepProblemType(t, err); got != problem.TypeValidationFailed {
				t.Fatalf("problem.Type = %q, want %q (невалидный source → 422)", got, problem.TypeValidationFailed)
			}
		})
	}
}
