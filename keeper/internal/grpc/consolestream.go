package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ConsoleStream carries one interactive console session on its own bidi stream
// (ADR-0074 amendment 2026-07-27, NIM-188).
//
// Why a second RPC after NIM-142 deliberately avoided one: on EventStream a
// console shares a single write mutex and a single HTTP/2 flow-control window
// with the run's event traffic. A `yes`-style flood then either delays
// TaskEvent/RunResult or has to be dropped, and a large paste of keystrokes
// competes with apply dispatch for the same 10-slot outbound queue. Splitting
// the transport removes the contention instead of rationing it.
//
// It does NOT open a session and it does NOT re-authorize one. `soul.console`
// is checked before a session is minted, at the operator's socket
// (ADR-0074(c)); this stream only attaches to an id the Hub already minted, and
// [ConsoleStreamManager.lookup] hands it frames only when the session's SID
// matches the peer cert of the Soul that dialed (ADR-012(i)). Attaching to an
// unknown or foreign id therefore buys nothing: nothing is ever routed to it.
func (h *eventStreamHandler) ConsoleStream(stream grpclib.BidiStreamingServer[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul]) error {
	ctx := stream.Context()
	sid, ok := authenticatedSIDFrom(ctx)
	if !ok {
		h.logger.Error("ConsoleStream invoked without authenticated SID — interceptor misconfigured")
		return status.Error(codes.Internal, "authentication context missing")
	}
	if h.deps.Manager == nil {
		return status.Error(codes.Unavailable, "console transport is not enabled")
	}

	sessionID, err := recvConsoleAttach(stream)
	if err != nil {
		return err
	}

	entry, err := h.deps.Manager.Consoles().Register(sid, sessionID)
	if err != nil {
		h.logger.Warn("console stream: attach refused",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		if errors.Is(err, ErrConsoleStreamTaken) {
			return status.Error(codes.AlreadyExists, "console stream already attached for this session")
		}
		return status.Error(codes.ResourceExhausted, "too many console streams")
	}

	// Acknowledge before anything else: the ack is what tells the Soul this
	// Keeper implements the RPC at all, so it must precede every other frame of
	// the session and must not wait behind the writer's queue.
	if err := stream.Send(&keeperv1.ConsoleToSoul{
		Payload: &keeperv1.ConsoleToSoul_ConsoleAttached{
			ConsoleAttached: &keeperv1.ConsoleAttached{SessionId: sessionID},
		},
	}); err != nil {
		h.deps.Manager.Consoles().Unregister(entry)
		return err
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// Ranging over the queue is the whole writer: Unregister closes it,
		// which is the only way this goroutine ends. A Send error means the
		// stream is already gone and the receive loop is about to return.
		for msg := range entry.outCh {
			if err := stream.Send(msg); err != nil {
				h.logger.Debug("console stream: send failed",
					slog.String("sid", sid),
					slog.String("session_id", sessionID),
					slog.Any("error", err))
				return
			}
		}
	}()
	defer func() {
		h.deps.Manager.Consoles().Unregister(entry)
		<-writerDone
	}()

	h.logger.Info("console stream: attached",
		slog.String("sid", sid), slog.String("session_id", sessionID))

	terminal, err := h.runConsoleStream(ctx, stream, sid, sessionID)
	if !terminal {
		// The transport died with the pty still live as far as anyone here
		// knows. The pty itself is already gone — it cannot outlive the stream
		// that carried it — so the operator gets the terminal frame that would
		// otherwise never arrive, and their pane stops pretending to be alive.
		h.logger.Warn("console stream: closed without a terminal frame — synthesizing exit",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		h.handleConsoleUpstream(ctx, sid, sessionID, &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
				SessionId:    sessionID,
				ExitCode:     -1,
				Reason:       keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN,
				ErrorMessage: "console stream closed",
			}},
		})
	}
	return err
}

// runConsoleStream is the receive loop. It reports whether the Soul delivered a
// terminal ConsoleExit before the stream ended.
func (h *eventStreamHandler) runConsoleStream(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul],
	sid, sessionID string,
) (terminal bool, err error) {
	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return terminal, nil
			}
			return terminal, recvErr
		}

		// One stream carries one session, so a frame naming another one is a
		// Soul-side bug or an injection attempt. Either way it is not routable
		// here: dropping it keeps the "stream == session" invariant that makes
		// the SID check sufficient.
		if id := upstreamConsoleSessionID(msg); id != sessionID {
			h.logger.Warn("console stream: frame for a foreign session — dropping",
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
				slog.String("frame_session_id", id))
			continue
		}

		if _, isExit := msg.GetPayload().(*keeperv1.ConsoleFromSoul_ConsoleExit); isExit {
			terminal = true
		}

		upstream := consoleFromSoul(msg)
		if upstream == nil {
			// A second ConsoleAttach, or a payload this build does not know.
			continue
		}
		h.handleConsoleUpstream(ctx, sid, sessionID, upstream)
	}
}

// recvConsoleAttach reads the mandatory first frame and returns the session id
// the stream is attaching to.
func recvConsoleAttach(stream grpclib.BidiStreamingServer[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul]) (string, error) {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", status.Error(codes.InvalidArgument, "console stream closed before ConsoleAttach")
		}
		return "", err
	}
	attach := first.GetConsoleAttach()
	if attach == nil || attach.GetSessionId() == "" {
		return "", status.Error(codes.InvalidArgument, "first frame must be ConsoleAttach with a session_id")
	}
	return attach.GetSessionId(), nil
}
