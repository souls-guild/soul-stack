package module

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"google.golang.org/grpc"
)

// ServeBundle is the main() of a module bundle:
//
//	func main() {
//		module.ServeBundle(module.Bundle{
//			Compat:  module.Compat{Keeper: ">=0.9 <2.0"},
//			Modules: []module.Def{acl.Module, config.Module, info.Module},
//		})
//	}
//
// It dispatches on argv[1]:
//
//   - a module name — serve that module over the gRPC SoulModule service, exactly as
//     the single-module [Serve] does. The host forks per Apply (ADR-020(d)), so one
//     process serves one module and the proto contract does not change:
//     `protocol_version` stays where it is.
//   - `schema` — print the canonical schema document to stdout and exit 0. This is how
//     `soul-mod stamp` derives what it stamps, and it is why `schema` cannot be a
//     module name.
//   - anything else, or nothing at all — a message on stderr and a non-zero exit. An
//     artifact asked for a module it does not have must fail, never fall through to
//     "the first one" or "the only one": which module runs decides which host gets
//     changed.
//
// ServeBundle does not return — it exits the process. Use [ServeBundleArgs] when you
// need the exit code as a value.
func ServeBundle(b Bundle) {
	os.Exit(ServeBundleArgs(b, os.Args[1:], os.Stdout, os.Stderr))
}

// ServeBundleArgs is [ServeBundle] with the arguments and streams injected, returning
// the process exit code instead of taking it. Exported for tests and for authors who
// wrap the SDK in their own main().
func ServeBundleArgs(b Bundle, args []string, stdout, stderr io.Writer) int {
	if issues := b.Validate(); schema.HasErrors(issues) {
		fmt.Fprintln(stderr, "invalid module bundle:")
		for _, i := range issues {
			if i.Level != schema.LevelError {
				continue
			}
			fmt.Fprintf(stderr, "  %s %s: %s\n", i.Path, i.Code, i.Message)
		}
		return 2
	}

	if len(args) == 0 {
		fmt.Fprintf(stderr, "missing module name: expected one of %s, or %q\n",
			strings.Join(quoted(moduleNames(b)), ", "), schema.SchemaSubcommand)
		return 2
	}

	switch name := args[0]; name {
	case schema.SchemaSubcommand:
		doc, err := b.Schema()
		if err != nil {
			fmt.Fprintf(stderr, "render schema: %v\n", err)
			return 2
		}
		if _, err := stdout.Write(doc); err != nil {
			fmt.Fprintf(stderr, "write schema: %v\n", err)
			return 2
		}
		return 0
	default:
		def, ok := lookupModule(b, name)
		if !ok {
			fmt.Fprintf(stderr, "unknown module %q: this artifact serves %s\n",
				name, strings.Join(quoted(moduleNames(b)), ", "))
			return 2
		}
		if err := serveModule(def.Impl); err != nil {
			fmt.Fprintf(stderr, "serve module %q: %v\n", name, err)
			return 1
		}
		return 0
	}
}

// Serve is the single-module form kept for plugins that ship one module and for
// in-tree test artifacts. It serves immediately, with no subcommand and no schema: an
// artifact served this way cannot be described to Keeper, so anything an operator has
// to approve uses [ServeBundle] instead.
func Serve(impl SoulModule) error { return serveModule(impl) }

func serveModule(impl SoulModule) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSoulModuleServer(s, &serverAdapter{impl: impl})
	})
}

func lookupModule(b Bundle, name string) (Def, bool) {
	for _, d := range b.Modules {
		if d.Name == name {
			return d, true
		}
	}
	return Def{}, false
}

func moduleNames(b Bundle) []string {
	out := make([]string, 0, len(b.Modules))
	for _, d := range b.Modules {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

func quoted(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strconv.Quote(n))
	}
	return out
}
