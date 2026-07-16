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
// Pilot: only level L0 (render-only, hermetic) and only the
// assert.rendered_tasks section. Exit 0 when all cases pass, 1 on fail/error,
// 2 on a usage error.
//
// [ADR-004]: docs/adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper
// [ADR-023]: docs/adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
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

func printUsage(w *os.File) {
	fmt.Fprintln(w, `soul-trial — Soul Stack Trial runner (ADR-023).

Usage:
  soul-trial <command> [args]

Commands:
  run <path>   Run L0 trials. <path> = a case.yml, a case dir (tests/<case>/),
               or a tree searched recursively for case.yml.
  help         Show this message.

Levels: L0 (render-only, hermetic) only in this pilot.`)
}

// runTrial parses arguments, runs L0 trials, and prints a text table with
// trial coverage. Exit 0 -- all cases pass; 1 -- there's a fail or a run error.
func runTrial(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: soul-trial run <case-file-or-dir>")
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "soul-trial run: exactly one path argument is required")
		fs.Usage()
		return exitUsage
	}

	results, err := trial.Run(context.Background(), fs.Arg(0))
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
