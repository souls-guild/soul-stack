package scenario

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// listedHostScenarioName names the synthetic scenario a listed host's destiny is
// rendered in. It appears only in render diagnostics.
const listedHostScenarioName = "_ssh_apply"

// inputNameRe is the shape of a destiny input name that can be read as
// `input.<name>` in CEL.
var inputNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// listedHostDestinyRenderer is the render `core.ssh.apply` runs for a host that is
// not in this run's roster (NIM-905): the destiny resolved through THIS run's
// resolver (service.yml destiny[] at its ref, with the compat gate and the
// provenance record an `apply:` gets), rendered against THIS run's incarnation so
// the vault fence keyed on the service stays on.
//
// A run with no destiny source still gets a render: the render then refuses the
// destiny exactly as it refuses an `apply:` without one.
func (r *Runner) listedHostDestinyRenderer(res *destinyResolver, meta render.IncarnationMeta, modules config.ModuleManifestResolver, secretStateFields map[string]bool) coremodutil.DestinyRenderFunc {
	if r.deps.Render == nil {
		return nil
	}
	// A nil *destinyResolver must not become a non-nil interface: the render would
	// call Resolve on it instead of refusing.
	var resolver render.DestinyResolver
	if res != nil {
		resolver = res
	}
	return func(ctx context.Context, name, sid string, input map[string]any) (*coremodutil.RenderedDestiny, error) {
		return renderListedHostDestiny(ctx, r.deps.Render, resolver, meta, modules, secretStateFields, name, sid, input)
	}
}

// capturingResolver remembers the destiny the render resolved, so its `input:`
// schema can say which values are secret.
type capturingResolver struct {
	inner render.DestinyResolver
	got   *render.ResolvedDestiny
}

func (c *capturingResolver) Resolve(ctx context.Context, name string) (*render.ResolvedDestiny, error) {
	d, err := c.inner.Resolve(ctx, name)
	if err == nil {
		c.got = d
	}
	return d, err
}

// renderListedHostDestiny renders destiny `name` for the single host `sid` as a
// synthetic scenario of one `apply:` task whose roster is that host, with no
// Soulprint — the state the pipeline already serves for a host whose first report
// has not arrived.
//
// ★ The values reach the destiny BY REFERENCE: the applier's `input:` is
// `${ input.<name> }` and the values are the scenario input. A value is data — a
// token, an address a cloud API returned — and one that happens to contain
// `${ … }` must arrive as that text. Copying the values into `apply: input:`, as
// the bare push API does, would evaluate them on the Keeper.
func renderListedHostDestiny(ctx context.Context, p *render.Pipeline, res render.DestinyResolver, meta render.IncarnationMeta, modules config.ModuleManifestResolver, secretStateFields map[string]bool, name, sid string, input map[string]any) (*coremodutil.RenderedDestiny, error) {
	if sid == "" {
		return nil, errors.New("scenario: render for a listed host: empty sid")
	}
	refs := make(map[string]any, len(input))
	for k := range input {
		if !inputNameRe.MatchString(k) {
			return nil, fmt.Errorf("scenario: destiny input name %q is not an identifier", k)
		}
		refs[k] = "${ input." + k + " }"
	}

	sealed := render.NewSealedSet()
	var (
		destiny  render.DestinyResolver
		captured *capturingResolver
	)
	if res != nil {
		captured = &capturingResolver{inner: res}
		destiny = captured
	}
	tasks, _, err := p.Render(ctx, render.RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: listedHostScenarioName,
			Tasks: []config.Task{{
				Name:  "ssh.apply " + name,
				Apply: &config.ApplyTask{Destiny: name, Input: refs},
			}},
		},
		Input:       input,
		Incarnation: meta,
		Hosts:       []*topology.HostFacts{{SID: sid, Transport: config.TransportSSH}},
		Destiny:     destiny,
		Modules:     modules,
		Ctx:         ctx,
		Sealed:      sealed,
		// Carried as renderApplyDestiny carries it: State is nil here, so it
		// addresses nothing today, and forwarding State one day is one edit.
		SecretStateFields: secretStateFields,
	})
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("scenario: destiny %q rendered to no task for %q", name, sid)
	}

	// Two sources, because each misses what the other holds: a sealed cell is
	// masked as its whole value, and a secret input interpolated into a longer
	// string is a sealed cell whose whole value an agent never echoes alone.
	var secrets []string
	paths := sealed.Paths()
	for _, t := range tasks {
		if t.Params != nil {
			secrets = append(secrets, render.SealedValues(t.Params.AsMap(), paths)...)
		}
	}
	if captured != nil && captured.got != nil {
		for k, def := range captured.got.Input {
			if v, ok := input[k].(string); ok && v != "" && def != nil && def.Secret {
				secrets = append(secrets, v)
			}
		}
	}
	sort.Strings(secrets)

	ref := ""
	if dr, ok := res.(*destinyResolver); ok {
		ref = dr.deps[name].Ref
	}
	return &coremodutil.RenderedDestiny{
		Ref:     ref,
		Tasks:   render.ToProtoTasksForHost(tasks, sid),
		Secrets: secrets,
	}, nil
}
