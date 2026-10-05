package scenario

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

const listedToken = "bootstrap-token-listed-7c1d"

type listedHostResolver struct{ d *render.ResolvedDestiny }

func (r listedHostResolver) Resolve(_ context.Context, name string) (*render.ResolvedDestiny, error) {
	if name != r.d.Name {
		return nil, fmt.Errorf("scenario: destiny %q is not declared in service.yml::destiny[]", name)
	}
	return r.d, nil
}

func installDestiny(extra ...config.Task) *render.ResolvedDestiny {
	tasks := []config.Task{
		{
			Name: "Write the note",
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"path": "/etc/soul/note", "content": "${ input.note }", "owner": "${ incarnation.name }"},
			},
		},
		{
			Name: "Redeem the bootstrap token",
			Module: &config.ModuleTask{
				Module: "core.exec.run",
				Params: map[string]any{
					"cmd":  "/usr/local/bin/soul",
					"args": []any{"init"},
					"env":  map[string]any{"SOUL_BOOTSTRAP_TOKEN": "${ input.bootstrap_token }"},
				},
			},
		},
	}
	return &render.ResolvedDestiny{
		Name: "soul",
		Input: config.InputSchemaMap{
			"note":            {Type: "string", Required: true},
			"bootstrap_token": {Type: "string", Secret: true, Default: ""},
		},
		Tasks: append(tasks, extra...),
	}
}

func listedPipeline(t *testing.T) *render.Pipeline {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel engine: %v", err)
	}
	return render.NewPipeline(nil, engine, nil, nil)
}

var listedMeta = render.IncarnationMeta{ID: "edge-1", Service: "edge"}

// ★ GUARD (decision 6). A value reaches the destiny BY REFERENCE: text that looks
// like an expression — here a vault() call — arrives as that text and is never
// evaluated on the Keeper. Register values come from plugins and cloud APIs.
//
// Mutation: put the values themselves into the synthetic `apply: input:` (the
// bare push API's construction) and this reddens — the vault() call is evaluated.
func TestListedHostRender_ValuesArriveVerbatim(t *testing.T) {
	note := `${ vault("secret/edge/admin#password") }`
	rd, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{installDestiny()}, listedMeta, nil, nil,
		"soul", "vm-1.example", map[string]any{"note": note, "bootstrap_token": listedToken})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := rd.Tasks[0].GetParams().AsMap()["content"]; got != note {
		t.Errorf("content = %v, want the literal text %q", got, note)
	}
}

// The destiny renders with the run's incarnation, carries its tasks in order for
// the listed host, and reports the secret its own schema declares so a message
// can be masked.
func TestListedHostRender_TasksIncarnationAndSecrets(t *testing.T) {
	rd, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{installDestiny()}, listedMeta, nil, nil,
		"soul", "vm-1.example", map[string]any{"note": "hello", "bootstrap_token": listedToken})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(rd.Tasks) != 2 || rd.Tasks[1].GetName() != "Redeem the bootstrap token" || rd.Tasks[1].GetModule() != "core.exec.run" {
		t.Fatalf("tasks = %v", rd.Tasks)
	}
	if owner := rd.Tasks[0].GetParams().AsMap()["owner"]; owner != listedMeta.ID {
		t.Errorf("incarnation.name rendered %v, want the run's %q", owner, listedMeta.ID)
	}
	found := false
	for _, s := range rd.Secrets {
		if s == listedToken {
			found = true
		}
	}
	if !found {
		t.Errorf("secrets = %v, want the destiny's secret input sealed", rd.Secrets)
	}
}

// A listed host has no Soulprint: a destiny reading a fact fails the render,
// naming it, rather than rendering an empty value.
func TestListedHostRender_NoSoulprintFacts(t *testing.T) {
	readsFact := config.Task{
		Name: "Pick the package manager",
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{"path": "/tmp/pm", "content": "${ soulprint.self.os.pkg_mgr }"},
		},
	}
	_, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{installDestiny(readsFact)}, listedMeta, nil, nil,
		"soul", "vm-1.example", map[string]any{"note": "hello"})
	if err == nil {
		t.Fatal("a destiny reading soulprint.self.os rendered for a host with no Soulprint")
	}
	if !strings.Contains(err.Error(), "soulprint.self.os.pkg_mgr") || !strings.Contains(err.Error(), "no such key: os") {
		t.Errorf("error = %v, want the missing fact named", err)
	}
}

// The destiny is resolved by the run's resolver, so an undeclared one gets the
// refusal `apply:` gets.
func TestListedHostRender_UndeclaredDestinyIsRefused(t *testing.T) {
	_, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{installDestiny()}, listedMeta, nil, nil,
		"redis", "vm-1.example", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("err = %v, want the resolver's refusal", err)
	}
}

func TestListedHostRender_RefusesANonIdentifierInputName(t *testing.T) {
	_, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{installDestiny()}, listedMeta, nil, nil,
		"soul", "vm-1.example", map[string]any{"note": "x", "bad-name": "y"})
	if err == nil || !strings.Contains(err.Error(), "bad-name") {
		t.Fatalf("err = %v", err)
	}
}

// A run with no destiny source still hands the module a render, and the render
// refuses the destiny the way it refuses an `apply:` without one.
func TestListedHostDestinyRenderer_NoSourceRefusesLikeApply(t *testing.T) {
	r := &Runner{deps: Deps{Render: listedPipeline(t)}}
	f := r.listedHostDestinyRenderer(nil, listedMeta, nil, nil)
	if f == nil {
		t.Fatal("no renderer: the module would refuse with a message about scenario runs, not about the destiny source")
	}
	_, err := f(context.Background(), "soul", "vm-1.example", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "DestinyResolver not configured") {
		t.Fatalf("err = %v, want apply:'s refusal", err)
	}
}

// A secret input interpolated into a longer string is masked as its own value
// too: an agent's message can echo the bare token without the rest of the cell.
// The destiny here uses the token ONLY inside `--token=…`, so the sealed cell's
// whole value is not the token.
//
// Mutation: drop the captured-schema loop in renderListedHostDestiny and this
// reddens.
func TestListedHostRender_SecretInputValueIsMaskedOnItsOwn(t *testing.T) {
	inArgs := config.Task{
		Name: "Redeem through a flag",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"cmd": "/usr/local/bin/soul", "args": []any{"init", "--token=${ input.bootstrap_token }"}},
		},
	}
	onlyInArgs := &render.ResolvedDestiny{
		Name:  "soul",
		Input: installDestiny().Input,
		Tasks: []config.Task{inArgs},
	}
	rd, err := renderListedHostDestiny(context.Background(), listedPipeline(t), listedHostResolver{onlyInArgs}, listedMeta, nil, nil,
		"soul", "vm-1.example", map[string]any{"note": "hello", "bootstrap_token": listedToken})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, s := range rd.Secrets {
		if s == listedToken {
			return
		}
	}
	t.Errorf("secrets = %v, want the bare token value among them", rd.Secrets)
}
