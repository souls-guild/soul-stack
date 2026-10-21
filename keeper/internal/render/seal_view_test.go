package render

import (
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The seal over a task's whole view (ADR-0082 amendment 2026-10-15, point 15): a name
// is tainted when ANY of its definitions in the view — service `vars/`, the scenario's
// `vars:`, a destiny's `vars.yml`, block + task — reads a secret source or a tainted
// name, whatever rung the definition and the secret came from.

// sealView renders one task over the hop scenario's secret input and returns its
// sealed paths.
func sealView(t *testing.T, in RenderInput) map[string]bool {
	t.Helper()
	paths, _ := renderHop(t, NewPipeline(nil, newEngine(t), nil, nil), in, 0)
	return paths
}

func contentTask(vars map[string]any, params map[string]any) config.Task {
	return config.Task{Name: "write", Vars: vars, Module: &config.ModuleTask{Module: "core.file.present", Params: params}}
}

// TestRender_SealTaintFollowsAChainThroughTheView — a definition that names no secret
// source itself is tainted by reading a name that does, in any rung of the view and in
// either direction across it. The service rung's own text cannot name a secret, so a
// service definition is tainted only through a name an upper rung defines.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: walk only the task rung in
// sealViewVars (`if l.Rung != cel.RungTask { continue }`). Every case goes red.
func TestRender_SealTaintFollowsAChainThroughTheView(t *testing.T) {
	cases := map[string]func(*RenderInput){
		"a service definition over a task override": func(in *RenderInput) {
			in.ServiceVars = serviceView(t, map[string]any{"dsn": "redis://:${ vars.password }@${ vars.host }", "host": "localhost"})
			in.Scenario.Tasks[0].Vars = map[string]any{"password": "${ input.admin_password }"}
		},
		"a service definition over a scenario definition": func(in *RenderInput) {
			in.ServiceVars = serviceView(t, map[string]any{"dsn": "redis://:${ vars.password }@${ vars.host }", "host": "localhost"})
			in.Scenario.Vars = map[string]any{"password": "${ input.admin_password }"}
		},
		"a scenario definition over a secret input": func(in *RenderInput) {
			in.Scenario.Vars = map[string]any{"dsn": "redis://:${ input.admin_password }@${ vars.host }", "host": "localhost"}
		},
		"a scenario definition over a secret state field": func(in *RenderInput) {
			in.State = map[string]any{"db_password": hopSecret}
			in.SecretStateFields = map[string]bool{"db_password": true}
			in.Scenario.Vars = map[string]any{"dsn": "redis://:${ incarnation.state.db_password }@${ vars.host }", "host": "localhost"}
		},
		"a task definition over a scenario definition, two hops": func(in *RenderInput) {
			in.Scenario.Vars = map[string]any{"pw": "${ input.admin_password }", "host": "localhost"}
			in.Scenario.Tasks[0].Vars = map[string]any{"line": ":${ vars.pw }", "dsn": "redis://${ vars.line }@${ vars.host }"}
		},
		"a self-reference over a tainted lower definition": func(in *RenderInput) {
			in.Scenario.Vars = map[string]any{"dsn": "${ input.admin_password }", "host": "localhost"}
			in.Scenario.Tasks[0].Vars = map[string]any{"dsn": "redis://:${ vars.dsn }@${ vars.host }"}
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			in := hopRenderInput(hopScenario(contentTask(nil, map[string]any{
				"content": "${ vars.dsn }",
				"bind":    "${ vars.host }",
			})))
			set(&in)
			paths := sealView(t, in)
			if !paths["content"] {
				t.Errorf("the cell reading vars.dsn is NOT sealed: %v", paths)
			}
			if paths["bind"] {
				t.Errorf("the cell reading the public vars.host was sealed: %v", paths)
			}
		})
	}

	t.Run("a vars.yml definition over a destiny task override", func(t *testing.T) {
		dst := destinyWith(
			map[string]any{"dsn": "redis://:${ vars.pw }@localhost", "conf_dir": "/etc/redis"},
			contentTask(map[string]any{"pw": "${ input.password }"}, map[string]any{
				"content": "${ vars.dsn }",
				"path":    "${ vars.conf_dir }/redis.conf",
			}))
		in := hopRenderInput(applyHopScenario())
		in.Destiny = &stubDestinyResolver{resolved: dst}
		paths := sealView(t, in)
		if !paths["content"] || paths["path"] {
			t.Errorf("sealed %v, want content and not path", paths)
		}
	})
}

// TestRender_SealANestedSecretSealsTheWholeName — every string of a definition counts,
// at any depth, and the name it defines is tainted whole: a cell reading the map, or a
// public field of it, is sealed.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the map and list arms of
// the walk in sealMemo.rawReads, leaving only the string one. Every case goes red.
func TestRender_SealANestedSecretSealsTheWholeName(t *testing.T) {
	for name, vars := range map[string]map[string]any{
		"in a map":  {"creds": map[string]any{"user": "admin", "password": "${ input.admin_password }"}},
		"in a list": {"creds": map[string]any{"user": "admin", "passwords": []any{"${ input.admin_password }"}}},
	} {
		t.Run(name, func(t *testing.T) {
			paths := sealView(t, hopRenderInput(hopScenario(contentTask(vars, map[string]any{
				"whole": "${ vars.creds }",
				"user":  "${ vars.creds.user }",
				"port":  "${ input.port }",
			}))))
			if !paths["whole"] || !paths["user"] {
				t.Errorf("sealed %v, want whole and user: creds is tainted whole", paths)
			}
			if paths["port"] {
				t.Errorf("a public cell was sealed: %v", paths)
			}
		})
	}

	t.Run("in a service definition over a task override", func(t *testing.T) {
		in := hopRenderInput(hopScenario(contentTask(
			map[string]any{"password": "${ input.admin_password }"},
			map[string]any{"whole": "${ vars.conn }"},
		)))
		in.ServiceVars = serviceView(t, map[string]any{"conn": map[string]any{"opts": []any{"auth=${ vars.password }"}}})
		if paths := sealView(t, in); !paths["whole"] {
			t.Errorf("the cell reading vars.conn is NOT sealed: %v", paths)
		}
	})
}

// TestRender_SealADefinitionReadingVarsWholeOrByIndexIsTaintedWhenAnyNameIs — a
// definition reading `vars[expr]` or `vars` whole names no field the walk can follow,
// so it is tainted when any name of the view is: a service file's index over a task's
// secret would otherwise carry it one hop down, in a file the task's author never sees.
// A view with no tainted name leaves it alone.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the whole-read test from
// cel.SealReads.readsSecret. The secret cases go red.
func TestRender_SealADefinitionReadingVarsWholeOrByIndexIsTaintedWhenAnyNameIs(t *testing.T) {
	cases := map[string]struct {
		service, scenario map[string]any
		cell              string
	}{
		"by a key the text does not spell": {
			service: map[string]any{"k": "pw", "pick": "${ vars[vars.k] }"},
			cell:    "${ vars.pick }",
		},
		"whole": {
			service:  map[string]any{"all": map[string]any{}},
			scenario: map[string]any{"all": "${ vars }"},
			cell:     "${ vars.all.pw }",
		},
	}
	for name, tc := range cases {
		for _, secret := range []bool{true, false} {
			pw := "plain"
			if secret {
				pw = "${ input.admin_password }"
			}
			t.Run(fmt.Sprintf("%s, secret %v", name, secret), func(t *testing.T) {
				scn := hopScenario(contentTask(map[string]any{"pw": pw}, map[string]any{"content": tc.cell}))
				scn.Vars = tc.scenario
				in := hopRenderInput(scn)
				in.ServiceVars = serviceView(t, tc.service)
				if paths := sealView(t, in); paths["content"] != secret {
					t.Errorf("secret task var %v: sealed %v, want content sealed = %v", secret, paths, secret)
				}
			})
		}
	}
}

// TestRender_SealACellReadingVarsWholeIsSealedOnlyWhenAViewNameIsTainted — a cell
// `${ vars }` materializes every definition of the view, so it is sealed when any
// name of the view is tainted — a service one included — and is not when none is.
func TestRender_SealACellReadingVarsWholeIsSealedOnlyWhenAViewNameIsTainted(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(*RenderInput)
		want bool
	}{
		"a tainted task var": {func(in *RenderInput) {
			in.Scenario.Tasks[0].Vars = map[string]any{"pw": "${ input.admin_password }"}
		}, true},
		"a tainted scenario var": {func(in *RenderInput) {
			in.Scenario.Vars = map[string]any{"pw": "${ input.admin_password }"}
		}, true},
		"no tainted name in the view": {func(in *RenderInput) {
			in.Scenario.Vars = map[string]any{"pw": "${ input.port }"}
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			in := hopRenderInput(hopScenario(contentTask(nil, map[string]any{
				"all":  "${ vars }",
				"pass": "${ input.admin_password }",
			})))
			in.ServiceVars = serviceView(t, map[string]any{"conf_dir": "/etc/redis", "dsn": "redis://${ vars.conf_dir }"})
			tc.set(&in)
			paths := sealView(t, in)
			if paths["all"] != tc.want {
				t.Errorf("sealed %v, want all sealed = %v", paths, tc.want)
			}
			if !paths["pass"] {
				t.Errorf("the direct secret cell is not sealed: %v", paths)
			}
		})
	}
}
