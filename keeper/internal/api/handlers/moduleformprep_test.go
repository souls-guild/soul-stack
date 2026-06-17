package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// stubFormPrepResolver — фейковый [FormPrepSIDResolver]: захватывает filter и
// отдаёт заранее заданный результат.
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

// formPrepProblemType извлекает problem.Type из ошибки FormPrepTyped (nil → "").
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
	// Пустой результат → sids:[] (non-nil), не nil.
	if out.Sids == nil {
		t.Fatal("sids must be non-nil (json []), got null")
	}
}

// TestFormPrepTyped_SourceValidation — доменная XOR-валидация source (ровно один
// непустой вариант) → 422. Bind-фазовые кейсы (unknown field / malformed body → 400)
// сняты — они покрыты huma-integration в пакете api (handler-native: тело декодит huma).
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
