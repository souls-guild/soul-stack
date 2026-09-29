package render

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

type costKV struct{ n atomic.Int64 }

func (c *costKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	c.n.Add(1)
	return map[string]any{"value": "secret-" + path}, nil
}

func costHosts(n int) []*topology.HostFacts {
	out := make([]*topology.HostFacts, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, hostWithRole(fmt.Sprintf("h%02d.example.com", i), "replica", []string{"redis"},
			map[string]any{"primary_ip": fmt.Sprintf("10.0.0.%d", i+1)}, map[string]any{"family": "debian"}))
	}
	return out
}

// costScenario — five inputs: one per-host, one a vault() read, three literal.
func costScenario() (*config.ScenarioManifest, *stubDestinyResolver) {
	const tmplPath = "templates/redis.conf.tmpl"
	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name: "redis",
		Input: config.InputSchemaMap{
			"master_addr": {Type: "string", Required: true},
			"port":        {Type: "string"},
			"maxmem":      {Type: "string"},
			"appendonly":  {Type: "string"},
			"pw":          {Type: "string"},
		},
		Tasks: []config.Task{{
			Name: "write redis.conf",
			Module: &config.ModuleTask{
				Module: moduleFileRendered,
				Params: map[string]any{"path": "/etc/redis/redis.conf", "template": tmplPath},
			},
		}},
		Templates: fakeReader{files: map[string][]byte{
			tmplPath: []byte("replicaof {{ .input.master_addr }} {{ .input.port }}\n"),
		}},
	}}
	return applyScenario("redis", map[string]any{
		"master_addr": "${ soulprint.self.network.primary_ip }",
		"port":        "${ '6379' }",
		"maxmem":      "${ incarnation.id }-mem",
		"appendonly":  "${ 'yes' }",
		"pw":          "${ vault('secret/redis/admin').value }",
	}), res
}

// ★ The cost NIM-908 does NOT pay. apply.input renders once per host now, and a
// `${ vault(...) }` among its values would be an extra Vault round-trip per host if
// the resolution memo were not per-PASS ([WithVaultFence]) — nine hosts, nine reads
// of the same path, on a system where reading a secret is audited.
//
// The memo makes it one. Pinning it here because the guarantee is a property of
// WHERE the memo is bound, not of this loop: moving the fence inside the per-host
// iteration would leave every test green and multiply the audited reads by the
// roster size.
func TestApplyInput_VaultReadsDoNotScaleWithTheRoster(t *testing.T) {
	for _, n := range []int{1, 9} {
		manifest, res := costScenario()
		kv := &costKV{}
		engine, err := cel.New(cel.WithVault(kv))
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		p := NewPipeline(nil, engine, nil, nil)
		in := perHostRenderInput(manifest, res, costHosts(n))
		if _, _, err := p.Render(context.Background(), in); err != nil {
			t.Fatalf("Render with %d hosts: %v", n, err)
		}
		if got := kv.n.Load(); got != 1 {
			t.Errorf("hosts=%d: ReadKV called %d times, want 1 -- the per-pass vault memo is not covering the per-host apply.input loop", n, got)
		}
	}
}

// BenchmarkApplyInput_ResolveByRoster is where the cost figures quoted in
// [Pipeline.resolveApplyInput] and in the ADR-009 amendment come from, so they can be
// re-measured rather than believed:
//
//	go test ./internal/render/ -run '^$' -bench BenchmarkApplyInput_ResolveByRoster -benchtime 2000x -count 5
//
// The `pw` entry is dropped here because it calls `vault()` and this Engine has no KV
// reader — which is also why the quoted figure says FOUR inputs while costScenario
// declares five. Measured 2026-09-28 on this tree, five runs each: ~0.17 ms at one
// host, ~0.69 ms at nine, i.e. ~0.065 ms per extra host. Not an assertion: a timing
// threshold in the gate is a flake, and the number
// that matters for correctness — that vault() does NOT scale with the roster — is
// asserted by the test above.
func BenchmarkApplyInput_ResolveByRoster(b *testing.B) {
	for _, n := range []int{1, 9} {
		b.Run(fmt.Sprintf("hosts=%d", n), func(b *testing.B) {
			manifest, res := costScenario()
			delete(manifest.Tasks[0].Apply.Input, "pw")
			delete(res.resolved.Input, "pw")
			e, err := cel.New()
			if err != nil {
				b.Fatal(err)
			}
			p := NewPipeline(nil, e, nil, nil)
			hosts := costHosts(n)
			in := perHostRenderInput(manifest, res, hosts)
			applier := manifest.Tasks[0]
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// The classification is inside the loop because renderApplyDestiny pays
				// it once per applier per pass, exactly like the resolve.
				variant := hostVariantInputNames(e, applier, keeperRegisterNames(manifest))
				if _, _, _, err := p.resolveApplyInput(in, applier, res.resolved, hosts, variant); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
