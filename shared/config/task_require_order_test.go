package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// A `require:` barrier is resolved at the awaiting task's plan position, so a
// source that starts later is never in the set: the runner neither waits nor
// fails (soul/internal/runtime.asyncFlows.barriers, ADR-0075(b.1)). These guard
// the static rejection of every shape of that, and — the other half — that an
// honest barrier is not caught by it. The subject is a destiny's task file, the
// only place `require:` is legal since NIM-941.

func TestRequireOrder_ForwardIsRejected(t *testing.T) {
	src := `- module: core.service.restarted
  require: [conf]
  params: { name: nginx }
- module: core.file.rendered
  async: true
  register: conf
  params: { src: a.tmpl, path: /etc/nginx/a.conf }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if !hasCodeAt(diags, "require_forward_reference", "$[0].require[0]") {
		dump(t, diags)
		t.Fatalf("expected require_forward_reference on the forward barrier")
	}
}

// A task naming its own register is the degenerate cycle: it is not launched
// when its own barrier resolves, so the wait is a no-op like any other forward
// one. Rejecting forward edges is what makes a require: cycle unrepresentable.
func TestRequireOrder_SelfReferenceIsRejected(t *testing.T) {
	src := `- module: core.exec.run
  register: probe
  require: [probe]
  params: { cmd: "true" }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if !hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("expected require_forward_reference on a self-barrier")
	}
}

// Position is the in-order walk, block: included — a block fans out at its own
// position and its children take the indices straight after it. So a barrier
// inside a block still points forward when its source sits below the block.
func TestRequireOrder_ForwardIntoLaterTaskFromBlockChild(t *testing.T) {
	src := `- block:
    - module: core.service.restarted
      require: [conf]
      params: { name: nginx }
- module: core.file.rendered
  async: true
  register: conf
  params: { src: a.tmpl, path: /etc/nginx/a.conf }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if !hasCodeAt(diags, "require_forward_reference", "$[0].block[0].require[0]") {
		dump(t, diags)
		t.Fatalf("expected require_forward_reference on a barrier inside a block")
	}
}

// The mirror case: the source is a block CHILD below the awaiting task. The
// child's position is after the block's, so this too is forward.
func TestRequireOrder_ForwardIntoLaterBlockChild(t *testing.T) {
	src := `- module: core.service.restarted
  require: [conf]
  params: { name: nginx }
- block:
    - module: core.file.rendered
      async: true
      register: conf
      params: { src: a.tmpl, path: /etc/nginx/a.conf }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if !hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("expected require_forward_reference for a source inside a later block")
	}
}

// Negative — the barrier every plan is supposed to write: the source is above,
// so the runner has launched it by the time the barrier resolves.
func TestRequireOrder_BackwardIsAccepted(t *testing.T) {
	src := `- module: core.file.rendered
  async: true
  register: conf
  params: { src: a.tmpl, path: /etc/nginx/a.conf }
- module: core.service.restarted
  require: [conf]
  params: { name: nginx }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("a backward barrier must not be flagged")
	}
}

// Negative — a barrier between siblings of one block. They render in order at
// contiguous indices, so an earlier sibling is a legitimate source.
func TestRequireOrder_BackwardWithinBlockIsAccepted(t *testing.T) {
	src := `- block:
    - module: core.file.rendered
      async: true
      register: conf
      params: { src: a.tmpl, path: /etc/nginx/a.conf }
    - module: core.service.restarted
      require: [conf]
      params: { name: nginx }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("a barrier on an earlier sibling of the same block must not be flagged")
	}
}

// Negative — `require: all` carries no names and is positional by definition
// ("every async task started earlier in this run"), so it cannot point forward.
func TestRequireOrder_RequireAllIsAccepted(t *testing.T) {
	src := `- module: core.exec.run
  require: all
  params: { cmd: "check-cluster-state.sh" }
- module: core.file.rendered
  async: true
  register: conf
  params: { src: a.tmpl, path: /etc/nginx/a.conf }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("require: all must not be flagged")
	}
}

// A name no task declares is unknown_register_reference's — raising both codes
// on one element would just double the noise for one typo.
func TestRequireOrder_UnknownNameStaysUnknownOnly(t *testing.T) {
	src := `- module: core.service.restarted
  require: [typo]
  params: { name: nginx }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if !hasCode(diags, "unknown_register_reference") {
		dump(t, diags)
		t.Fatalf("expected unknown_register_reference")
	}
	if hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("an unknown name must not also be reported as a forward barrier")
	}
}

// A CEL-wrapped element resolves to a name only at render, so there is no
// position to compare — skipped rather than guessed at, as in checkRefList.
func TestRequireOrder_CELWrappedElementIsSkipped(t *testing.T) {
	src := `- module: core.service.restarted
  require: ["${ input.barrier }"]
  params: { name: nginx }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if hasCode(diags, "require_forward_reference") {
		dump(t, diags)
		t.Fatalf("a CEL-wrapped element must not be flagged")
	}
}

// The rule rides validateTaskRefs, which runs on every task file a destiny is
// made of — an included body too, not only `tasks/main.yml`.
func TestRequireOrder_ForwardInADestinyIncludedBody(t *testing.T) {
	body := `- module: core.service.restarted
  require: [conf]
  params: { name: nginx }
- module: core.file.rendered
  async: true
  register: conf
  params: { src: a.tmpl, path: /etc/nginx/a.conf }
`
	tasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte("- include: body.yml\n"), ValidateOptions{DestinyTasks: true})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("the including file is clean")
	}
	_, idiags := ExpandIncludesInDestiny(tasks, mapResolver(map[string]string{"body.yml": body}))
	if !hasCodeAt(idiags, "require_forward_reference", "$[0].require[0]") {
		dump(t, idiags)
		t.Fatalf("expected require_forward_reference in a destiny's included body")
	}
}
