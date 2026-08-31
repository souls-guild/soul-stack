// soul-lint is the offline linter for Destiny / service vars / Soul Stack
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
//	validate-manifest <path> [--json]  validate a plugin's schema document
//	                                    (dist/schema.json, or a stamped
//	                                    artifact).
//	plugin-init       <namespace>/<name> [flags]  scaffold a new SoulModule
//	                                    plugin (ADR-016 amendment 2026-05-27).
//
// The validate-destiny / validate-service / validate-scenario subcommands also
// take `--modules <alias>=<path>`, repeatable (NIM-228, reshaped by NIM-377).
// With it, the `params:` of a plugin module are checked exactly as `core.*`
// already is; without it they cannot be, and each such module is reported as
// `plugin_params_unchecked` rather than passing in silence. Core needs no flag —
// its manifests are compiled in.
//
// The alias is on the flag because the artifact has no self-name: address level 1
// is the registration alias an operator picks, so `redis=./dist/schema.json`
// states the same word the task writes and the operator will register.
//
// `validate-scenario` also takes `--service-name <name>` (NIM-726) — the name the
// service is registered under, for the own-namespace Vault fence ([ADR-0083] §7).
// It is on the flag for the same reason the module alias is: nothing in a service
// repository states the name any more. Without it the fence cannot run, and says
// so (`own_namespace_fence_unchecked`) rather than passing in silence.
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
		os.Exit(runSubcommand(sub, "validate-destiny <path> [--json] [--modules ALIAS=PATH]...", validate.KindDestiny, os.Args[2:]))
	case "validate-service":
		os.Exit(runSubcommand(sub, "validate-service <path> [--json] [--modules ALIAS=PATH]...", validate.KindService, os.Args[2:]))
	case "validate-scenario":
		os.Exit(runSubcommand(sub, "validate-scenario <path> [--json] [--service-name NAME] [--modules ALIAS=PATH]...", validate.KindScenario, os.Args[2:]))
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
		jsonOut     bool
		path        string
		modules     []string
		serviceName string
		wantModule  bool // the previous arg was `--modules`, so this one is its value
		wantService bool // likewise for `--service-name`
	)
	for _, a := range args {
		if wantModule {
			modules = append(modules, a)
			wantModule = false
			continue
		}
		if wantService {
			serviceName = a
			wantService = false
			continue
		}
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stdout, usageLine)
			return 0
		case a == "--service-name" || a == "-service-name":
			wantService = true
		case strings.HasPrefix(a, "--service-name=") || strings.HasPrefix(a, "-service-name="):
			// Not repeatable: a service has one name. A second occurrence overwrites,
			// rather than being refused, for the same reason every other flag here does.
			_, value, _ := strings.Cut(a, "=")
			serviceName = value
		case a == "--modules" || a == "-modules":
			wantModule = true
		case strings.HasPrefix(a, "--modules=") || strings.HasPrefix(a, "-modules="):
			// Repeatable: one binding per occurrence, appended in order. An empty
			// value is kept rather than dropped so LoadModuleSchemas rejects it by
			// the same rule as every other malformed binding — silently ignoring
			// `--modules=` would mean "check nothing", which is the failure mode
			// this flag exists to remove.
			_, value, _ := strings.Cut(a, "=")
			modules = append(modules, value)
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "soul-lint %s: unknown flag %q\n", sub, a)
			return 2
		default:
			if path != "" {
				fmt.Fprintln(os.Stderr, usageLine)
				return 2
			}
			path = a
		}
	}
	if wantModule {
		fmt.Fprintf(os.Stderr, "soul-lint %s: --modules needs an <alias>=<path> binding\n", sub)
		return 2
	}
	if wantService {
		fmt.Fprintf(os.Stderr, "soul-lint %s: --service-name needs a <name>\n", sub)
		return 2
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, usageLine)
		return 2
	}
	return validate.Run(validate.Options{
		Path:        path,
		JSON:        jsonOut,
		Kind:        kind,
		Modules:     modules,
		ServiceName: serviceName,
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
	fmt.Fprintln(w, "  validate-destiny  <path> [--json] [--modules A=P]...  validate destiny.yml manifest")
	fmt.Fprintln(w, "  validate-service  <path> [--json] [--modules A=P]...  validate service.yml manifest")
	fmt.Fprintln(w, "  validate-scenario <path> [--json] [--service-name N] [--modules A=P]...  validate scenario/<name>/main.yml")
	fmt.Fprintln(w, "  validate-manifest <path> [--json]              validate a plugin schema document")
	fmt.Fprintln(w, "  plugin-init       <namespace>/<name> [flags]      scaffold a new SoulModule plugin")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  --modules <alias>=<path>  bind a plugin's schema document to the alias a task")
	fmt.Fprintln(w, "                 addresses it by (redis=./dist/schema.json). Repeatable. <path> is a")
	fmt.Fprintln(w, "                 schema.json, a stamped artifact, or the dist/ dir holding one. The")
	fmt.Fprintln(w, "                 alias is stated here because the artifact carries no name of its own.")
	fmt.Fprintln(w, "                 Without a binding, that module's params cannot be checked and it is")
	fmt.Fprintln(w, "                 reported as plugin_params_unchecked rather than passing silently.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  --service-name <name>  the name the service is REGISTERED under. Read by")
	fmt.Fprintln(w, "                 validate-scenario, for the own-namespace Vault fence (ADR-0083 §7). It is")
	fmt.Fprintln(w, "                 a flag because no file in a service repository states the name: it is")
	fmt.Fprintln(w, "                 assigned once, at registration. Without it the fence cannot run and is")
	fmt.Fprintln(w, "                 reported as own_namespace_fence_unchecked rather than skipped silently.")
}
