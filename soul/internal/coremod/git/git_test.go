package git_test

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/git"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func newModule(r *internaltest.Runner, stat func(string) (bool, error)) *git.Module {
	return &git.Module{Runner: r, StatDir: stat}
}

func TestValidate(t *testing.T) {
	m := git.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "cloned",
		Params: mustStruct(t, map[string]any{"repo": "git@x:y.git"}),
	})
	if reply.Ok {
		t.Fatal("Validate without path: ok unexpectedly")
	}
}

func TestApply_Cloned_AlreadyPresent_NoOp(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 0}
	r.On("[cwd=/srv/app] git rev-parse HEAD", util.Result{ExitCode: 0, Stdout: "deadbeef\n"})
	m := newModule(r, func(p string) (bool, error) { return p == "/srv/app/.git", nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cloned",
		Params: mustStruct(t, map[string]any{
			"repo": "https://example/x.git",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true for an already-cloned repo")
	}
	for _, c := range r.Calls {
		if c == "git clone --branch main -- https://example/x.git /srv/app" {
			t.Fatalf("unexpected clone with cloned+exists")
		}
	}
}

func TestApply_Cloned_Missing_Clones(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 0}
	r.On("git clone --branch main -- https://example/x.git /srv/app", util.Result{ExitCode: 0})
	r.On("[cwd=/srv/app] git rev-parse HEAD", util.Result{ExitCode: 0, Stdout: "abc123\n"})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cloned",
		Params: mustStruct(t, map[string]any{
			"repo": "https://example/x.git",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false on clone")
	}
	if got := stream.Last().Output.Fields["head"].GetStringValue(); got != "abc123" {
		t.Fatalf("head=%q want abc123", got)
	}
}

func TestApply_Cloned_DepthAndBranch(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("git clone --branch v2 --depth 1 -- https://example/x.git /srv/app", util.Result{ExitCode: 0})
	r.On("[cwd=/srv/app] git rev-parse HEAD", util.Result{ExitCode: 0, Stdout: "v2sha\n"})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cloned",
		Params: mustStruct(t, map[string]any{
			"repo":   "https://example/x.git",
			"path":   "/srv/app",
			"branch": "v2",
			"depth":  float64(1),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false on clone depth+branch")
	}
}

func TestApply_Pulled_Existing_HeadSame_NoChange(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	// before and after are the same sha
	r.OnSeq("[cwd=/srv/app] git rev-parse HEAD",
		util.Result{ExitCode: 0, Stdout: "same\n"},
		util.Result{ExitCode: 0, Stdout: "same\n"},
	)
	r.On("[cwd=/srv/app] git pull --ff-only", util.Result{ExitCode: 0})
	m := newModule(r, func(p string) (bool, error) { return p == "/srv/app/.git", nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "pulled",
		Params: mustStruct(t, map[string]any{
			"repo": "https://example/x.git",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true when HEAD did not change")
	}
}

func TestApply_Pulled_HeadMoves_Changed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.OnSeq("[cwd=/srv/app] git rev-parse HEAD",
		util.Result{ExitCode: 0, Stdout: "old\n"},
		util.Result{ExitCode: 0, Stdout: "new\n"},
	)
	r.On("[cwd=/srv/app] git pull --ff-only", util.Result{ExitCode: 0})
	m := newModule(r, func(p string) (bool, error) { return p == "/srv/app/.git", nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "pulled",
		Params: mustStruct(t, map[string]any{
			"repo": "https://example/x.git",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false on HEAD shift")
	}
	if got := stream.Last().Output.Fields["head"].GetStringValue(); got != "new" {
		t.Fatalf("head=%q want new", got)
	}
}

func TestApply_Pulled_Missing_Clones(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("git clone --branch main -- https://example/x.git /srv/app", util.Result{ExitCode: 0})
	r.On("[cwd=/srv/app] git rev-parse HEAD", util.Result{ExitCode: 0, Stdout: "first\n"})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "pulled",
		Params: mustStruct(t, map[string]any{
			"repo": "https://example/x.git",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false on initial clone in pulled")
	}
}

// TestApply_Cloned_DashRepo_SeparatorPresent guards the security review L1
// fix: a repo starting with `-` must go after `--`, otherwise git parses it
// as an option (argument injection).
func TestApply_Cloned_DashRepo_SeparatorPresent(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("git clone --branch main -- --upload-pack=evil /srv/app", util.Result{ExitCode: 0})
	r.On("[cwd=/srv/app] git rev-parse HEAD", util.Result{ExitCode: 0, Stdout: "sha\n"})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cloned",
		Params: mustStruct(t, map[string]any{
			"repo": "--upload-pack=evil",
			"path": "/srv/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("clone with `--` should pass, got failed: %+v", stream.Last())
	}
}

func TestApply_CloneFailure(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1, Stderr: "fatal: repo not found"}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cloned",
		Params: mustStruct(t, map[string]any{
			"repo": "bad",
			"path": "/srv/app",
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("Failed=false on clone failure")
	}
}
