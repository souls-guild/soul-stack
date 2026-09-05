package main

// ★ NIM-790. The report must PRINT a case's notices — under a PASS above all.
//
// The finding this channel carries is `plugin_params_unchecked`: a part of the
// definition was never judged. A PASS with nothing beside it is exactly what
// "checked and clean" looks like, so a runner that produces the hint and prints
// only the PASS has told the operator the opposite of what it found. That is the
// defect this ticket removed, and it lived for the whole life of the harness.
//
// This is the LAST link of the chain, and until this file the only untested one:
// everything upstream is covered by `keeper/internal/trial`, but `printNotices` is
// what turns a Result field into something a human — or `make trial`'s
// false-green guard, which greps this very stdout — can see. Delete either
// `printNotices` call in report.go and this goes red; without it, that deletion
// leaves every Go test green AND makes the corpus gate pass by printing nothing
// for it to find.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// uncheckedNotice is the finding as `shared/config` builds it: a hint, at the file
// and line of the step nobody could check.
func uncheckedNotice() diag.Diagnostic {
	return diag.Diagnostic{
		Level:    diag.LevelHint,
		Phase:    diag.PhaseSemanticValidate,
		File:     "scenario/create/provision.yml",
		Line:     42,
		Code:     config.DiagPluginParamsUnchecked,
		Message:  "params of redis.acl were not checked: no module manifests were supplied",
		YAMLPath: "$.tasks[0].module",
	}
}

func TestPrintResults_PrintsNoticesUnderAPass(t *testing.T) {
	var b bytes.Buffer
	allPass := printResults(&b, []trial.Result{{
		Case: "c", Pass: true, Level: trial.LevelL0, Service: "redis", ServiceStated: true,
		Notices: []diag.Diagnostic{uncheckedNotice()},
	}})
	got := b.String()

	// A notice does not fail the run: nobody bound a manifest is not the case
	// author's defect. It is reported, not counted against them.
	if !allPass {
		t.Errorf("a notice failed the run; it is a hint, not a failure\n%s", got)
	}
	if !strings.Contains(got, "PASS") {
		t.Fatalf("expected a PASS line\n%s", got)
	}
	// The three things a reader acts on: what was not checked, where, and that it
	// is a hint rather than a defect.
	for _, want := range []string{
		config.DiagPluginParamsUnchecked,
		"scenario/create/provision.yml:42",
		string(diag.LevelHint),
		"params of redis.acl were not checked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not carry %q\n%s", want, got)
		}
	}
}

// An L1 case renders nothing, but it loads the same definition and can produce the
// same finding — and its branch of printResults returns early, which is how a
// second copy of this bug gets written.
func TestPrintResults_PrintsNoticesForAnL1Case(t *testing.T) {
	var b bytes.Buffer
	printResults(&b, []trial.Result{{
		Case: "m", Pass: true, Level: trial.LevelL1,
		Notices: []diag.Diagnostic{uncheckedNotice()},
	}})
	if got := b.String(); !strings.Contains(got, config.DiagPluginParamsUnchecked) {
		t.Errorf("an L1 case's notices are dropped by the report\n%s", got)
	}
}

// And a case with nothing to say says nothing. Without this the tests above pass
// on a report that prints a notice line unconditionally, which is the same false
// signal in the other direction.
func TestPrintResults_QuietWhenThereIsNothingToReport(t *testing.T) {
	var b bytes.Buffer
	printResults(&b, []trial.Result{{
		Case: "c", Pass: true, Level: trial.LevelL0, Service: "redis", ServiceStated: true,
	}})
	if got := b.String(); strings.Contains(got, config.DiagPluginParamsUnchecked) {
		t.Errorf("a clean case reported an unchecked module\n%s", got)
	}
}

// A notice with no position still names something openable rather than printing a
// bare colon: a cross-file finding loses its AST position, and the reader has to be
// sent somewhere.
func TestPrintResults_NoticeWithoutAPositionStillReadable(t *testing.T) {
	var b bytes.Buffer
	printResults(&b, []trial.Result{{
		Case: "c", Pass: true, Level: trial.LevelL0, Service: "redis", ServiceStated: true,
		Notices: []diag.Diagnostic{{
			Level: diag.LevelWarning, Code: "cross_file_finding", Message: "something about the whole plan",
		}},
	}})
	got := b.String()
	if !strings.Contains(got, "cross_file_finding") {
		t.Fatalf("a fileless notice was dropped\n%s", got)
	}
	if strings.Contains(got, " :") {
		t.Errorf("a fileless notice printed an empty position\n%s", got)
	}
}
