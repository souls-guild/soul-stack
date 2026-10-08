package runtime

import (
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// relayConfirmAfter is how long a stream must have stayed up after an event was
// sent on it for the event to count as delivered. A gRPC Send only queues the
// message: one queued just before a drop is lost with the connection, and nothing
// says which. The client keepalive (30s ping, 10s timeout, soul/internal/grpc)
// closes a dead connection within 40s, so a stream that lived this long past the
// send was alive when it was given the event.
const relayConfirmAfter = 60 * time.Second

// relayMaxRefusals is how many streams the Keeper may end refusing an event before
// the relay gives the event up. An event over the Keeper's message size limit
// kills every stream it is sent on; kept forever it would hold the Soul in a
// reconnect loop. A stream that ends for any other reason — a Keeper restarting —
// counts nothing.
const relayMaxRefusals = 3

// OwnedApply is one message of an apply the Soul answers for — what a WardRoster
// entry declares.
type OwnedApply struct {
	ApplyID string
	Passage int32
}

// Relay is the EventSink of an apply that may outlive the stream it arrived on
// (docs/scenario/settings.md §2.2): the Keeper waits soul_timeout for a lost agent,
// and the wait buys nothing unless what the agent finished meanwhile reaches it
// afterwards. A Send never fails the run. With no stream, or once the stream's Send
// fails, events queue in order; [Relay.Attach] hands the next stream every event
// not yet confirmed delivered, the ones a dead stream may have swallowed included.
// A replayed event can therefore arrive twice: the Keeper takes a second RunResult
// for a terminal row as a no-op, and a second TaskEvent rewrites what the first
// wrote and adds one more audit record.
type Relay struct {
	mu      sync.Mutex
	sink    EventSink
	attach  uint64
	entries []relayEntry
	nowFn   func() time.Time
	// last and lastAttach are the stream detached most recently: a refusal is
	// learned from the stream's Recv, often after a failed Send detached it.
	last       EventSink
	lastAttach uint64
}

type relayEntry struct {
	applyID string
	passage int32
	ev      *keeperv1.TaskEvent
	rr      *keeperv1.RunResult
	// sentOn is the attachment the entry was last sent on, 0 for never; sentAt is
	// when; refusals counts the streams the Keeper refused while it was out on them.
	sentOn   uint64
	sentAt   time.Time
	refusals int
}

// NewRelay builds a relay with no stream attached.
func NewRelay() *Relay {
	return &Relay{nowFn: time.Now}
}

// SendTaskEvent queues ev for the Keeper and sends it if a stream is attached.
func (r *Relay) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	r.push(relayEntry{applyID: ev.GetApplyId(), passage: ev.GetPassage(), ev: ev})
	return nil
}

// SendRunResult queues rr for the Keeper and sends it if a stream is attached.
func (r *Relay) SendRunResult(rr *keeperv1.RunResult) error {
	r.push(relayEntry{applyID: rr.GetApplyId(), passage: rr.GetPassage(), rr: rr})
	return nil
}

func (r *Relay) push(e relayEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	r.entries = append(r.entries, e)
	if r.sink != nil {
		r.sendFrom(len(r.entries) - 1)
	}
}

// Attach makes sink the stream events go to and sends it, in order, every event
// not confirmed delivered. The caller attaches only after the stream's WardRoster,
// which declares the applies these events belong to ([Relay.Undelivered]).
func (r *Relay) Attach(sink EventSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sink != nil {
		r.closeAttachment()
	}
	r.attach++
	r.sink = sink
	r.sendFrom(0)
}

// Detach drops sink, if it is still the attached stream.
func (r *Relay) Detach(sink EventSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sink == sink {
		r.closeAttachment()
	}
}

// Refused records that the Keeper ended sink's stream refusing a message of size
// bytes, and detaches it. The TaskEvent of that size the stream carried
// unconfirmed takes a refusal — the events sent after it never reached the Keeper
// and take none — and one refused relayMaxRefusals times is given up. A RunResult
// never is: it carries no output, so it is not what the Keeper refuses, and a step
// whose result is lost holds its barrier until scenario_timeout.
func (r *Relay) Refused(sink EventSink, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sink == sink {
		r.closeAttachment()
	}
	if r.last != sink {
		return
	}
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.ev != nil && e.sentOn == r.lastAttach && e.wireSize() == size {
			e.refusals++
			if e.refusals >= relayMaxRefusals {
				slog.Default().Warn("relay: the Keeper refused every stream an apply event was sent on - dropped",
					slog.String("apply_id", e.applyID), slog.Int("passage", int(e.passage)), slog.Int("refusals", e.refusals))
				continue
			}
		}
		kept = append(kept, e)
	}
	r.entries = kept
	r.last = nil
}

// Settle forgets the earlier messages of an apply the Keeper sent a later one of:
// it sends the next only after it settled the host's previous one — by its result,
// or without it, after which nothing of it counts. Another apply's events are
// untouched: a host can serve several runs.
func (r *Relay) Settle(applyID string, passage int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.applyID == applyID && e.passage < passage {
			continue
		}
		kept = append(kept, e)
	}
	r.entries = kept
}

// Undelivered lists the messages with events not confirmed delivered — each still
// the Soul's for the WardRoster, or the Keeper's sweep would close a row whose
// result is about to arrive.
func (r *Relay) Undelivered() []OwnedApply {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	seen := make(map[OwnedApply]bool)
	var out []OwnedApply
	for _, e := range r.entries {
		o := OwnedApply{ApplyID: e.applyID, Passage: e.passage}
		if o.ApplyID != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

// sendFrom sends every entry from i on that the attached stream has not carried,
// stopping — and detaching — at the first failure. r.mu held.
func (r *Relay) sendFrom(i int) {
	now := r.nowFn()
	for ; i < len(r.entries); i++ {
		e := &r.entries[i]
		if e.sentOn == r.attach {
			continue
		}
		var err error
		if e.ev != nil {
			err = r.sink.SendTaskEvent(e.ev)
		} else {
			err = r.sink.SendRunResult(e.rr)
		}
		if err != nil {
			if _, merr := proto.Marshal(e.wire()); merr != nil {
				// grpc refuses it before the wire and ends the stream with it; no
				// stream can carry it, and kept it would end every one it is replayed on.
				slog.Default().Warn("relay: an apply event cannot be serialized - dropped",
					slog.String("apply_id", e.applyID), slog.Int("passage", int(e.passage)), slog.Any("error", merr))
				r.entries = append(r.entries[:i], r.entries[i+1:]...)
			}
			r.closeAttachment()
			return
		}
		e.sentOn, e.sentAt = r.attach, now
	}
}

// wire is e as the stream carries it.
func (e *relayEntry) wire() *keeperv1.FromSoul {
	if e.ev != nil {
		return &keeperv1.FromSoul{Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: e.ev}}
	}
	return &keeperv1.FromSoul{Payload: &keeperv1.FromSoul_RunResult{RunResult: e.rr}}
}

// wireSize is the size grpc quotes when the Keeper refuses e.
func (e *relayEntry) wireSize() int {
	return proto.Size(e.wire())
}

// closeAttachment ends the attached stream. What it carried long enough before
// the end is delivered; what it did not, waits for the next stream. r.mu held.
func (r *Relay) closeAttachment() {
	r.prune()
	r.last, r.lastAttach = r.sink, r.attach
	r.sink = nil
}

// prune drops the entries the attached stream has carried for relayConfirmAfter.
// r.mu held.
func (r *Relay) prune() {
	if r.sink == nil {
		return
	}
	now := r.nowFn()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.sentOn == r.attach && now.Sub(e.sentAt) >= relayConfirmAfter {
			continue
		}
		kept = append(kept, e)
	}
	r.entries = kept
}
