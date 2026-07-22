// soul-lint is the offline linter for Destiny / Essence / Soul Stack
// configs, and the scaffold tool for SoulModule plugin authors.
//
// MVP subcommand set:
//
//	validate-config   <path> [--json]  validate keeper.yml or soul.yml.
//	validate-destiny  <path> [--json]  validate destiny.yml (the destiny
//	                                    root manifest).
//	validate-service  <path> [--json]  validate service.yml (the service
//	                                    root manifest).
//	validate-scenario <path> [--json]  validate scenario/<name>/main.yml.
//	validate-manifest <path> [--json]  validate a plugin's manifest.yaml
//	                                    (kind: soul_module / cloud_driver /
//	                                    ssh_provider).
//	plugin-init       <namespace>/<name> [flags]  scaffold a new SoulModule
//	                                    plugin (ADR-016 amendment 2026-05-27).
//
// Exit codes: 0 = ok, 1 = has errors, 2 = I/O fatal / usage.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/souls-guild/soul-stack/soul-lint/internal/plugininit"
	"github.com/souls-guild/soul-stack/soul-lint/internal/validate"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	switch sub {
	case "validate-config":
		os.Exit(runSubcommand(sub, "validate-config <path> [--json]", validate.KindConfig, os.Args[2:]))
	case "validate-destiny":
		os.Exit(runSubcommand(sub, "validate-destiny <path> [--json]", validate.KindDestiny, os.Args[2:]))
	case "validate-service":
		os.Exit(runSubcommand(sub, "validate-service <path> [--json]", validate.KindService, os.Args[2:]))
	case "validate-scenario":
		os.Exit(runSubcommand(sub, "validate-scenario <path> [--json]", validate.KindScenario, os.Args[2:]))
	case "validate-manifest":
		os.Exit(runSubcommand(sub, "validate-manifest <path> [--json]", validate.KindManifest, os.Args[2:]))
	case "plugin-init":
		os.Exit(runPluginInit(os.Args[2:]))
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "soul-lint: unknown subcommand %q\n\n", sub)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

// runSubcommand parses flags and the positional <path>. Same shape across
// all validate-* subcommands (spec M1.2.a, symmetric with M0).
func runSubcommand(sub, usage string, kind validate.Kind, args []string) int {
	usageLine := "Usage: soul-lint " + usage
	var (
		jsonOut bool
		path    string
	)
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		case "-h", "--help":
			fmt.Fprintln(os.Stdout, usageLine)
			return 0
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "soul-lint %s: unknown flag %q\n", sub, a)
				return 2
			}
			if path != "" {
				fmt.Fprintln(os.Stderr, usageLine)
				return 2
			}
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, usageLine)
		return 2
	}
	return validate.Run(validate.Options{
		Path: path,
		JSON: jsonOut,
		Kind: kind,
	}, os.Stdout, os.Stderr)
}

// runPluginInit parses flags for `plugin-init <namespace>/<name> [flags]`.
// Its argument style mirrors validate-* (manual argparse, no cobra).
func runPluginInit(args []string) int {
	const usageLine = "Usage: soul-lint plugin-init <namespace>/<name> [--out DIR] [--description TEXT] [--author NAME] [--force]"
	var (
		spec        string
		out         string
		description string
		author      string
		force       bool
	)
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--force":
			force = true
			i++
		case "-h", "--help":
			fmt.Fprintln(os.Stdout, usageLine)
			return 0
		case "--out", "--description", "--author":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "soul-lint plugin-init: flag %s requires a value\n", a)
				return 2
			}
			switch a {
			case "--out":
				out = args[i+1]
			case "--description":
				description = args[i+1]
			case "--author":
				author = args[i+1]
			}
			i += 2
		default:
			if strings.HasPrefix(a, "--out=") {
				out = strings.TrimPrefix(a, "--out=")
				i++
				continue
			}
			if strings.HasPrefix(a, "--description=") {
				description = strings.TrimPrefix(a, "--description=")
				i++
				continue
			}
			if strings.HasPrefix(a, "--author=") {
				author = strings.TrimPrefix(a, "--author=")
				i++
				continue
			}
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "soul-lint plugin-init: unknown flag %q\n", a)
				return 2
			}
			if spec != "" {
				fmt.Fprintln(os.Stderr, usageLine)
				return 2
			}
			spec = a
			i++
		}
	}
	if spec == "" {
		fmt.Fprintln(os.Stderr, usageLine)
		return 2
	}
	ns, nm, err := plugininit.ParseSpec(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul-lint plugin-init: %v\n", err)
		return 2
	}
	return plugininit.Run(plugininit.Options{
		Namespace:   ns,
		Name:        nm,
		Out:         out,
		Description: description,
		Author:      author,
		Force:       force,
	}, os.Stdout, os.Stderr)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "Usage: soul-lint <command> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  validate-config   <path> [--json]              validate keeper.yml or soul.yml")
	fmt.Fprintln(w, "  validate-destiny  <path> [--json]              validate destiny.yml manifest")
	fmt.Fprintln(w, "  validate-service  <path> [--json]              validate service.yml manifest")
	fmt.Fprintln(w, "  validate-scenario <path> [--json]              validate scenario/<name>/main.yml")
	fmt.Fprintln(w, "  validate-manifest <path> [--json]              validate plugin manifest.yaml")
	fmt.Fprintln(w, "  plugin-init       <namespace>/<name> [flags]   scaffold a new SoulModule plugin")
}
