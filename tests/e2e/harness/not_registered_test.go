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
		// tests of NIM-317 asked for `service-<X>` while the examples were
		// registered as `<X>`, and only seeing both names side by side makes that
		// visible. Since NIM-726 the message cites RegisterService rather than the
		// example's `service.yml`, because the manifest no longer states a name.
		for _, want := range []string{`hello-world`, `RegisterService`} {
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
		// With no service ref the message has to name a PLACEHOLDER where the name
		// would go — that is the whole degradation being tested. Asserting on
		// "RegisterService" would be vacuous: the empty-registry branch always
		// prints the `stack.RegisterService(t, …)` snippet, so the pin would stay
		// green with the placeholder deleted. Pin the placeholder itself. It cites
		// registration and not the example's `service.yml` because since NIM-726
		// the manifest states no name, and sending an author to that file would have
		// them add a `name:` key that makes the service refuse to load.
		if !strings.Contains(got, "<the name you register the example under>") {
			t.Errorf("message must still point at where the name comes from\n--- message ---\n%s", got)
		}
		if strings.Contains(got, "service.yml") {
			t.Errorf("the message must not send the author to service.yml for a name it no longer has\n--- message ---\n%s", got)
		}
	})
}
