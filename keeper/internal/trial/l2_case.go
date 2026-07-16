//go:build integration

package trial

import (
	"fmt"
	"os"

	yaml "github.com/goccy/go-yaml"
)

// L2Case — L2 level case (execution on ephemeral Linux stand with post-apply
// verification, ADR-023). Unlike L0 (render-only) L2 actually applies
// rendered plan on host via `soul apply` (push-oneshot, ADR-004) and
// verifies result with verify tasks. Structure is read-only after loading.
//
// Format — extension of L0 case with stand:/input:/expect_idempotent:/verify: fields
// (docs/destiny/testing.md §L2). Decoded only under build-tag integration:
// default harness marks L2 case as Skipped (see Run / isL2Case), does not
// parse its structure.
type L2Case struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Stand       Stand          `yaml:"stand"`
	Input       map[string]any `yaml:"input,omitempty"`
	// Idempotent — double run of same ApplyRequest: second application must
	// not change host state (all register.changed==false). Defaults to true:
	// idempotence — mandatory part of L2 check (testing.md §L2 / open Q №5).
	Idempotent *bool    `yaml:"expect_idempotent,omitempty"`
	Verify     []Verify `yaml:"verify,omitempty"`
}

// Stand — description of ephemeral stand (docs/destiny/testing.md §L2). Reuse
// of mode semantics: push (push-oneshot soul apply per ADR-004) — L2 pilot
// does not introduce new stand values.
type Stand struct {
	// Driver — stand driver. Pilot supports only docker.
	Driver string `yaml:"driver"`
	// Image — base image of stand host (e.g. ubuntu:24.04). For init: systemd
	// field is ignored: stand built from debian-12.Dockerfile (see StartL2Stand).
	Image string `yaml:"image"`
	// Mode — how destiny reaches stand. push = push-oneshot soul apply
	// (Keeper renders plan, delivers soul + ApplyRequest to host, executes).
	Mode string `yaml:"mode"`
	// Init — stand init system (optional). `none` (default) — container lives
	// under `sleep infinity`, no PID1-init (current L2 pilot behavior). `systemd` —
	// systemd-PID1 stand (privileged, image from debian-12.Dockerfile e2e-live),
	// needed for cases with core.service.* (is-active/restart require live systemd).
	Init string `yaml:"init,omitempty"`
}

// Stand init modes (stand.init field). Closed-set: unknown value
// rejected on case load (validate).
const (
	// StandInitNone — no PID1-init: container on `sleep infinity` (default).
	StandInitNone = "none"
	// StandInitSystemd — systemd-PID1 stand for core.service.* cases.
	StandInitSystemd = "systemd"
)

// init returns effective value of stand.init (default none if empty).
func (s Stand) init() string {
	if s.Init == "" {
		return StandInitNone
	}
	return s.Init
}

// Verify — one result check (docs/destiny/testing.md §L2). Each
// executes one module task on same stand with same `soul apply` (single-task
// ApplyRequest) and verifies register-output fields via Expect. No separate assertion
// DSL (testing.md) — checks expressed with same modules.
type Verify struct {
	Name   string      `yaml:"name"`
	Apply  VerifyApply `yaml:"apply"`
	Expect Expect      `yaml:"expect"`
}

// VerifyApply — module task of verify step. Module — full name with state suffix
// (e.g. core.cmd.shell), as in destiny task; Params — its params (without
// CEL-render: verify steps specified literally on stand).
type VerifyApply struct {
	Module string         `yaml:"module"`
	Params map[string]any `yaml:"params"`
}

// Expect — expectations on register-output of verify task (docs/destiny/testing.md §L2).
// Set of keys — minimum of recorded (exit_code / stdout / stdout_contains).
// Pointers/optionality: unspecified field not checked (partial assert).
type Expect struct {
	ExitCode       *int    `yaml:"exit_code,omitempty"`
	Stdout         *string `yaml:"stdout,omitempty"`
	StdoutContains string  `yaml:"stdout_contains,omitempty"`
}

// expectIdempotent returns effective value of expect_idempotent (default true).
func (c *L2Case) expectIdempotent() bool {
	if c.Idempotent == nil {
		return true
	}
	return *c.Idempotent
}

// LoadL2Case reads and validates L2 case.yml (strict decode: unknown key is
// error). path — path to file or case directory (resolveCaseFile).
func LoadL2Case(path string) (*L2Case, string, error) {
	file, err := resolveCaseFile(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("trial: reading %s: %w", file, err)
	}
	var c L2Case
	if err := yaml.UnmarshalWithOptions(data, &c, yaml.Strict()); err != nil {
		return nil, "", fmt.Errorf("trial: parsing L2 %s: %w", file, err)
	}
	if err := c.validate(); err != nil {
		return nil, "", fmt.Errorf("trial: %s: %w", file, err)
	}
	return &c, file, nil
}

func (c *L2Case) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name: required")
	}
	if c.Stand.Driver != "docker" {
		return fmt.Errorf("stand.driver: L2 pilot supports only docker (got %q)", c.Stand.Driver)
	}
	if c.Stand.Image == "" {
		return fmt.Errorf("stand.image: required")
	}
	if c.Stand.Mode != "push" {
		return fmt.Errorf("stand.mode: L2 pilot supports only push (got %q)", c.Stand.Mode)
	}
	switch c.Stand.Init {
	case "", StandInitNone, StandInitSystemd:
	default:
		return fmt.Errorf("stand.init: unknown value %q (allowed none|systemd)", c.Stand.Init)
	}
	if len(c.Verify) == 0 {
		return fmt.Errorf("verify: empty (L2 checks apply result with verify tasks)")
	}
	for i, v := range c.Verify {
		if v.Apply.Module == "" {
			return fmt.Errorf("verify[%d].apply.module: required", i)
		}
	}
	return nil
}
