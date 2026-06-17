// Package main — entrypoint бинаря `soul-trial` ([ADR-004], [ADR-023]).
//
// Офлайн-раннер испытаний (Trial) Destiny/Scenario. Второй бинарь-артефакт
// модуля `keeper` (параллель cmd/keeper): импортит keeper/internal/render +
// keeper/internal/trial напрямую (вариант A раскладки, ADR-023).
//
// Subcommand-router на stdlib `flag` (стиль cmd/keeper):
//
//	soul-trial run <case-file-or-dir>   прогон L0-испытаний
//	soul-trial help                     показать справку
//
// Пилот: только уровень L0 (render-only, hermetic) и только секция
// assert.rendered_tasks. Exit 0 при pass всех кейсов, 1 при fail/ошибке,
// 2 при usage-ошибке.
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

// runTrial парсит аргументы, прогоняет L0-испытания и печатает текст-таблицу
// с trial coverage. Exit 0 — все кейсы pass; 1 — есть fail или ошибка прогона.
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
		fmt.Fprintln(os.Stderr, "soul-trial run: ровно один аргумент-путь обязателен")
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
