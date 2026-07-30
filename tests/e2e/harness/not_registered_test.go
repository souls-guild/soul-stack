//go:build e2e

package harness

import (
	"strings"
	"testing"
)

// TestNotRegisteredMessage_NamesTheOmission pins the two things the diagnostic
// has to distinguish, because the keeper cannot distinguish them: a test that
// registered nothing, and a test that registered under a different name.
//
// Why a test for a message (NIM-317). Four L3a tests spent a release failing at
// CreateIncarnation with the handler's own sentence — "service
// service-hello-world is not registered (manage via service.* API, ADR-029)" —
// which reads as a keeper defect and names neither the missing
// stack.RegisterService call nor the fact that the service name itself no longer
// existed. Nobody re-read it, because a red suite and an unwritten suite look
// identical from outside. The repair is only durable if the wording stays; a
// well-meant refactor that collapses both branches into one generic string puts
// the next author back where the last four were. This test is what makes that
// collapse fail.
//
// No Stack, no containers: notRegisteredMessage is pure, so this runs in the
// e2e job as fast as a unit test.
func TestNotRegisteredMessage_NamesTheOmission(t *testing.T) {
	const body = `{"status":422,"detail":"service hello-world is not registered (manage via service.* API, ADR-029)"}`

	t.Run("registered nothing — hands over the exact call to add", func(t *testing.T) {
		got := notRegisteredMessage(
			"CreateIncarnation test-hello", "hello-world",
			"examples/service/hello-world", nil, []byte(body))

		for _, want := range []string{
			`CreateIncarnation test-hello`,
			`registered NOTHING`,
			`stack.RegisterService(t, "hello-world", "examples/service/hello-world")`,
			`ADR-029`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("message does not contain %q\n--- message ---\n%s", want, got)
			}
		}
	})

	t.Run("registered under another name — says so instead of repeating NOTHING", func(t *testing.T) {
		got := notRegisteredMessage(
			"CreateIncarnation test-hello", "service-hello-world",
			"examples/service/hello-world",
			map[string]string{"hello-world": "examples/service/hello-world"},
			[]byte(body))

		if strings.Contains(got, "registered NOTHING") {
			t.Errorf("a mismatched name must not be reported as an empty registry\n--- message ---\n%s", got)
		}
		// The registered name has to be quoted back: it IS the fix — the four
		// tests of NIM-317 asked for `service-<X>` while the examples declare
		// `<X>`, and only seeing both names side by side makes that visible.
		for _, want := range []string{`hello-world`, `service.yml`} {
			if !strings.Contains(got, want) {
				t.Errorf("message does not contain %q\n--- message ---\n%s", want, got)
			}
		}
	})

	t.Run("no service ref — still actionable", func(t *testing.T) {
		// RunScenario knows only the incarnation, so the service name is not
		// available. The message must degrade to a usable instruction rather
		// than print an empty name.
		got := notRegisteredMessage(
			"RunScenario test-hello/create", "",
			"examples/service/hello-world", nil, []byte(body))

		if strings.Contains(got, `service "" `) {
			t.Errorf("empty service name leaked into the message\n--- message ---\n%s", got)
		}
		if !strings.Contains(got, "service.yml") {
			t.Errorf("message must still point at where the name comes from\n--- message ---\n%s", got)
		}
	})
}
