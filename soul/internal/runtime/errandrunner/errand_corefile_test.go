package errandrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sdkmod "github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"
)

// This file exercises the NIM-488 fix against the REAL core.file module and a
// real filesystem, not a fixture: a fake that answers "ok" proves nothing about
// whether a dry_run Errand actually reaches core.file.Plan.
//
// core.file carries PlanReadSafe and no ErrandReadSafe — before NIM-488 every
// request below was terminal with `errand_module_not_allowed`, which is what
// made ErrandRequest.dry_run unreachable for every module in the tree.

func realFileRunner() *Runner {
	return New(coremod.NewRegistry(map[string]sdkmod.SoulModule{
		file.Name: file.New(),
	}), nil, nil)
}

func TestErrand_DryRun_ReachesCoreFilePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "reference.txt")
	if err := os.WriteFile(src, []byte("reference body\n"), 0o600); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	target := filepath.Join(dir, "target.txt")

	r := realFileRunner()
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-nim488-plan",
		Module:   "core.file.present",
		Input:    mustStruct(t, map[string]any{"path": target, "src": src}),
		DryRun:   true,
	})

	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		t.Fatalf("status = %v; want SUCCESS (err=%q)", res.GetStatus(), res.GetErrorMessage())
	}
	// Pure-read: Plan compares, Apply writes. The target must still not exist.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target %s exists after a dry_run Errand; Plan must not write (stat err=%v)", target, err)
	}
}

// TestErrand_DryRun_PlanActuallyRanOnTheHost proves the Plan body executed
// rather than the runner short-circuiting to a bare SUCCESS: core.file.planPresent
// reads `src` off the disk and fails when it is missing. A stub that answered
// "ok" without touching the filesystem would return SUCCESS here.
//
// It does NOT discriminate Plan from Apply — Apply fails on the same missing
// `src` with the same message. What rules Apply out is the no-write assertion in
// TestErrand_DryRun_ReachesCoreFilePlan; the two are a pair.
func TestErrand_DryRun_PlanActuallyRanOnTheHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there.txt")

	r := realFileRunner()
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-nim488-planread",
		Module:   "core.file.present",
		Input:    mustStruct(t, map[string]any{"path": filepath.Join(dir, "target.txt"), "src": missing}),
		DryRun:   true,
	})

	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
		t.Fatalf("status = %v; want FAILED from core.file.Plan (err=%q)", res.GetStatus(), res.GetErrorMessage())
	}
	if !strings.Contains(res.GetErrorMessage(), "no such file") {
		t.Errorf("error_message = %q; want core.file's own read-src failure — a SUCCESS here would mean Plan never ran", res.GetErrorMessage())
	}
}

// TestErrand_ApplyPathStaysClosedOnCoreFile is the security half against the
// real module: admitting core.file to dry_run must not admit it to ad-hoc
// Apply. An omitted dry_run field is the same wire state as false (proto3 bool
// zero value), so both must be refused, and nothing may be written.
func TestErrand_ApplyPathStaysClosedOnCoreFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		dryRun bool
		set    bool
	}{
		{"explicit false", false, true},
		{"field omitted", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			target := filepath.Join(dir, "written.txt")

			req := &keeperv1.ErrandRequest{
				ErrandId: "e-nim488-apply-" + tc.name,
				Module:   "core.file.present",
				Input:    mustStruct(t, map[string]any{"path": target, "content": "pwned"}),
			}
			if tc.set {
				req.DryRun = tc.dryRun
			}

			res := realFileRunner().Run(context.Background(), req)
			if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED {
				t.Fatalf("status = %v; want MODULE_NOT_ALLOWED (err=%q)", res.GetStatus(), res.GetErrorMessage())
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Errorf("%s was written through an ad-hoc Errand; core.file.Apply must stay unreachable (stat err=%v)", target, err)
			}
		})
	}
}
