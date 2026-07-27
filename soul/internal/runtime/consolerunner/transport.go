package consolerunner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The dedicated console transport (ADR-0074 amendment 2026-07-27, NIM-188).
//
// NIM-142 put the console on the EventStream because that avoided a new RPC.
// The cost showed up exactly where it was predicted to: every FromSoul goes
// through one write mutex and one HTTP/2 flow-control window, so pty output and
// a run's TaskEvent/RunResult take turns. The console had to drop output to
// avoid holding that mutex, and a busy console still added latency to apply.
//
// A session now dials its own bidi stream on the SAME connection, so neither
// side waits on the other. Dropping stops being necessary along with it: with a
// private window, a slow reader applies backpressure down to the pty instead of
// forcing the sender to discard bytes (see [session.enqueue]).
//
// The fallback is not optional. A Soul that reaches a Keeper without the RPC
// must still give its operator a console, so the transport keeps the
// EventStream Sink and settles on one carrier per session before the first
// frame goes out.

// attachAckTimeout bounds the wait for ConsoleAttached. The ack is one round
// trip on an established connection; anything near this budget means the
// Keeper is not answering, and the EventStream carrier is the better bet. Kept
// under the runner's teardown budget (3×KillGrace) so a wait can never outlive
// the shutdown that is trying to end it.
const attachAckTimeout = 3 * time.Second

// Dialer opens a dedicated console stream. Implemented by the Soul's gRPC
// session; nil at construction means this build talks to Keeper the old way.
type Dialer interface {
	ConsoleStream(ctx context.Context) (ConsoleStreamClient, error)
}

// ConsoleStreamClient is the narrow bidi surface the runner needs, declared
// here for the same reason [Sink] is: the package stays free of a gRPC
// dependency and its tests need no live connection.
type ConsoleStreamClient interface {
	Send(*keeperv1.ConsoleFromSoul) error
	Recv() (*keeperv1.ConsoleToSoul, error)
	CloseSend() error
}

// carrier is what a session sends on: the sink, plus the two things only a
// dedicated stream can offer — lossless flow control, and a cut that unblocks a
// sender wedged on a Keeper that stopped reading.
type carrier struct {
	sink     Sink
	lossless bool
	cut      func()
}

// sessionTransport is one session's dedicated stream together with the
// EventStream carrier it falls back to.
//
// Settling happens exactly once, and every send waits for it: a frame sent
// before the verdict could otherwise go out on one carrier while the next goes
// out on the other, and two carriers mean two independent streams with no
// ordering between them.
type sessionTransport struct {
	id       string
	stream   ConsoleStreamClient
	cancel   context.CancelFunc
	fallback Sink
	logger   *slog.Logger

	// ready is closed by settle. Its close is the happens-before edge that
	// publishes `alive`, so readers need no lock.
	ready chan struct{}
	alive bool
	once  sync.Once
}

// dialConsoleStream opens a stream for one session and announces which session
// it belongs to. A dial failure is not an error the caller must handle — it
// returns nil and the session rides the EventStream carrier.
func (r *Runner) dialConsoleStream(id string) *sessionTransport {
	if r.dialer == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := r.dialer.ConsoleStream(ctx)
	if err != nil {
		cancel()
		r.logger.Info("console: dedicated stream unavailable, using EventStream",
			slog.String("session_id", id), slog.Any("error", err))
		return nil
	}

	t := &sessionTransport{
		id:       id,
		stream:   stream,
		cancel:   cancel,
		fallback: r.sink,
		logger:   r.logger,
		ready:    make(chan struct{}),
	}

	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleAttach{
			ConsoleAttach: &keeperv1.ConsoleAttach{SessionId: id},
		},
	}); err != nil {
		t.settle(false, err.Error())
		cancel()
		return nil
	}

	go t.watchdog()
	return t
}

// watchdog settles the transport as unusable if the ack never lands. Without
// it, a Keeper that accepted the stream and then went silent would park the
// session's sender until teardown.
func (t *sessionTransport) watchdog() {
	timer := time.NewTimer(attachAckTimeout)
	defer timer.Stop()
	select {
	case <-t.ready:
	case <-timer.C:
		t.settle(false, "no ConsoleAttached within the ack budget")
	}
}

// settle fixes the carrier for this session. First caller wins.
func (t *sessionTransport) settle(alive bool, why string) {
	t.once.Do(func() {
		t.alive = alive
		if !alive {
			t.logger.Info("console: falling back to the EventStream carrier",
				slog.String("session_id", t.id), slog.String("reason", why))
		}
		close(t.ready)
	})
}

// SendFromSoul satisfies [Sink]. It hands console payloads to the dedicated
// stream once that is settled as usable, and everything else — plus everything
// at all if the stream is not — to the EventStream carrier.
func (t *sessionTransport) SendFromSoul(msg *keeperv1.FromSoul) error {
	frame := consoleFrame(msg)
	if frame == nil {
		return t.fallback.SendFromSoul(msg)
	}
	<-t.ready
	if !t.alive {
		return t.fallback.SendFromSoul(msg)
	}
	return t.stream.Send(frame)
}

// cut force-closes the stream. It is the escape hatch for a sender blocked in
// Send because the Keeper stopped reading: cancelling the RPC is the only thing
// that unblocks gRPC flow control, and a session must never be able to hold up
// the Soul's shutdown.
func (t *sessionTransport) cut() {
	t.settle(false, "session torn down before the transport settled")
	t.cancel()
}

// close ends the stream politely once the session's terminal frame is out.
func (t *sessionTransport) close() {
	_ = t.stream.CloseSend()
	t.cancel()
}

// consoleStreamLoop is the session's downstream reader. It is the only place
// that settles the transport as usable, and it is what turns a broken transport
// into a dead pty — a console never outlives the stream that carries it, which
// is the same invariant the EventStream carrier gets from its own teardown.
//
// Started only after the session is registered, so a stream that breaks
// immediately still finds a session to kill.
func (r *Runner) consoleStreamLoop(t *sessionTransport) {
	for {
		msg, err := t.stream.Recv()
		if err != nil {
			t.settle(false, err.Error())
			if s := r.lookup(t.id); s != nil {
				r.logger.Warn("console: dedicated stream lost, killing the session",
					slog.String("session_id", t.id), slog.Any("error", err))
				s.terminate(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)
			}
			return
		}

		switch p := msg.GetPayload().(type) {
		case *keeperv1.ConsoleToSoul_ConsoleAttached:
			t.settle(true, "")
		case *keeperv1.ConsoleToSoul_ConsoleStdin:
			r.Stdin(p.ConsoleStdin)
		case *keeperv1.ConsoleToSoul_ConsoleResize:
			r.Resize(p.ConsoleResize)
		case *keeperv1.ConsoleToSoul_ConsoleClose:
			r.Close(p.ConsoleClose)
		}
	}
}

// consoleFrame rewraps an upstream console payload for the dedicated stream.
// nil for anything else — the runner only ever sends console payloads, but the
// Sink signature is the wider one, so the fallback stays honest.
func consoleFrame(msg *keeperv1.FromSoul) *keeperv1.ConsoleFromSoul {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromSoul_ConsoleOpened:
		return &keeperv1.ConsoleFromSoul{
			Payload: &keeperv1.ConsoleFromSoul_ConsoleOpened{ConsoleOpened: p.ConsoleOpened},
		}
	case *keeperv1.FromSoul_ConsoleChunk:
		return &keeperv1.ConsoleFromSoul{
			Payload: &keeperv1.ConsoleFromSoul_ConsoleChunk{ConsoleChunk: p.ConsoleChunk},
		}
	case *keeperv1.FromSoul_ConsoleExit:
		return &keeperv1.ConsoleFromSoul{
			Payload: &keeperv1.ConsoleFromSoul_ConsoleExit{ConsoleExit: p.ConsoleExit},
		}
	default:
		return nil
	}
}
