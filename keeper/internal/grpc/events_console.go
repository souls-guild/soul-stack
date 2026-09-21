package grpc

import (
	"context"
	"log/slog"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Soul -> Keeper half of the interactive console contract (NIM-143).
//
// The three console payloads (`ConsoleOpened` / `ConsoleChunk` / `ConsoleExit`)
// are handed straight to the session manager, which owns the routing back to an
// operator socket — including the case where that socket lives on another
// Keeper instance. Nothing is persisted: a console is a live stream, not a run
// record. Session recording is a separate slice (NIM-145).
//
// ConsoleHub==nil (a build with no console wire-up) drops the frames with a
// debug line, like handleErrandResult with a nil ApplyBus: a stream must not
// fail over a subsystem the deployment did not enable.

// ConsoleHub is the session-manager surface the EventStream needs. An interface
// rather than the concrete *console.Hub so this package does not depend on
// keeper/internal/console — the dependency runs the other way at wire-up time
// (the Hub is handed grpc.Outbound as its dispatcher), and a direct import here
// would close the cycle.
type ConsoleHub interface {
	Deliver(ctx context.Context, sid string, msg *keeperv1.FromSoul)
}

// handleConsoleUpstream routes one console payload to the session manager.
func (h *eventStreamHandler) handleConsoleUpstream(ctx context.Context, sid, sessionID string, msg *keeperv1.FromSoul) {
	if h.deps.ConsoleHub == nil {
		h.logger.Debug("eventstream: console payload without a console hub - drop",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	h.deps.ConsoleHub.Deliver(ctx, sid, msg)
}
