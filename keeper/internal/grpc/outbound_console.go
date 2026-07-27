package grpc

import (
	"context"
	"errors"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Keeper -> Soul half of the interactive console contract (NIM-143,
// docs/soul/console.md §2). Pure "pipe" methods over [Outbound.deliver], so
// consoles inherit cluster routing for free: a local stream is written
// directly, otherwise the message goes to the lease holder over Redis pub/sub.
//
// No audit is written here. Session open/close are recorded once by the session
// manager (`console.opened` / `console.closed`), which knows the initiating
// Archon; keystrokes are session recording, a separate slice (NIM-145). Writing
// per-message audit at this level would produce a flood of rows with no
// archon_aid — the same reason [Outbound.SendErrand] stays silent.

// SendConsoleOpen asks a Soul to spawn a pty session.
func (o *Outbound) SendConsoleOpen(ctx context.Context, sid string, msg *keeperv1.ConsoleOpen) error {
	if msg == nil {
		return errors.New("grpc: ConsoleOpen is nil")
	}
	return o.deliver(ctx, sid, &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ConsoleOpen{ConsoleOpen: msg},
	}, "console-open", msg.GetSessionId())
}

// SendConsoleStdin forwards operator keystrokes into the pty master.
//
// This is the one hot path of the four: one message per keystroke. Once the
// session has its own console stream (NIM-188) the burst lands in that
// session's queue and no longer competes with apply dispatch; a Soul still on
// the EventStream transport shares the per-SID outbound queue as before. Either
// way an overflow returns [ErrOutboundQueueFull] and the caller surfaces it to
// the operator rather than growing an unbounded backlog.
func (o *Outbound) SendConsoleStdin(ctx context.Context, sid string, msg *keeperv1.ConsoleStdin) error {
	if msg == nil {
		return errors.New("grpc: ConsoleStdin is nil")
	}
	return o.deliver(ctx, sid, &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ConsoleStdin{ConsoleStdin: msg},
	}, "console-stdin", msg.GetSessionId())
}

// SendConsoleResize applies a new terminal geometry (TIOCSWINSZ → SIGWINCH).
func (o *Outbound) SendConsoleResize(ctx context.Context, sid string, msg *keeperv1.ConsoleResize) error {
	if msg == nil {
		return errors.New("grpc: ConsoleResize is nil")
	}
	return o.deliver(ctx, sid, &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ConsoleResize{ConsoleResize: msg},
	}, "console-resize", msg.GetSessionId())
}

// SendConsoleClose tells a Soul to kill the session's process group.
func (o *Outbound) SendConsoleClose(ctx context.Context, sid string, msg *keeperv1.ConsoleClose) error {
	if msg == nil {
		return errors.New("grpc: ConsoleClose is nil")
	}
	return o.deliver(ctx, sid, &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ConsoleClose{ConsoleClose: msg},
	}, "console-close", msg.GetSessionId())
}
