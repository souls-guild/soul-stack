package coremod_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// The verb-module failure contract, pinned doc-to-code (NIM-687).
//
// Before NIM-687 `core.exec.run` and `core.cmd.shell` reported `failed` only when
// the process could not be run at all: a command that started and exited non-zero
// was CHANGED, and the code was merely data in `register`. docs/destiny/tasks.md
// promised the opposite — that `failed` defaults to `exit_code != 0` — and had
// promised it in every release. A live run of demo-service-redis lost its real cause
// to that gap: the cluster-slot step exited 1 with "Connection refused", the
// framework recorded CHANGED, the run carried on and died a minute later on a
// neighbouring `until:` task, so the incarnation ended up holding somebody else's
// reason. The scenario author had left `failed_when:` off precisely because the
// documentation said it was implied.
//
// NIM-687 resolved the disagreement in favour of the docs, deliberately and with an
// ADR-015 amendment: the two modules take an `exit_codes` param defaulting to `[0]`,
// and a code outside the set fails the task. Two things stayed out of it — the
// guards (`creates`/`unless`/`onlyif`, whose skip reports `exit_code: 0` and never
// fails) and a process that never ran (`res.Err != nil`, a different class).
//
// This guard deliberately pins neither side on its own — the module behaviour has
// unit tests of its own and the prose can be reworded without touching code, and
// before NIM-687 both were green while contradicting each other. It couples them:
// it runs the REGISTERED verb modules against a real non-zero-exiting process with
// no `exit_codes` given, then requires the normative prose to state whatever was
// observed. Reword the doc and the assertion reds; revert the module default and the
// doc requirement flips with it, reddening until the prose is reverted too.
//
// It also pins the trap the change was most likely to fall into: the failing event
// must carry `stdout`/`stderr`/`exit_code` (`util.SendFailedWithOutput`, not the
// bare `util.SendFailed`). A failure that drops the output deletes exactly the
// stderr the exit-code default exists to surface, and makes a `failed_when:`
// predicate over `register.self.exit_code` unevaluable.

// failedWhenDocPath — the normative `failed_when:` section, relative to this
// package directory. docs/ sits at the repo root, outside the soul module; this
// is a filesystem path, not a Go import (same idiom as
// keeper/internal/mcp.mcpToolsDocPath).
const failedWhenDocPath = "../../../docs/destiny/tasks.md"

// The claims the prose can make about the default, matched lowercased against the
// section with code fences removed. The first two are mutually exclusive; the third
// is required alongside claimExitCodeDefault, because a documented exit-code default
// whose failure silently swallows the output is the version of this feature that
// makes diagnosis worse than the bug it fixes.
const (
	claimExitCodeDefault   = "a command that ran and exited non-zero fails the task"
	claimNoExitCodeDefault = "there is no exit-code default"
	claimOutputSurvives    = "the output survives the failure"
)

// reExitCodeDefaultClaim catches rewordings of [claimExitCodeDefault] ("defaults to
// a non-zero exit", "the default is exit code != 0"). Consulted only when the
// modules do NOT fail on a non-zero exit, i.e. when someone reverted the default and
// left the prose claiming it. Scoped to a single sentence — `[^.]` cannot cross a
// full stop.
var reExitCodeDefaultClaim = regexp.MustCompile(`default[^.]{0,80}(exit[ _-]?code|non-?zero exit)`)

// reNoExitCodeDefaultClaim is its mirror: rewordings of [claimNoExitCodeDefault]
// ("a non-zero exit is not a failure", "exit codes do not fail the task"). Consulted
// when the modules DO fail, where such a sentence is the pre-NIM-687 promise in new
// clothes. Also single-sentence-scoped, which is what keeps it off the true prose —
// there the negations sit in other sentences ("dependents do not fire", "the task is
// never considered abandoned") and none of them is about the exit code failing.
//
// Three branches, because a denial has three grammars and the markers only stop
// one of them. Restating the old promise ("a non-zero exit does not fail the task")
// deletes the marker, so the marker assertions catch it on their own. NEGATING the
// new one keeps the marker inside the sentence — "it is no longer true that a code
// outside `exit_codes` fails the task" contains the required substring verbatim —
// so strings.Contains vouches for the file while it tells the reader the opposite,
// and nothing but this matcher stands between that sentence and a green run.
// Pinned in every direction by TestNoExitCodeDefaultClaim_CatchesBothDirectionsOfDenial.
const (
	// The thing being talked about, where the negation has already been read and
	// the subject only has to confirm the sentence is about an exit code.
	denialSubject = `(non-?zero|exit[ _-]?code)`
	// The subject when it stands AHEAD of the negation, which has to be narrower.
	// `exit[ _-]?code` is a prefix of the parameter's own name, so with the wide
	// subject every ordinary sentence explaining what `exit_codes` does reads as a
	// denial: "a code listed in `exit_codes` does not fail the task", "`exit_codes`
	// lists the codes that do not fail the run" and "whatever the exit code, a task
	// with `failed_when: false` does not fail" all red, and prose like that is what
	// these docs will acquire more of. What is left is the subject that cannot be
	// scoped — "non-zero", the class the default turns down, and a code this
	// contract has already rejected, neither of which has a non-failing reading.
	// One residual, named rather than chased: a scoping condition placed ahead of
	// the class being named. "with `failed_when: false` a non-zero exit does not
	// fail the task" is its likeliest instance — `failed_when:` is documented
	// across these surfaces as the way back to the old behaviour, so sentences
	// like it will get written — and "a non-zero exit that `exit_codes` allows
	// does not fail the task" its least. The answer when one reds is to drop
	// "non-zero", which the prose on every surface already does, and NOT to widen
	// this subject: any filter that recognises the condition also swallows "it is
	// no longer true that a non-zero exit fails the task; use `failed_when:`",
	// which is the mistake the sentence filter below was built out of.
	rejectedSubject = `(non-?zero|outside [^.]{0,24}exit_codes|not (listed )?in [^.]{0,24}exit_codes|` +
		`rejected code|code [^.]{0,20}rejects)`
	// Denial after the subject: "a non-zero exit <…>".
	denialTail = `(does not fail|do not fail|will not fail|won't fail|never fails?|` +
		`does not make [^.]{0,20}fail|is not a failure|are not a failure|` +
		`is not considered a failure|is not treated as a failure|` +
		`no longer fails?|no longer a failure|no longer treated as a failure|no longer an error)`
	// Denial of the claim as a whole, ahead of it: "it is <…> that a non-zero exit fails".
	denialHead = `(no longer true|not true|untrue|no longer the case|not the case)`
	// Withdrawal ahead of the subject: "we <…> the task on a non-zero exit".
	//
	// Only "no longer", where the tail above takes every negation it is offered.
	// Ahead of the subject the others read the same as prose that states the
	// contract correctly and only scopes the non-failure to a condition — "the task
	// does not fail, whatever the exit code was" and "a declared code will not fail
	// the task, so `exit_codes: [0, 75]` accepts 75" are both how the waiver and the
	// accepted set get written up, and both would red. "no longer" is the one word
	// that means a change away from the current behaviour rather than a condition on
	// it, so it keeps the target and drops them. rejectedSubject above is the same
	// cut made from the other end: those two sentences red just as readily with
	// their clauses swapped, and it is the subject rather than the negation that
	// tells them apart in that order. Two residuals, deliberate and small: an adverb
	// between the words ("no longer fails SILENTLY on a non-zero exit" is about the
	// new behaviour and would red), and a denial split across a full stop, which is
	// left alone because joining sentences is what `[^.]` exists to prevent.
	denialVerbFirst = `no longer fails?`
)

var reNoExitCodeDefaultClaim = regexp.MustCompile(
	rejectedSubject + `[^.]{0,60}` + denialTail +
		`|` + denialHead + `[^.]{0,80}` + denialSubject + `[^.]{0,60}fail` +
		`|` + denialVerbFirst + `[^.]{0,60}` + denialSubject)

// verbProbe — how to make one verb module run a process that starts cleanly and
// exits non-zero, with no `exit_codes` param, so the default is what decides.
//
// It takes two runs. A module that fails on the code answers with a `failed` event,
// and so does a module whose executable would not launch; the difference matters,
// because only the first is the contract under test. okParams runs the same command
// exiting 0 first: once that comes back clean, the params are valid and the process
// does start on this box, so the only thing that differs on the failing run is the
// exit code itself.
type verbProbe struct {
	okParams   map[string]any
	failParams map[string]any
	wantCode   float64
}

// verbProbes is keyed by the FULL address from [coremanifest.VerbShellModules].
// A new verb module with no entry here fails the guard instead of silently
// narrowing it to the members someone remembered to list.
var verbProbes = map[string]verbProbe{
	// argv, no shell: `sh -c "exit N"` as three argv tokens.
	"core.exec.run": {
		okParams:   map[string]any{"cmd": "sh", "args": []any{"-c", "exit 0"}},
		failParams: map[string]any{"cmd": "sh", "args": []any{"-c", "exit 3"}},
		wantCode:   3,
	},
	// shell string, the module supplies `sh -c` itself.
	"core.cmd.shell": {
		okParams:   map[string]any{"cmd": "exit 0"},
		failParams: map[string]any{"cmd": "exit 3"},
		wantCode:   3,
	},
}

// normativeSurface — a file that tells somebody what a non-zero exit does.
// docs/destiny/tasks.md is the section the guard above pins in detail; these are
// the rest, and they are here because pinning one of them is pinning none — no
// count is given on purpose, since a stale one is what invites the next author
// to add a surface below without wondering whether it is covered. The
// pre-NIM-687 lie did not live in one file: it was in both module READMEs, in
// the scenario guide, in templating.md, and the operator-facing surfaces said
// nothing at all. Reverting the default while leaving any one of them claiming
// the old contract re-creates exactly the gap the incident came through — and a
// guard reading a single file would stay green through it.
type normativeSurface struct {
	// path is relative to this package directory (docs/ and keeper/ are outside
	// the soul module — a filesystem path, not a Go import).
	path string
	// markers are lowercased, whitespace-collapsed substrings that state the
	// post-NIM-687 contract. Required while the modules fail on a rejected code,
	// and required to be GONE when they do not: the marker is what makes each
	// surface track the modules in both directions rather than merely mention
	// exit codes. Reword the doc and the guard reds until the marker is reworded
	// with it, deliberately.
	markers []string
}

// normativeSurfaces — the audiences, one entry each: scenario authors (the
// scenario guide, templating), destiny authors (the module READMEs, the module
// index), operators driving the Errand and MCP surfaces, and the ADR that
// records the contract change itself. `soul-lint` has no prose of its own here;
// it validates params, not this default.
var normativeSurfaces = []normativeSurface{
	{
		path:    "../../../docs/scenario/orchestration.md",
		markers: []string{"a probe that exits non-zero fails its host by default"},
	},
	{
		path:    "../../../docs/templating.md",
		markers: []string{"whose accepted codes already default to `[0]`"},
	},
	{
		path:    "../../../docs/module/README.md",
		markers: []string{"success = the `exit_codes` set, default `[0]`"},
	},
	{
		path: "../../../docs/module/core/exec/README.md",
		markers: []string{
			"a command that ran and exited non-zero fails the task",
			"a code outside `exit_codes` fails the task",
		},
	},
	{
		path: "../../../docs/module/core/cmd/README.md",
		markers: []string{
			"any code outside the set fails the task",
			"a code outside `exit_codes` fails the task",
		},
	},
	{
		path:    "../../../docs/keeper/operator-api/errands.md",
		markers: []string{"a non-zero exit code is `status: failed`"},
	},
	{
		path:    "../../../docs/keeper/mcp-tools/souls.md",
		markers: []string{"a command that exits non-zero comes back `failed`"},
	},
	{
		// The MCP manifest ships its descriptions to the client as strings and has
		// no golden test of its own, so this is the only thing holding its copy of
		// the contract to the modules'. ASCII on purpose — the file is ASCII.
		path:    "../../../keeper/internal/mcp/manifest.go",
		markers: []string{"a command that ran and exited non-zero comes back failed"},
	},
	{
		path: "../../../docs/adr/0015-core-modules-mvp.md",
		markers: []string{
			"a non-zero code fails the task",
			"a non-zero exit now fails by default",
		},
	},
}

// forbiddenAnywhere — the exact sentences NIM-687 deleted, checked against every
// surface rather than the one each came from. Copying the old promise into a
// neighbouring file is the cheapest way to re-open the incident, and per-file
// scoping would not see it. Literals, not patterns: a reworded revival is the
// regex matchers' job, and these cannot false-positive on anything.
var forbiddenAnywhere = []string{
	// docs/module/core/exec/README.md, pre-NIM-687.
	"is **not** considered an error automatically",
	// docs/module/core/cmd/README.md, pre-NIM-687.
	"non-zero exit does not take a step by itself failed",
}

func TestVerbShellContract_NormativeSurfaces_TrackTheModules(t *testing.T) {
	nonZeroExitIsFailure := observeVerbShellFailure(t)

	for _, s := range normativeSurfaces {
		t.Run(s.path, func(t *testing.T) {
			text := normalisedFile(t, s.path)

			for _, marker := range s.markers {
				has := strings.Contains(text, marker)
				switch {
				case nonZeroExitIsFailure && !has:
					t.Errorf("the verb-shell modules fail the task on a code outside `exit_codes`, but "+
						"%s does not state %q.\nThis file is normative for one of the audiences that has "+
						"to know: silence here is how the contract and the docs came apart in the first "+
						"place. Restore the statement — rewording it means rewording the marker in "+
						"normativeSurfaces, deliberately.", s.path, marker)
				case !nonZeroExitIsFailure && has:
					t.Errorf("the verb-shell modules do NOT fail on a non-zero exit, but %s still states "+
						"%q.\nThe default was reverted and this surface kept the NIM-687 promise. Believing "+
						"it, an author omits failed_when: and a real failure is recorded as CHANGED. Either "+
						"restore the default in the modules, or withdraw the ADR-015 amendment and rewrite "+
						"every surface in normativeSurfaces.", s.path, marker)
				}
			}

			for _, forbidden := range forbiddenAnywhere {
				if strings.Contains(text, forbidden) {
					t.Errorf("%s contains the deleted pre-NIM-687 sentence %q.\nIt says a non-zero exit is "+
						"not a failure, which stopped being true; sitting next to the correct sentence it "+
						"is worse than either alone, because whichever one the reader finds first wins.",
						s.path, forbidden)
				}
			}

			if !nonZeroExitIsFailure {
				return
			}
			if m := reNoExitCodeDefaultClaim.FindString(contractSentences(text)); m != "" {
				t.Errorf("the verb-shell modules fail on a code outside `exit_codes`, but %s denies an "+
					"exit-code default in %q.\nThat is the pre-NIM-687 promise in new wording. If the "+
					"sentence is about something else, say so in words the matcher does not read as a "+
					"denial rather than widening the matcher.", s.path, m)
			}
		})
	}
}

// observeVerbShellFailure runs every registered verb-shell module against a
// process that starts cleanly and exits non-zero, with no `exit_codes` given,
// and returns what the default did. It requires the members to AGREE: the two
// modules are one contract by construction (coremanifest.verbShellModules), and
// a split — the knob added to one manifest and not the other — is the specific
// failure the closed set exists to prevent, so it fails here rather than being
// averaged into a single answer the doc assertions would then be checked against.
func observeVerbShellFailure(t *testing.T) bool {
	t.Helper()

	reg := coremod.Default(installmod.Deps{})
	verbs := coremanifest.VerbShellModules()
	if len(verbs) == 0 {
		t.Fatal("coremanifest.VerbShellModules() is empty — the guard would assert nothing")
	}

	var first string
	var answer bool
	for _, full := range verbs {
		probe, ok := verbProbes[full]
		if !ok {
			t.Fatalf("verb module %q has no entry in verbProbes — see the message in "+
				"TestVerbModules_NonZeroExit_MatchesDocumentedContract", full)
		}
		dot := strings.LastIndex(full, ".")
		if dot < 0 {
			t.Fatalf("verb module address %q is not `<namespace>.<name>.<state>`", full)
		}
		mod, ok := reg.Lookup(full[:dot])
		if !ok {
			t.Fatalf("registry has no %q (from verb address %q)", full[:dot], full)
		}
		state := full[dot+1:]
		// Control run first. A `failed` event says nothing about WHY on its own:
		// a rejected exit code and an executable that never launched look
		// identical from here. On a box where `sh` cannot be started at all,
		// every probe answers failed=true, the observation comes back "the
		// default rejects non-zero" for the wrong reason, and every assertion
		// below passes without the contract having been exercised once. The
		// clean exit is the witness that the shell runs.
		if ok := applyProbe(t, mod, state, probe.okParams); ok.GetFailed() {
			t.Fatalf("%s reports failed for a command that exits 0: %q.\nThe non-zero probe cannot then "+
				"mean what this test reads into it — every answer would be `failed` whatever the code.",
				full, ok.GetMessage())
		}
		failEv := applyProbe(t, mod, state, probe.failParams)
		// The observation is "a code outside the set is rejected", not "this
		// module fails a lot": a module that failed 0 as well would satisfy a
		// bare GetFailed() while the documented default had become "everything
		// fails", and the marker assertions would keep vouching for prose that
		// no longer describes it.
		if failEv.GetFailed() {
			if code := failEv.GetOutput().GetFields()["exit_code"].GetNumberValue(); code != probe.wantCode {
				t.Fatalf("%s failed the non-zero probe but reports exit_code=%v, want %v — the failure is "+
					"not the one this test claims to observe", full, code, probe.wantCode)
			}
		}
		got := failEv.GetFailed()
		if first == "" {
			first, answer = full, got
			continue
		}
		if got != answer {
			t.Fatalf("%s reports failed=%v for a non-zero exit but %s reports failed=%v.\nThe verb-shell "+
				"modules are one contract (coremanifest.verbShellModules) and no document can describe "+
				"both at once — the `exit_codes` default belongs to the set, not to a module.",
				first, answer, full, got)
		}
	}
	return answer
}

// normalisedFile reads a whole file lowercased with whitespace collapsed to
// single spaces, so a reflow of a paragraph cannot hide a claim from a matcher
// by wrapping it across two lines. Fenced blocks are dropped, as in
// failedWhenProse: a marker is a claim made TO A READER, and the same words
// inside a YAML example are a sample, not a promise. Keeping fences would let
// an author delete the sentence and leave the guard green on an example that
// says nothing normative — the exact drift this test exists to catch.
func normalisedFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\nThe file is normative for the verb-shell exit-code contract. If it moved, "+
			"move the entry in normativeSurfaces with it; if it is gone, its audience needs another one.",
			path, err)
	}

	var kept []string
	inFence := false
	for _, line := range strings.Split(string(raw), "\n") {
		l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">"))
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			kept = append(kept, l)
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.Join(kept, "\n")), " "))
}

// The markers are plain substrings, so without this the cheapest way to satisfy
// the guard while gutting the documentation is to delete the sentence and leave
// a YAML sample that happens to contain the same words. That is the drift the
// coupler exists to catch, dressed as a passing test.
func TestNormalisedFile_FencedSampleIsNotAClaim(t *testing.T) {
	const marker = "a code outside `exit_codes` fails the task"
	path := filepath.Join(t.TempDir(), "surface.md")
	body := "# Surface\n\nSee the example.\n\n```yaml\n# " + marker + "\n- module: core.cmd.shell\n```\n\nNothing else.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := normalisedFile(t, path); strings.Contains(got, strings.ToLower(marker)) {
		t.Errorf("normalisedFile kept %q from inside a fenced block.\nThe marker assertions would then "+
			"vouch for a file whose prose no longer states the contract at all — a sample is not a "+
			"promise to the reader.", marker)
	}
}

// Both matchers are dormant on a green tree: reNoExitCodeDefaultClaim only ever
// reports when someone writes a denial, and reExitCodeDefaultClaim runs only in
// the reverted branch, which no passing build reaches. A matcher nothing exercises
// is a matcher whose holes are found by the incident, so its inputs are pinned
// here. The negatives matter as much as the positives: one that reds on the
// correct prose gets widened by the next author until it matches nothing, which
// costs the same as deleting it and looks like maintenance.
func TestNoExitCodeDefaultClaim_CatchesBothDirectionsOfDenial(t *testing.T) {
	denials := []struct {
		name string
		text string
		deny bool
	}{
		// The old promise restated. These also lose the marker, so the assertions
		// in the surfaces test catch them too — belt and braces, on purpose.
		{"old promise", "a non-zero exit does not fail the task", true},
		{"old promise, plural", "non-zero exit codes do not fail the step", true},
		{"old promise, never", "a non-zero exit never fails a task by itself", true},
		// The new contract negated. The marker survives inside every one of these,
		// so this branch is the only thing standing between them and a green run.
		{"negated, prefix", "it is no longer true that a code outside `exit_codes` fails the task", true},
		{"negated, suffix", "a non-zero exit no longer fails the task", true},
		{"negated, noun", "a non-zero exit code is no longer a failure", true},
		{"negated, not the case", "it is not the case that a non-zero exit fails the task", true},
		{"negated, treated", "a non-zero exit is no longer treated as a failure", true},
		{"negated, error", "a non-zero exit code is no longer an error", true},
		{"denial, will not", "a non-zero exit will not fail the task", true},
		{"denial, considered", "a non-zero exit is not considered a failure", true},
		{"denial, contraction", "a non-zero exit won't fail the task", true},
		{"denial, does not make", "a non-zero exit does not make the task fail", true},
		// Withdrawal ahead of subject. Neither of the first two branches reads this order.
		{"denial, verb first", "we no longer fail the task on a non-zero exit", true},
		// The subject naming a code the contract turned down. Every other positive
		// here reaches the first branch through "non-zero", so without these the
		// rest of rejectedSubject would be unexercised guesswork.
		{"denial, code outside the set", "a code outside `exit_codes` does not fail the task", true},
		{"denial, code not listed", "a code not listed in `exit_codes` does not fail the task", true},
		{"denial, the module's own rejection", "an exit code the module rejects is not a failure", true},
		// The one that mentions the guards. contractSentences used to drop every
		// fragment containing "skip" or "guard", which hid exactly this: a denial
		// carrying both required markers, invisible to the branch added to catch it.
		{"negated, and blames the guards", "it is no longer true that a code outside `exit_codes` fails the task; the guards decide", true},
		// The prose the surfaces actually carry.
		{"the contract", "a command that ran and exited non-zero fails the task", false},
		{"the contract, adr", "a non-zero exit now fails by default", false},
		{"the set", "success = the `exit_codes` set, default `[0]`", false},
		{"the escape hatch", "`failed_when: false` waives it and the task is recorded ok", false},
		{"a negation about something else", "it is no longer true that the incarnation name is a coven", false},
		// Sentence scoping: the negation and the claim are adjacent but separate
		// sentences. Without `[^.]` the second branch would join them and red on a
		// file that states the contract correctly right after an unrelated denial.
		{"negation in the previous sentence", "it is no longer true that the name is a coven. a command that exited non-zero fails the task", false},
		// The sentence contractSentences exists for, verbatim from both module
		// READMEs and ADR-0015. It is a statement about `creates`/`unless`/`onlyif`
		// and it is true; without the exclusion three surfaces red on correct prose.
		{"the skip branch", "a skipped task reports `exit_code: 0` and never fails", false},
		{"the skip branch, exit_codes named", "the guards run first, so a skipped task never fails whatever `exit_codes` says", false},
		// Correct prose that scopes the non-failure to a condition. These are how the
		// waiver and the accepted-set both get written up, so they are the sentences
		// these docs will acquire more of, not fewer. An earlier draft of the
		// withdrawal branch took any negated "fail" near an exit code and reddened
		// every one of them; they are pinned here so re-widening it is not silent.
		{"scoped by the set", "the run does not fail on a non-zero exit that `exit_codes` allows", false},
		{"scoped by a declared code", "a declared code will not fail the task, so `exit_codes: [0, 75]` is how you accept 75", false},
		{"scoped by failed_when", "with `failed_when:` set the task does not fail, whatever the exit code was", false},
		{"scoped by the waiver", "a waived task does not fail the run; the exit code is still recorded", false},
		{"scoped by a timeout", "a `timed_out` errand does not fail the scenario, and its exit code is absent", false},
		// The same scoped prose with its clauses swapped. The five above put the
		// negation first and were safe for that reason alone: the subject-first
		// branch read `exit[ _-]?code`, which is a prefix of the parameter's own
		// name, so it reddened on any sentence that named the parameter and said
		// something did not fail — the commoner order, since English puts the
		// subject first. These pin the class rather than the word order.
		{"scoped by the set, subject first", "a code listed in `exit_codes` does not fail the task", false},
		{"scoped by a declared code, subject first", "with `exit_codes: [0, 75]` a declared code will not fail the task", false},
		{"scoped by failed_when, subject first", "whatever the exit code, a task with `failed_when: false` does not fail", false},
		{"the set explained", "`exit_codes` lists the codes that do not fail the run", false},
		{"the set explained, by example", "`exit_codes: [0, 2]` means exit 2 does not fail the task", false},
	}
	for _, tc := range denials {
		t.Run(tc.name, func(t *testing.T) {
			m := reNoExitCodeDefaultClaim.FindString(contractSentences(tc.text))
			switch {
			case tc.deny && m == "":
				t.Errorf("reNoExitCodeDefaultClaim does not read %q as a denial.\nA surface can carry this "+
					"sentence, keep every required marker, and tell the reader a non-zero exit is safe to "+
					"ignore — which is the state the incident came out of.", tc.text)
			case !tc.deny && m != "":
				t.Errorf("reNoExitCodeDefaultClaim reads %q as a denial (matched %q).\nThis is prose the "+
					"surfaces are required to carry, so the matcher reds on a correct file and the next "+
					"author narrows it until it catches nothing.", tc.text, m)
			}
		})
	}

	// The mirror, consulted only where the modules stopped failing: there the same
	// sentences flip roles, and a doc still promising the default is the lie in the
	// other direction.
	claims := []struct {
		name  string
		text  string
		claim bool
	}{
		{"the contract", "the default is exit code 0 and anything else fails", true},
		{"defaults to", "`exit_codes` defaults to `[0]`, so a non-zero exit fails", true},
		{"withdrawn", "a non-zero exit is not a failure", false},
		{"unrelated default", "the default retry count is three", false},
	}
	for _, tc := range claims {
		t.Run("mirror/"+tc.name, func(t *testing.T) {
			if got := reExitCodeDefaultClaim.MatchString(tc.text); got != tc.claim {
				t.Errorf("reExitCodeDefaultClaim.MatchString(%q)=%v want %v.\nThis matcher only runs once "+
					"the default has been reverted, so nothing else will find its holes first.", tc.text, got, tc.claim)
			}
		})
	}
}

// reSkipSentence matches the one true sentence that reads to the denial matcher
// exactly like a denial: a SKIPPED task "reports `exit_code: 0` and never fails",
// said of `creates`/`unless`/`onlyif` in both module READMEs and in ADR-0015.
// It has to be excluded, and it has to be excluded by its shape rather than by
// its letters. Dropping every fragment that merely contains "skip" or "guard"
// also drops "it is no longer true that a code outside `exit_codes` fails the
// task; the guards decide" — a denial that keeps every required marker, so the
// matcher below is the only thing standing between it and a green run, and a
// filter wide enough to hide it defeats the branch that exists to catch it.
// The remaining hole is a revival of the old promise phrased around a skipped
// task specifically, which is a much narrower thing to have written by accident.
var reSkipSentence = regexp.MustCompile(`skipped task`)

// contractSentences removes those sentences before the denial matcher runs.
func contractSentences(text string) string {
	var kept []string
	for _, s := range strings.Split(text, ".") {
		if reSkipSentence.MatchString(s) {
			continue
		}
		kept = append(kept, s)
	}
	return strings.Join(kept, ".")
}

func TestVerbModules_NonZeroExit_MatchesDocumentedContract(t *testing.T) {
	prose := failedWhenProse(t)
	reg := coremod.Default(installmod.Deps{})

	verbs := coremanifest.VerbShellModules()
	if len(verbs) == 0 {
		t.Fatal("coremanifest.VerbShellModules() is empty — the guard would assert nothing")
	}

	for _, full := range verbs {
		t.Run(full, func(t *testing.T) {
			probe, ok := verbProbes[full]
			if !ok {
				t.Fatalf("verb module %q has no entry in verbProbes: a verb module was added without "+
					"extending this guard, leaving its failed/exit_code contract undocumented and "+
					"unchecked. Add a probe that exits non-zero.", full)
			}

			dot := strings.LastIndex(full, ".")
			if dot < 0 {
				t.Fatalf("verb module address %q is not `<namespace>.<name>.<state>`", full)
			}
			modName, state := full[:dot], full[dot+1:]

			mod, ok := reg.Lookup(modName)
			if !ok {
				t.Fatalf("registry has no %q (from verb address %q)", modName, full)
			}

			// Control run: the same command, exiting 0. It is what makes the failing
			// run below readable — a `failed` event on its own looks the same whether
			// the code was rejected or the executable would not launch. Once this run
			// comes back clean, the params are valid and the process does start here,
			// so the only thing left to explain a failure is the code.
			okEv := applyProbe(t, mod, state, probe.okParams)
			if okEv.GetFailed() {
				t.Fatalf("control probe (exit 0) reported failed=true, message=%q — the params are wrong "+
					"or the box cannot run the command at all, so the non-zero run would prove nothing "+
					"about the failed/exit_code contract", okEv.GetMessage())
			}
			if code := okEv.GetOutput().GetFields()["exit_code"].GetNumberValue(); code != 0 {
				t.Fatalf("control probe (exit 0) returned exit_code = %v, want 0 — the probe is not "+
					"running the process it thinks it is", code)
			}

			ev := applyProbe(t, mod, state, probe.failParams)
			nonZeroExitIsFailure := ev.GetFailed()

			// Either way the code must be visible in the output — on success because
			// that is where a scenario reads it, on failure because a verdict without
			// the stderr that explains it is worse than no verdict. The control run
			// above already proved the command runs here, so a missing exit_code is
			// the module dropping it, not the box.
			if code := ev.GetOutput().GetFields()["exit_code"].GetNumberValue(); code != probe.wantCode {
				if nonZeroExitIsFailure {
					t.Fatalf("%s failed the task on a non-zero exit but its final event carries "+
						"exit_code = %v, want %v.\nThe failure must go out through "+
						"util.SendFailedWithOutput, not the bare util.SendFailed: dropping the output "+
						"deletes the stderr this default exists to surface and makes a predicate over "+
						"register.self.exit_code unevaluable.", full, code, probe.wantCode)
				}
				t.Fatalf("exit_code = %v, want %v — the probe did not run a process exiting non-zero, "+
					"so this test would assert nothing about the failed/exit_code contract",
					code, probe.wantCode)
			}
			if nonZeroExitIsFailure {
				for _, key := range []string{"stdout", "stderr"} {
					if _, ok := ev.GetOutput().GetFields()[key]; !ok {
						t.Errorf("%s failed the task on a non-zero exit but its final event has no %q "+
							"field — the failure lost the output it must carry "+
							"(util.SendFailedWithOutput).", full, key)
					}
				}
			}

			assertProseMatchesBehaviour(t, full, nonZeroExitIsFailure, prose)
		})
	}
}

// applyProbe runs one state of one module against the real production runner and
// returns its final event.
func applyProbe(t *testing.T, mod module.SoulModule, state string, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()

	p, err := structpb.NewStruct(params)
	if err != nil {
		t.Fatalf("structpb.NewStruct(%v): %v", params, err)
	}
	stream := &internaltest.ApplyStream{}
	if err := mod.Apply(&pluginv1.ApplyRequest{State: state, Params: p}, stream); err != nil {
		t.Fatalf("Apply(%s, %v): %v", state, params, err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatalf("Apply(%s, %v) sent no final event", state, params)
	}
	return ev
}

// assertProseMatchesBehaviour requires the normative prose to state the contract
// that was just observed. Both directions red, which is the point: the guard has
// no opinion of its own about which contract is right, only that one document and
// one implementation may not each claim a different one.
func assertProseMatchesBehaviour(t *testing.T, module string, nonZeroExitIsFailure bool, prose string) {
	t.Helper()

	if nonZeroExitIsFailure {
		// Observed contract (NIM-687): a process that ran and exited outside
		// `exit_codes` fails the task, and the default set is `[0]`.
		if !strings.Contains(prose, claimExitCodeDefault) {
			t.Errorf("%s reports failed=true for a process that ran and exited non-zero, but the "+
				"failed_when: section of %s never states %q.\nThat section is normative for scenario "+
				"authors: an undocumented exit-code default turns working scenarios red with no "+
				"explanation anywhere. Restore the statement (rewording it means rewording "+
				"claimExitCodeDefault here too, deliberately).",
				module, failedWhenDocPath, claimExitCodeDefault)
		}
		if strings.Contains(prose, claimNoExitCodeDefault) {
			t.Errorf("%s reports failed=true for a non-zero exit, but %s still states %q — the "+
				"pre-NIM-687 prose survived the change and now contradicts the code.",
				module, failedWhenDocPath, claimNoExitCodeDefault)
		} else if m := reNoExitCodeDefaultClaim.FindString(prose); m != "" {
			t.Errorf("%s reports failed=true for a non-zero exit, but %s denies an exit-code default "+
				"in %q.\nThat is the pre-NIM-687 promise in a new wording, and believing it a scenario "+
				"author omits both exit_codes: and failed_when: — then a step that used to pass turns "+
				"red with the doc saying it should not. State the module's real contract instead.",
				module, failedWhenDocPath, m)
		}
		if !strings.Contains(prose, claimOutputSurvives) {
			t.Errorf("%s fails the task on a rejected exit code and its event carries the output, but "+
				"%s never states %q.\nUndocumented, that is the difference between a scenario author "+
				"writing a `failed_when:` predicate over register.self.exit_code and assuming the "+
				"fields are gone on failure.", module, failedWhenDocPath, claimOutputSurvives)
		}
		return
	}

	// The modules no longer fail on a non-zero exit — the default was reverted. That
	// is a contract change back, not a bug fix: it re-opens the incident this guard
	// is named after, so it needs the ADR-015 amendment withdrawn and the prose
	// rewritten to match. A green test is not the approval.
	if strings.Contains(prose, claimExitCodeDefault) {
		t.Errorf("%s does NOT report failed for a process that ran and exited non-zero (the exit code "+
			"goes to register.self.exit_code and the task ends CHANGED), but %s claims %q.\nThat is the "+
			"NIM-687 promise again: believing it, a scenario author omits failed_when: and a genuine "+
			"failure is recorded as CHANGED, so the run dies later on a different task carrying the "+
			"wrong reason. Either restore the exit-code default in the modules, or withdraw the "+
			"ADR-015 amendment and rewrite the prose.", module, failedWhenDocPath, claimExitCodeDefault)
	} else if m := reExitCodeDefaultClaim.FindString(prose); m != "" {
		t.Errorf("%s does NOT report failed for a process that ran and exited non-zero, but %s asserts "+
			"an exit-code default in %q.\nSame promise in a different wording, and just as dangerous.",
			module, failedWhenDocPath, m)
	}

	if !strings.Contains(prose, claimNoExitCodeDefault) {
		t.Errorf("%s does NOT report failed for a non-zero exit, but the failed_when: section of %s "+
			"does not state %q either.\nThe section is normative for scenario authors and must say "+
			"plainly which contract holds — silence is what let NIM-687 happen.",
			module, failedWhenDocPath, claimNoExitCodeDefault)
	}
}

// failedWhenProse returns the `### `failed_when:“ section of
// [failedWhenDocPath]: lowercased, blockquote markers unwrapped, fenced code
// blocks dropped, whitespace collapsed to single spaces.
//
// Fences go because they hold literal YAML the author is meant to copy — the
// section legitimately shows `exit_codes: [0, 1]` and `failed_when: "false"` as the
// opt-outs, and those are RECOMMENDED FIXES, not claims about the default.
// Whitespace is collapsed so a future reflow of the paragraph cannot smuggle a
// claim past the matchers by wrapping it across two lines.
func failedWhenProse(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(failedWhenDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", failedWhenDocPath, err)
	}

	const header = "### `failed_when:`"
	start := strings.Index(string(raw), header)
	if start < 0 {
		t.Fatalf("%s has no %q section — the guard cannot find the prose it pins, so it would pass "+
			"vacuously. Was the section renamed or moved?", failedWhenDocPath, header)
	}
	body := string(raw)[start+len(header):]
	if end := strings.Index(body, "\n### "); end >= 0 {
		body = body[:end]
	}

	var kept []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">"))
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			kept = append(kept, l)
		}
	}

	prose := strings.ToLower(strings.Join(strings.Fields(strings.Join(kept, " ")), " "))
	if prose == "" {
		t.Fatalf("the %q section of %s has no prose outside code fences", header, failedWhenDocPath)
	}
	return prose
}
