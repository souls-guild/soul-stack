// Package main -- entrypoint of the `soul-trial` binary ([ADR-004], [ADR-023]).
//
// An offline Trial runner for Destiny/Scenario. The second binary artifact
// of the `keeper` module (parallel to cmd/keeper): imports keeper/internal/render +
// keeper/internal/trial directly (layout variant A, ADR-023).
//
// Subcommand router on stdlib `flag` (cmd/keeper style):
//
//	soul-trial run <case-file-or-dir>   run L0 trials
//	soul-trial help                     show help
//
// `run` takes `--modules <alias>=<path>`, repeatable, spelled exactly as
// `soul-lint` spells it and parsed by the same loader ([definition.LoadSchemas]).
// With it, the `params:` of a plugin step in a case's scenario or destiny are
// checked against the module's schema document; without it they cannot be, and
// every such step is reported under its case as `plugin_params_unchecked` rather
// than passing in silence (NIM-790). Core needs no binding — its manifests are
// compiled in.
//
// Pilot: only level L0 (render-only, hermetic) and only the
// assert.rendered_tasks section. Exit 0 when all cases pass, 1 on fail/error,
// 2 on a usage error — including a `--modules` binding that cannot be read, which
// is fatal rather than a downgrade to "unchecked": the operator asked for that
// check by naming the alias.
//
// Soul trial command documentation link.
// Soul trial command documentation link.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
	"github.com/souls-guild/soul-stack/shared/definition"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "run":
		os.Exit(runTrial(args))
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "soul-trial: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `soul-trial — Soul Stack Trial runner (ADR-023).

Usage:
  soul-trial <command> [args]

Commands:
  run <path> [--modules ALIAS=PATH]...
               Run L0 trials. <path> = a case.yml, a case dir (tests/<case>/),
               or a tree searched recursively for case.yml.
  help         Show this message.

Flags:
  --modules <alias>=<path>  bind a plugin's schema document to the alias a task
                            addresses it by, so the params: of its steps are
                            checked. <path> is a schema.json, a stamped artifact,
                            or the dist/ dir holding one. Repeatable, one binding
                            per module. Without it a plugin step is reported as
                            plugin_params_unchecked rather than passing silently.

Levels: L0 (render-only, hermetic) only in this pilot.`)
}

// moduleBindings collects a repeatable `--modules <alias>=<path>`.
//
// A flag.Value rather than a comma-separated string: a path may contain a comma,
// and a binding silently truncated at one would bind the wrong document — which
// this flag exists to prevent, not to introduce. An empty value is kept rather
// than dropped so the loader refuses it by the same rule as every other malformed
// binding; `--modules=` meaning "check nothing" is the failure mode being removed.
type moduleBindings []string

func (m *moduleBindings) String() string { return strings.Join(*m, ",") }

func (m *moduleBindings) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// hoistFlags moves the flags ahead of the positional argument.
//
// stdlib `flag` stops parsing at the first non-flag word, so
// `run ./svc --modules redis=…` would silently leave the binding unparsed and
// then refuse the run for having two positionals — a message about the wrong
// thing. soul-lint accepts the flag on either side of the path (its own usage line
// puts it after), and an operator who learned `--modules` there must not have to
// learn a different word order here.
//
// This is a reordering, not a second parser: every argument still reaches the one
// FlagSet, which remains the only thing that decides what a flag means. Two forms
// need care, and both are about not letting the reordering INVENT a meaning:
//
//   - `--modules <value>` is two words, so the value travels with the flag. A
//     TRAILING `--modules` has no value to take, and taking the path instead would
//     turn "you forgot the binding" into "you passed no path" — a message about the
//     wrong argument. It is emitted alone, and the FlagSet says what is missing.
//   - `--` ends flag parsing, so everything after it is positional and the
//     separator itself is dropped. Hoisting it with the flags would put it BEFORE
//     the path and hide the path from the parser entirely.
func hoistFlags(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			return append(flags, positional...)
		case a == "--modules" || a == "-modules":
			flags = append(flags, a)
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

// runTrial parses arguments, runs L0 trials, and prints a text table with
// trial coverage. Exit 0 -- all cases pass; 1 -- there's a fail or a run error.
func runTrial(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: soul-trial run <case-file-or-dir> [--modules ALIAS=PATH]...")
	}
	var bindings moduleBindings
	fs.Var(&bindings, "modules", "bind a plugin schema document as <alias>=<path>; repeatable")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		// `-h` anywhere in the run line is a help request, not a usage error — the
		// FlagSet has already printed the flags. soul-lint exits 0 on its own
		// `--help`, and a help request that reports failure teaches an operator to
		// distrust the exit code.
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "soul-trial run: exactly one path argument is required")
		fs.Usage()
		return exitUsage
	}

	// Fail closed, exactly as soul-lint does: a binding the operator NAMED and that
	// cannot be read is a check they asked for and did not get, and running on to
	// print PASS would be the failure the flag exists to remove.
	// config.DiagPluginParamsUnchecked stays for the case it describes — a module
	// nobody bound at all.
	modules, err := definition.LoadSchemas(bindings)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul-trial run: %v\n", err)
		return exitUsage
	}

	results, err := trial.Run(context.Background(), fs.Arg(0), trial.Options{Modules: modules})
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul-trial run: %v\n", err)
		return exitError
	}

	allPass := printResults(os.Stdout, results)
	if !allPass {
		return exitError
	}
	return exitOK
}
