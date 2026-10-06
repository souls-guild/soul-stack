package redis

import (
	"context"
	"testing"
	"time"
)

// A linear run's step rows go to the work-queue only when every live Keeper
// claims steps correctly (ADR-027 amend (p)(d)(ii)); anything short of a record
// that says so reads as "cannot".
func TestAllLiveHaveCapability(t *testing.T) {
	const declares = `{"started_at":"2026-10-06T00:00:00Z","kid":"x","capabilities":["step_queue"]}`
	cases := []struct {
		name  string
		metas map[string]string
		want  bool
	}{
		{"every live instance declares it", map[string]string{
			testKIDa: declares,
			testKIDb: `{"kid":"b","capabilities":["other","step_queue"]}`,
		}, true},
		{"an older instance's record", map[string]string{
			testKIDa: declares,
			testKIDb: `{"started_at":"2026-10-06T00:00:00Z","kid":"b"}`,
		}, false},
		{"the bare-KID fallback record", map[string]string{
			testKIDa: declares,
			testKIDb: testKIDb,
		}, false},
		{"another capability only", map[string]string{
			testKIDa: `{"kid":"a","capabilities":["other"]}`,
		}, false},
		{"no live instance", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClientMR(t)
			ctx := context.Background()
			for kid, meta := range tc.metas {
				if err := RegisterInstance(ctx, c, kid, meta, 30*time.Second, false); err != nil {
					t.Fatalf("RegisterInstance %s: %v", kid, err)
				}
			}
			got, err := AllLiveHaveCapability(ctx, c, CapabilityStepQueue)
			if err != nil {
				t.Fatalf("AllLiveHaveCapability: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllLiveHaveCapability_RedisDownIsCannot(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	if err := RegisterInstance(ctx, c, testKIDa, `{"capabilities":["step_queue"]}`, 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	mr.Close()
	got, err := AllLiveHaveCapability(ctx, c, CapabilityStepQueue)
	if err == nil || got {
		t.Errorf("got %v, err %v; want false with an error", got, err)
	}
}
