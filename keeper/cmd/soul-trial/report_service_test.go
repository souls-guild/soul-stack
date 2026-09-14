package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
)

// ★ NIM-726. The report must NAME the service the own-namespace Vault fence ran
// against, and must mark a name nobody stated.
//
// The fence is keyed on the name a service is REGISTERED under, and since NIM-726
// nothing in a service repository states it, so L0 derives one from the directory.
// A derived name that is not the registered name is non-empty — it clears every
// `service == ""` guard — and simply matches no path: the fence runs, finds nothing,
// and the case is green. That is indistinguishable from a genuinely clean scenario in
// any output that does not print the name, and offline there is no second source to
// check it against. So the report prints it. Same rule soul-lint applies with
// `own_namespace_fence_unchecked`: a check whose input was guessed says so, and the
// operator — the only party who knows the registered name — can see it.
//
// Delete either branch below and this goes red.
func TestPrintResults_NamesTheFencedService(t *testing.T) {
	t.Run("derived — says where the name came from", func(t *testing.T) {
		var b bytes.Buffer
		printResults(&b, []trial.Result{{
			Case: "c", Pass: true, Level: trial.LevelL0,
			Service: "demo-service-redis", ServiceStated: false,
		}})
		got := b.String()
		if !strings.Contains(got, `"demo-service-redis"`) {
			t.Fatalf("report does not name the fenced service\n%s", got)
		}
		// The hint has to name the escape hatch: an operator who sees a word that is
		// not their service needs to know what to do about it.
		if !strings.Contains(got, "fixtures.service") {
			t.Fatalf("derived name reported without naming fixtures.service\n%s", got)
		}
	})

	t.Run("stated — no hint, someone already decided", func(t *testing.T) {
		var b bytes.Buffer
		printResults(&b, []trial.Result{{
			Case: "c", Pass: true, Level: trial.LevelL0,
			Service: "redis", ServiceStated: true,
		}})
		got := b.String()
		if !strings.Contains(got, `"redis"`) {
			t.Fatalf("report does not name the fenced service\n%s", got)
		}
		if strings.Contains(got, "fixtures.service") {
			t.Fatalf("a stated name must not be nagged about\n%s", got)
		}
	})
}
