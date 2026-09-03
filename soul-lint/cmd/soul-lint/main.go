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
//	validate-service-tree <dir> [--json]  validate a WHOLE service — the
//	                                    manifest, types.yml, the covenant each
//	                                    scenario extends, and every scenario —
//	                                    reporting all of them rather than
//	                                    stopping at the first broken part.
//	validate-manifest <path> [--json]  validate a plugin's schema document
//	                                    (dist/schema.json, or a stamped
//	                                    artifact).
//	schema-stamp      <dir>            write migrations/schema.lock: the top of
//	                                    the ladder and a fingerprint of the
//	                                    parsed state_schema.
//	plugin-init       <namespace>/<name> [flags]  scaffold a new SoulModule
//	                                    plugin (ADR-016 amendment 2026-05-27).
//	list-secret-paths <path> --service-name NAME  print the Vault address of
//	                                    every secret declared in a service.yml.
//
// `validate-service-tree` is the whole-service mode (NIM-753). Its positional is
// the service DIRECTORY (or the service.yml inside it); it takes the same flags as
// the per-file family and applies them to every part it walks. Its one rule is
// that a broken part does not suppress the diagnostics of the rest — the rule a
// service repository's own `set -e` script cannot hold, and where eight
// divergences accumulated behind one red manifest line before this existed. The
// per-file commands stay, for an editor and for re-checking one file — with one
// behaviour change among them: `validate-scenario` shares the include resolver, so
// on an `upgrade/<slug>/main.yml` it now resolves from `scenario/<slug>/` and
// `scenario/` as the engine does, instead of treating that path as a loose file.
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
// `list-secret-paths` takes the same flag and for the same reason, but there it is
// REQUIRED rather than advisory: the name is a segment of every path it prints, so
// without it there is nothing to print.
//
// Exit codes: 0 = ok, 1 = has errors, 2 = I/O fatal / usage.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/souls-guild/soul-stack/soul-lint/internal/plugininit"
	"github.com/souls-guild/soul-stack/soul-lint/internal/secretpaths"
	"github.com/souls-guild/soul-stack/soul-lint/internal/validate"
)

// command is one entry of the subcommand table. The table is the single place a
// subcommand is declared: main dispatches from it and printUsage prints from it,
// so a command cannot exist in one and be missing from the other — which is what
// a switch beside a hand-written usage block eventually produces. Adding one is one
// row and its run function.
type command struct {
	name string
	// args is the argument shape after the name, for the usage line.
	args string
	// summary is the one line printed by printUsage.
	summary string
	// run receives its own table entry, so it can build its usage line from the
	// name and shape rather than repeating them as a literal.
	run func(c command, args []string) int
}

func (c command) usageLine() string { return "Usage: soul-lint " + c.name + " " + c.args }

// validateCommand builds a table entry for one of the validate-* family — the
// commands that share [runSubcommand]'s flag shape and differ only in Kind.
func validateCommand(name, args, summary string, kind validate.Kind) command {
	return command{name: name, args: args, summary: summary, run: func(c command, a []string) int {
		return runSubcommand(c.name, c.name+" "+c.args, kind, a)
	}}
}

var commandTable = []command{
	validateCommand("validate-config", "<path> [--json]",
		"validate keeper.yml or soul.yml", validate.KindConfig),
	validateCommand("validate-destiny", "<path> [--json] [--modules ALIAS=PATH]...",
		"validate destiny.yml manifest", validate.KindDestiny),
	validateCommand("validate-service", "<path> [--json] [--modules ALIAS=PATH]...",
		"validate service.yml manifest", validate.KindService),
	validateCommand("validate-scenario", "<path> [--json] [--service-name NAME] [--modules ALIAS=PATH]...",
		"validate scenario/<name>/main.yml", validate.KindScenario),
	{
		name:    "validate-service-tree",
		args:    "<dir> [--json] [--service-name NAME] [--modules ALIAS=PATH]...",
		summary: "validate a WHOLE service: manifest, types.yml, every scenario",
		run: func(c command, a []string) int {
			return runValidateServiceTree(c, a)
		},
	},
	validateCommand("validate-manifest", "<path> [--json]",
		"validate a plugin schema document", validate.KindManifest),
	{
		name:    "schema-stamp",
		args:    "<dir>",
		summary: "write migrations/schema.lock from the current state_schema and ladder",
		run: func(c command, a []string) int {
			return runSchemaStamp(c, a)
		},
	},
	{
		name:    "plugin-init",
		args:    "<namespace>/<name> [flags]",
		summary: "scaffold a new SoulModule plugin",
		run:     func(_ command, a []string) int { return runPluginInit(a) },
	},
	{
		name:    "list-secret-paths",
		args:    "<path> --service-name NAME",
		summary: "print the derived Vault address of every declared secret",
		run:     func(_ command, a []string) int { return runListSecretPaths(a) },
	},
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	switch sub {
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		os.Exit(0)
	}
	for _, c := range commandTable {
		if c.name == sub {
			os.Exit(c.run(c, os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, "soul-lint: unknown subcommand %q\n\n", sub)
	printUsage(os.Stderr)
	os.Exit(2)
}

// commonFlags is what every validate-* command parses: one positional and the
// three flags. Held apart from the commands themselves so that the tree command,
// whose positional is a DIRECTORY rather than a file, still parses `--json`,
// `--modules` and `--service-name` by the same rules — a second parser is how a
// flag starts meaning one thing in one command and another elsewhere.
type commonFlags struct {
	jsonOut     bool
	path        string
	modules     []string
	serviceName string
}

// parseCommonFlags returns the parsed flags; `done` true means the caller must
// return `code` immediately (a help request, or a usage error already reported).
func parseCommonFlags(sub, usageLine string, args []string) (f commonFlags, code int, done bool) {
	var (
		wantModule  bool // the previous arg was `--modules`, so this one is its value
		wantService bool // likewise for `--service-name`
	)
	for _, a := range args {
		if wantModule {
			f.modules = append(f.modules, a)
			wantModule = false
			continue
		}
		if wantService {
			f.serviceName = a
			wantService = false
			continue
		}
		switch {
		case a == "--json" || a == "-json":
			f.jsonOut = true
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stdout, usageLine)
			return f, 0, true
		case a == "--service-name" || a == "-service-name":
			wantService = true
		case strings.HasPrefix(a, "--service-name=") || strings.HasPrefix(a, "-service-name="):
			// Not repeatable: a service has one name. A second occurrence overwrites,
			// rather than being refused, for the same reason every other flag here does.
			_, value, _ := strings.Cut(a, "=")
			f.serviceName = value
		case a == "--modules" || a == "-modules":
			wantModule = true
		case strings.HasPrefix(a, "--modules=") || strings.HasPrefix(a, "-modules="):
			// Repeatable: one binding per occurrence, appended in order. An empty
			// value is kept rather than dropped so LoadModuleSchemas rejects it by
			// the same rule as every other malformed binding — silently ignoring
			// `--modules=` would mean "check nothing", which is the failure mode
			// this flag exists to remove.
			_, value, _ := strings.Cut(a, "=")
			f.modules = append(f.modules, value)
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "soul-lint %s: unknown flag %q\n", sub, a)
			return f, 2, true
		default:
			if f.path != "" {
				fmt.Fprintln(os.Stderr, usageLine)
				return f, 2, true
			}
			f.path = a
		}
	}
	if wantModule {
		fmt.Fprintf(os.Stderr, "soul-lint %s: --modules needs an <alias>=<path> binding\n", sub)
		return f, 2, true
	}
	if wantService {
		fmt.Fprintf(os.Stderr, "soul-lint %s: --service-name needs a <name>\n", sub)
		return f, 2, true
	}
	if f.path == "" {
		fmt.Fprintln(os.Stderr, usageLine)
		return f, 2, true
	}
	return f, 0, false
}

// runSubcommand parses flags and the positional <path>. Same shape across
// all validate-* subcommands (spec M1.2.a, symmetric with M0).
func runSubcommand(sub, usage string, kind validate.Kind, args []string) int {
	f, code, done := parseCommonFlags(sub, "Usage: soul-lint "+usage, args)
	if done {
		return code
	}
	return validate.Run(validate.Options{
		Path:        f.path,
		JSON:        f.jsonOut,
		Kind:        kind,
		Modules:     f.modules,
		ServiceName: f.serviceName,
	}, os.Stdout, os.Stderr)
}

// runValidateServiceTree is the whole-service mode (NIM-753): one invocation for
// the manifest, the type catalog and every scenario, with every part reported
// rather than the run ending at the first broken one.
//
// It takes the same three flags as the per-file family and for the same reasons,
// which is why it shares their parser. Its positional differs — a service
// DIRECTORY (or the service.yml inside it, both accepted) — and that difference
// is resolved by validate.RunTree, not here.
func runValidateServiceTree(c command, args []string) int {
	f, code, done := parseCommonFlags(c.name, c.usageLine(), args)
	if done {
		return code
	}
	return validate.RunTree(validate.TreeOptions{
		Root:        f.path,
		JSON:        f.jsonOut,
		Modules:     f.modules,
		ServiceName: f.serviceName,
	}, os.Stdout, os.Stderr)
}

// runSchemaStamp parses the positional of `schema-stamp <dir>` — the generator
// half of the schema lock (NIM-737).
//
// It does NOT go through parseCommonFlags, and that is the same call
// list-secret-paths makes for the same reason: this command prints a generated
// artifact, not diagnostics. `--json` would have nothing to encode, `--modules`
// binds checks it does not run, and `--service-name` names a fence it never
// reaches. A flag accepted and ignored is worse than one that is absent.
func runSchemaStamp(c command, args []string) int {
	var root string
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stdout, c.usageLine())
			return validate.ExitOK
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "soul-lint %s: unknown flag %q\n", c.name, a)
			return validate.ExitIOFatal
		default:
			if root != "" {
				fmt.Fprintln(os.Stderr, c.usageLine())
				return validate.ExitIOFatal
			}
			root = a
		}
	}
	if root == "" {
		fmt.Fprintln(os.Stderr, c.usageLine())
		return validate.ExitIOFatal
	}
	return validate.RunStamp(validate.StampOptions{Root: root}, os.Stdout, os.Stderr)
}

// runListSecretPaths parses flags for
// `list-secret-paths <service.yml> --service-name NAME`.
//
// It does not go through runSubcommand: that one is the shape shared by the validate-*
// family (a validate.Kind, --json, --modules), and this command is none of those — it
// prints a derivation, not diagnostics. The `--service-name` spellings it accepts are
// kept identical to that family's, so the flag behaves the same wherever it is typed.
func runListSecretPaths(args []string) int {
	return listSecretPaths(args, os.Stdout, os.Stderr)
}

// listSecretPaths is runListSecretPaths with the streams injected, so a test can assert
// what was PRINTED and not merely the exit code. A CLI test that checks the code alone
// passes with the wrong service name substituted under it.
func listSecretPaths(args []string, out, errOut io.Writer) int {
	const usageLine = "Usage: soul-lint list-secret-paths <service.yml> --service-name NAME"
	var (
		path        string
		serviceName string
		wantService bool // the previous arg was `--service-name`, so this one is its value
	)
	for _, a := range args {
		if wantService {
			// A flag-shaped token here is a missing value, not a service name. The
			// validate-* family swallows it, where the cost is a fence that quietly
			// does not run; here the cost is a printed path with `--json` in the
			// service segment and exit 0 — a confident wrong answer. A name that
			// genuinely starts with `-` is still reachable as `--service-name=-x`.
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(errOut, "soul-lint list-secret-paths: --service-name needs a <name>, got the flag %q — use --service-name=%s if that really is the name\n", a, a)
				return secretpaths.ExitIOFatal
			}
			serviceName = a
			wantService = false
			continue
		}
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(out, usageLine)
			return secretpaths.ExitOK
		case a == "--service-name" || a == "-service-name":
			wantService = true
		case strings.HasPrefix(a, "--service-name=") || strings.HasPrefix(a, "-service-name="):
			_, value, _ := strings.Cut(a, "=")
			serviceName = value
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errOut, "soul-lint list-secret-paths: unknown flag %q\n", a)
			return secretpaths.ExitIOFatal
		default:
			if path != "" {
				fmt.Fprintln(errOut, usageLine)
				return secretpaths.ExitIOFatal
			}
			path = a
		}
	}
	if wantService {
		fmt.Fprintln(errOut, "soul-lint list-secret-paths: --service-name needs a <name>")
		return secretpaths.ExitIOFatal
	}
	if path == "" {
		fmt.Fprintln(errOut, usageLine)
		return secretpaths.ExitIOFatal
	}
	return secretpaths.Run(secretpaths.Options{Path: path, ServiceName: serviceName}, out, errOut)
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

// printUsage prints the command list from [commandTable] and the flag notes
// below it. The list is generated rather than written out: a hand-kept copy is
// how `validate-manifest` could have shipped undiscoverable, and how the next
// command would.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: soul-lint <command> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	width := 0
	for _, c := range commandTable {
		if n := len(c.name); n > width {
			width = n
		}
	}
	for _, c := range commandTable {
		// Name column padded to the widest name, then the argument shape and the
		// summary on the continuation line: the shapes differ too much in length
		// for a second aligned column to stay readable as commands are added.
		fmt.Fprintf(w, "  %-*s %s\n", width, c.name, c.args)
		fmt.Fprintf(w, "  %-*s   %s\n", width, "", c.summary)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  validate-service-tree is the whole-service mode: it checks the manifest, types.yml,")
	fmt.Fprintln(w, "                 the covenant each scenario extends, and EVERY scenario, and reports all")
	fmt.Fprintln(w, "                 of them - a broken part does not suppress the diagnostics of the rest.")
	fmt.Fprintln(w, "                 The per-file commands above stay, for checking one file at a time.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  schema-stamp writes migrations/schema.lock - the top of the ladder plus a hash of the")
	fmt.Fprintln(w, "                 PARSED state_schema, so a comment or a reordered key does not move it.")
	fmt.Fprintln(w, "                 validate-service compares both on every run: a schema edited with no")
	fmt.Fprintln(w, "                 ladder step behind it is caught by nothing else, offline or online. It")
	fmt.Fprintln(w, "                 refuses to stamp a service that is red - a stamp claims the schema and")
	fmt.Fprintln(w, "                 the ladder agreed, which a broken tree has not established.")
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
	fmt.Fprintln(w, "                 list-secret-paths takes the same flag and REQUIRES it: the name is a")
	fmt.Fprintln(w, "                 segment of every path it prints.")
}
