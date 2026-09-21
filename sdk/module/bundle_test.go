package module

import (
	"bytes"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"google.golang.org/grpc"
)

// tracer is a module implementation that records whether it was ever asked to serve.
// The point of the dispatch tests is not that the right module runs, but that the
// wrong one does not.
type tracer struct {
	BaseModule
	name   string
	served *[]string
}

func (t *tracer) Apply(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	*t.served = append(*t.served, t.name)
	return nil
}

func testBundle(served *[]string) Bundle {
	return Bundle{
		Compat: Compat{Keeper: ">=0.9 <2.0"},
		Modules: []Def{
			{
				Name:         "acl",
				Description:  "Redis ACL users",
				Capabilities: []Capability{NetworkOutbound},
				SideEffects:  []SideEffect{{User: "redis_acl_user"}},
				Impl:         &tracer{name: "acl", served: served},
				States: map[string]State{
					"present": {
						Description: "The ACL user exists",
						Input: Input{
							"host": {Type: String, Required: true, Description: "Redis host to connect to"},
							"port": {Type: Int, Default: 6379},
							"login_password": {Type: String, Secret: true, Pattern: `^vault:.*`,
								Description: "Password for login_username; MUST be a vault-ref"},
							"tls_enable": {Type: Bool, Default: false},
						},
					},
					"absent": {Description: "The ACL user is gone", Input: Input{
						"host": {Type: String, Required: true},
					}},
				},
			},
			{
				Name:        "config",
				Description: "Redis configuration",
				Impl:        &tracer{name: "config", served: served},
				States: map[string]State{
					"applied": {Description: "The configuration is applied"},
				},
			},
		},
	}
}

func TestServeBundle_SchemaSubcommandPrintsCanonicalDocument(t *testing.T) {
	var served []string
	var stdout, stderr bytes.Buffer

	if code := ServeBundleArgs(testBundle(&served), []string{"schema"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	if len(served) != 0 {
		t.Fatalf("the schema subcommand must not serve anything, served %v", served)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	got := stdout.Bytes()
	ok, err := schema.IsCanonical(got)
	if err != nil {
		t.Fatalf("the printed document does not parse: %v", err)
	}
	if !ok {
		t.Fatalf("the printed document is not canonical: %s", got)
	}
	doc, err := schema.Unmarshal(got)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if doc.Kind != schema.KindSoulModule || doc.ProtocolVersion != protocolVersion {
		t.Fatalf("kind/protocol_version: got %q/%d", doc.Kind, doc.ProtocolVersion)
	}
	if doc.Compat.Keeper != ">=0.9 <2.0" {
		t.Fatalf("compat: got %q", doc.Compat.Keeper)
	}
	if names := doc.ModuleNames(); len(names) != 2 || names[0] != "acl" || names[1] != "config" {
		t.Fatalf("modules: got %v", names)
	}
	if issues := schema.Validate(doc); schema.HasErrors(issues) {
		t.Fatalf("the printed document does not validate: %v", issues)
	}

	// The artifact has no self-name: nothing in the bytes says "redis".
	for _, forbidden := range []string{"namespace", `"name":"soul-mod`, "publisher"} {
		if strings.Contains(string(got), forbidden) {
			t.Fatalf("the document carries a self-name (%q): %s", forbidden, got)
		}
	}
}

func TestServeBundle_SchemaOutputIsByteStable(t *testing.T) {
	var served []string
	b := testBundle(&served)
	var first bytes.Buffer
	if code := ServeBundleArgs(b, []string{"schema"}, &first, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	for i := range 100 {
		var out bytes.Buffer
		if code := ServeBundleArgs(b, []string{"schema"}, &out, &bytes.Buffer{}); code != 0 {
			t.Fatalf("run %d: exit code %d", i, code)
		}
		if !bytes.Equal(first.Bytes(), out.Bytes()) {
			t.Fatalf("run %d differs:\nfirst: %s\ngot:   %s", i, first.Bytes(), out.Bytes())
		}
	}
}

func TestServeBundle_UnknownModuleDoesNotServe(t *testing.T) {
	// The dangerous failure is not "it errors" but "it serves something": which
	// module runs decides which host gets changed.
	cases := map[string][]string{
		"unknown_name":    {"acl-v2"},
		"no_arguments":    {},
		"empty_name":      {""},
		"case_mismatch":   {"ACL"},
		"schema_typo":     {"schemas"},
		"flag_not_a_name": {"--help"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var served []string
			var stdout, stderr bytes.Buffer
			code := ServeBundleArgs(testBundle(&served), args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected a non-zero exit, stdout: %s", stdout.String())
			}
			if len(served) != 0 {
				t.Fatalf("served %v for args %v", served, args)
			}
			if stderr.Len() == 0 {
				t.Fatal("expected a message on stderr")
			}
			if stdout.Len() != 0 {
				t.Fatalf("nothing should reach stdout on failure, got: %s", stdout.String())
			}
		})
	}
}

func TestServeBundle_RejectsInvalidBundleBeforeDispatch(t *testing.T) {
	var served []string
	bad := Bundle{Modules: []Def{{
		Name:   "acl",
		Impl:   &tracer{name: "acl", served: &served},
		States: map[string]State{"present": {Description: "d", Input: Input{"host": {Type: "bogus"}}}},
	}}}
	var stdout, stderr bytes.Buffer
	if code := ServeBundleArgs(bad, []string{"acl"}, &stdout, &stderr); code == 0 {
		t.Fatal("expected an invalid bundle to be refused")
	}
	if len(served) != 0 {
		t.Fatalf("an invalid bundle must not serve, served %v", served)
	}
	if !strings.Contains(stderr.String(), "input_type_unknown") {
		t.Fatalf("stderr should name the problem, got: %s", stderr.String())
	}
}

func TestBundle_ValidateCatchesMissingImpl(t *testing.T) {
	b := Bundle{Modules: []Def{{
		Name:   "acl",
		States: map[string]State{"present": {Description: "d"}},
	}}}
	issues := b.Validate()
	if !schema.HasErrors(issues) {
		t.Fatal("a module with no Impl must be refused")
	}
	var found bool
	for _, i := range issues {
		if i.Code == "module_impl_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected module_impl_missing, got %v", issues)
	}
}

func TestBundle_DocumentDropsImplAndKeepsEverythingElse(t *testing.T) {
	var served []string
	doc := testBundle(&served).Document()
	acl, ok := doc.Module("acl")
	if !ok {
		t.Fatal("module acl is missing from the document")
	}
	if acl.Description != "Redis ACL users" {
		t.Fatalf("description: got %q", acl.Description)
	}
	if len(acl.Capabilities) != 1 || acl.Capabilities[0] != NetworkOutbound {
		t.Fatalf("capabilities: got %v", acl.Capabilities)
	}
	rt, v, okRes := acl.SideEffects[0].Resource()
	if !okRes || rt != "user" || v != "redis_acl_user" {
		t.Fatalf("side effect: got %q=%q ok=%v", rt, v, okRes)
	}
	p := acl.States["present"].Input["login_password"]
	if !p.Secret || p.Pattern != `^vault:.*` {
		t.Fatalf("secret parameter lost its shape: %+v", p)
	}
	if got := acl.States["present"].Input["port"].Default; got != 6379 {
		t.Fatalf("default: got %v (%T)", got, got)
	}

	// Capabilities and side effects stay PER MODULE: `config` discloses nothing
	// just because `acl` does.
	cfg, _ := doc.Module("config")
	if len(cfg.Capabilities) != 0 || len(cfg.SideEffects) != 0 {
		t.Fatalf("a sibling module inherited disclosure: %+v", cfg)
	}
}
