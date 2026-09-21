package module

import (
	"strconv"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// Authoring types.
//
// A module declares what it offers next to the code that offers it — one Go value, no
// hand-written YAML anywhere. From that value the SDK generates the canonical schema
// document (`sdk/schema`), which `soul-mod stamp` puts inside the artifact and beside
// it in `dist/schema.json`.
//
// Everything except [Def] and [Bundle] is an alias of the corresponding `sdk/schema`
// type: there is exactly one definition of a parameter or a state in the tree, and
// `module.Param` is the ergonomic spelling of it for an author. [Def] is the one that
// cannot be an alias — it carries the running implementation ([Def.Impl]) alongside the
// description, and an implementation does not serialize.

// Aliases of the schema types, so an author writes `module.String` and
// `module.Input{...}` without importing two packages.
type (
	State       = schema.State
	Input       = schema.Input
	Output      = schema.Output
	Param       = schema.Param
	ParamType   = schema.ParamType
	Capability  = schema.Capability
	SideEffect  = schema.SideEffect
	Compat      = schema.Compat
	Deprecated  = schema.Deprecated
	InputSource = schema.InputSource
	Document    = schema.Document
	Side        = schema.Side
)

// The declarable sides (NIM-747). A module says which half of the platform runs
// it, and every task addressing it inherits that — `on:` in a scenario is back to
// meaning "which covens" and nothing else.
//
// [SideSoul] is the zero value and the default: a module that declares nothing
// runs on the host, which is where every module written before this field runs.
const (
	SideSoul   = schema.SideSoul
	SideKeeper = schema.SideKeeper
)

// Parameter types. String/Int/Bool/List/Map are the canonical spellings; the rest are
// the `docs/input.md` synonyms, kept so nothing expressible before this SDK became
// inexpressible with it.
const (
	String  = schema.String
	Int     = schema.Int
	Bool    = schema.Bool
	List    = schema.List
	Map     = schema.Map
	Integer = schema.Integer
	Number  = schema.Number
	Boolean = schema.Boolean
	Array   = schema.Array
	Object  = schema.Object
)

// The closed capability set. A module declares what it will reach for; the operator
// reads that before approving the artifact.
//
// These are disclosure, not confinement. Nothing sandboxes a plugin at spawn: the host
// compares the declared strings against the allow-list and refuses to start on a
// mismatch, and after that the process is an ordinary process. The control that does
// bite is the approved sha256 — the host will not exec an artifact whose digest differs
// (ADR-020(g) and ADR-026(c), as corrected by NIM-377).
const (
	RunAsRoot       = schema.RunAsRoot
	NetworkOutbound = schema.NetworkOutbound
	NetworkInbound  = schema.NetworkInbound
	VaultAccess     = schema.VaultAccess
	FSWriteRoot     = schema.FSWriteRoot
	ExecSubprocess  = schema.ExecSubprocess
)

// Def is one module of a bundle: what it does, what it touches, the states it serves,
// and the implementation that serves them.
//
// The value lives next to the module's code:
//
//	var Module = module.Def{
//		Name:         "acl",
//		Description:  "Redis ACL users",
//		Capabilities: []module.Capability{module.NetworkOutbound},
//		SideEffects:  []module.SideEffect{{User: "redis_acl_user"}},
//		Impl:         &ACL{},
//
//		States: map[string]module.State{
//			"present": {
//				Description: "The ACL user exists with the given password and rules",
//				Input: module.Input{
//					"host": {Type: module.String, Required: true},
//					"port": {Type: module.Int, Default: 6379},
//				},
//			},
//		},
//	}
//
// Name is address level 2 — the `acl` in `redis.acl.present`. Level 1 is the alias the
// operator picks at registration and is deliberately absent from the artifact: the same
// bytes registered as `redis` and as `redis-community` serve two address spaces.
//
// Capabilities and SideEffects are declared PER MODULE. An operator approving `acl`
// should see what `acl` touches and nothing else; folding them up to the artifact would
// make every module disclose every other module's footprint.
type Def struct {
	Name        string
	Description string

	// IntroducedIn — the engine release in which this module first appeared
	// (ADR-0076(i)), plain MAJOR.MINOR.PATCH. Empty means at or before the
	// baseline.
	IntroducedIn string

	// Side — which half of the platform executes this module ([SideSoul] by
	// default, [SideKeeper] for one the Keeper runs itself against no host).
	// Declared once here rather than restated by every task that addresses it.
	//
	// The Keeper executes a module declaring [SideKeeper] (NIM-758), in its own
	// process rather than on a host — the same SoulModule contract, a different
	// place. Such a scenario still writes `on: keeper` on the task: the
	// declaration lives in the stamped schema document, which a scenario cannot
	// read, so the address alone does not say the side of a plugin.
	Side Side

	Capabilities []Capability
	SideEffects  []SideEffect

	// States maps a state name to its contract. `present` and `absent` are the
	// usual pair; a module may declare as many as it serves.
	States map[string]State

	// Impl is the running module — the value whose Apply the host calls when a task
	// names this module. It is not part of the schema document.
	Impl SoulModule
}

// Bundle is one artifact: several modules of one subject, served by one executable and
// dispatched by subcommand (`redis acl`).
//
// The host forks per Apply (ADR-020(d), one-shot), so serving several modules from one
// long-lived process would buy nothing — the bundle exists to make one artifact, one
// approval and one signature cover a subject, not to share a process.
type Bundle struct {
	// Compat is the engine window the artifact declares (ADR-0076(c)).
	Compat Compat

	// Modules are the modules this artifact serves. Names must be unique and must
	// not be `schema`, which argv[1] reserves for the document dump.
	Modules []Def
}

// Document renders the bundle as the schema document — the value that gets serialized,
// stamped and signed. Pure mapping: [Def.Impl] is dropped, everything else is copied.
func (b Bundle) Document() Document {
	doc := Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: protocolVersion,
		Compat:          b.Compat,
	}
	if len(b.Modules) == 0 {
		return doc
	}
	doc.Modules = make([]schema.Module, 0, len(b.Modules))
	for _, d := range b.Modules {
		doc.Modules = append(doc.Modules, schema.Module{
			Name:         d.Name,
			Description:  d.Description,
			Side:         d.Side,
			IntroducedIn: d.IntroducedIn,
			Capabilities: d.Capabilities,
			SideEffects:  d.SideEffects,
			States:       d.States,
		})
	}
	return doc
}

// Schema returns the canonical bytes of the bundle's document — exactly what the
// `schema` subcommand prints and what `soul-mod stamp` writes into the artifact.
func (b Bundle) Schema() ([]byte, error) {
	return schema.Marshal(b.Document())
}

// Validate reports what is wrong with the bundle, using the same rules the host applies
// at approval time. An author sees the message at build time instead of at deploy time.
func (b Bundle) Validate() []schema.Issue {
	issues := schema.Validate(b.Document())
	for i, d := range b.Modules {
		if d.Impl == nil {
			issues = append(issues, schema.Issue{
				Level: schema.LevelError, Phase: schema.PhaseSemantic,
				Path:    "$.modules[" + d.Name + "].impl",
				Code:    "module_impl_missing",
				Message: "module " + moduleLabel(i, d.Name) + " has no Impl",
				Hint:    "set Impl to the value whose Apply serves this module's states",
			})
		}
	}
	return issues
}

func moduleLabel(i int, name string) string {
	if name == "" {
		return "#" + strconv.Itoa(i)
	}
	return strconv.Quote(name)
}
