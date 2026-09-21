package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/sdk/schema"
)

// SoulModuleSpawner is the narrow contract over [Host.SpawnSoulModule] that
// [KeeperSideModules] spawns through. An interface so the registry is testable
// without forking a real binary — the mirror of soul/internal/runtime's
// PluginSpawner, which exists for the same reason on the other side.
type SoulModuleSpawner interface {
	SpawnSoulModule(ctx context.Context, d Discovered) (SoulModuleSession, error)
}

// SoulModuleSession is one spawned module's session: the Apply RPC and the
// close. Declared here rather than used as *[SoulModulePlugin] so a test can
// substitute it without a subprocess or a socket.
type SoulModuleSession interface {
	Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error)
	Close() error
}

// HostSpawner adapts *[Host] to [SoulModuleSpawner]. Exists to adapt the type:
// SpawnSoulModule returns *[SoulModulePlugin], the registry operates on the
// interface.
type HostSpawner struct{ Host *Host }

// SpawnSoulModule forwards to the host, keeping every gate it applies.
func (s HostSpawner) SpawnSoulModule(ctx context.Context, d Discovered) (SoulModuleSession, error) {
	p, err := s.Host.SpawnSoulModule(ctx, d)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// KeeperSideModules is the set of PLUGIN modules this Keeper may execute
// ITSELF: discovered kind=soul_module entries whose schema document declares
// `side: keeper` ([ADR-0087], NIM-758). It is what the scenario runner falls
// back to when a task's address is not one of the built-in keeper-side core
// modules.
//
// Two maps, not one, and that is the whole design. `mods` holds only what may
// run and is what [KeeperSideModules.LookupKeeperSide] answers from — a
// Soul-side module is not in it and cannot be reached by any lookup, so the
// fail-closed property does not depend on a caller remembering to ask about the
// side. `sides` holds EVERY discovered soul_module and exists only so the
// refusal can tell an operator the truth: "that module runs on the Soul" is a
// different problem from "there is no such module", with a different fix, and
// answering the second for the first is what sends an author looking for a
// typo that is not there.
//
// Immutable after construction, unlike the Soul-side registry: there is no
// keeper-side equivalent of hot-register (ADR-065(d)), so no lock is needed.
type KeeperSideModules struct {
	spawner SoulModuleSpawner
	logger  *slog.Logger

	// mods — `<alias>.<module>` → the entry to spawn, side: keeper only.
	mods map[string]Discovered
	// sides — `<alias>.<module>` → the side it declared, every soul_module.
	sides map[string]schema.Side
}

// NewKeeperSideModules indexes the discovered set by `<alias>.<module>`
// ([Discovered.Address]) — the address level the module part of a task address
// resolves to, before the state suffix. Level 1 is the registration alias the
// slot is named by, so two publishers of one subject cannot collide: the
// operator picked both names.
//
// discovered is what survived [Discover] and [FilterByCatalog], so an entry
// here is already declared in `keeper.yml::plugins.soul_modules`. Entries of
// another kind are skipped, and so is one with no module name (a
// single-endpoint kind).
func NewKeeperSideModules(spawner SoulModuleSpawner, discovered []Discovered, logger *slog.Logger) *KeeperSideModules {
	if logger == nil {
		logger = slog.Default()
	}
	r := &KeeperSideModules{
		spawner: spawner,
		logger:  logger,
		mods:    make(map[string]Discovered),
		sides:   make(map[string]schema.Side),
	}
	for _, d := range discovered {
		if d.Kind() != KindSoulModule || d.Module == "" {
			continue
		}
		addr := d.Address()
		side := DeclaredSide(d)
		r.sides[addr] = side
		if side == schema.SideKeeper {
			r.mods[addr] = d
		}
	}
	return r
}

// LookupKeeperSide returns a module that does a one-shot spawn on every Apply,
// and ONLY for an address whose document declared `side: keeper`. Anything else
// — an unknown address, or a module that runs on the Soul — is a miss here; ask
// [KeeperSideModules.DeclaredSide] to tell those two apart for the operator.
//
// The returned module.SoulModule implements Apply only; Validate/Plan keep the
// BaseModule defaults, as on the Soul side (the apply loop calls neither).
func (r *KeeperSideModules) LookupKeeperSide(base string) (module.SoulModule, bool) {
	if r == nil {
		return nil, false
	}
	d, ok := r.mods[base]
	if !ok {
		return nil, false
	}
	return &keeperSidePlugin{discovered: d, spawner: r.spawner, logger: r.logger}, true
}

// DeclaredSide reports the side the plugin module at base declares, and whether
// this Keeper knows that address at all. Present-and-`soul` is the case worth
// having the method for: it is a registered module that simply does not run
// here.
func (r *KeeperSideModules) DeclaredSide(base string) (schema.Side, bool) {
	if r == nil {
		return schema.SideSoul, false
	}
	side, ok := r.sides[base]
	return side, ok
}

// Names lists the keeper-side plugin addresses, sorted — for the startup log,
// which must be byte-identical for an identical cache.
func (r *KeeperSideModules) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.mods))
	for addr := range r.mods {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

// keeperSidePlugin adapts one-shot spawning to sdk/module.SoulModule: spawn →
// Apply (stream) → forward the ApplyEvents into the caller's stream → Close.
// Any stage error becomes an error for the dispatcher, which turns it into a
// failed task.
//
// Byte-for-byte the Soul-side pluginSoulModule, including the error prefixes
// (`plugin_spawn` / `plugin_apply_rpc` / `plugin_apply_stream` /
// `plugin_apply_forward`): the same failure of the same contract must be
// greppable under one name whichever side ran the module.
type keeperSidePlugin struct {
	module.BaseModule
	discovered Discovered
	spawner    SoulModuleSpawner
	logger     *slog.Logger
}

func (m *keeperSidePlugin) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	sess, err := m.spawner.SpawnSoulModule(ctx, m.discovered)
	if err != nil {
		return fmt.Errorf("plugin_spawn: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			m.logger.Warn("pluginhost: keeper-side plugin close error",
				slog.String("module", m.discovered.Address()),
				slog.Any("error", cerr),
			)
		}
	}()

	rpcStream, err := sess.Apply(ctx, req)
	if err != nil {
		return fmt.Errorf("plugin_apply_rpc: %w", err)
	}
	for {
		ev, err := rpcStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin_apply_stream: %w", err)
		}
		if sendErr := stream.Send(ev); sendErr != nil {
			return fmt.Errorf("plugin_apply_forward: %w", sendErr)
		}
	}
}
