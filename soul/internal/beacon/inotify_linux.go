//go:build linux

package beacon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/structpb"
)

// InotifyName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const InotifyName = beaconaddr.Inotify

const (
	stateInotifyQuiet  State = "quiet"
	stateInotifyEvents State = "events"
)

// inotifyBufferSize is the kernel-event read buffer. Kernel docs recommend
// BUF_LEN >= sizeof(struct inotify_event) + NAME_MAX + 1 ~= 16+256+1.
// 4 KiB fits roughly 16 average events per read — a balance between
// latency and read-syscall count.
const inotifyBufferSize = 4096

// inotifyReadIdle is the delay between read syscalls when the kernel fd is
// empty: read non-blocking → EAGAIN → wait before retrying. Without a pause
// the goroutine CPU-spins when idle. 100 ms balances real-time responsiveness
// against wakeup cost; lower means more syscall load, higher means more
// latency for the first event in a Check window.
const inotifyReadIdle = 100 * time.Millisecond

// InotifyBeacon is the core-beacon for the kernel inotify syscall
// (Linux-only, V5-3, ADR-030 amendment 2026-05-26).
//
// Fold-adapter (variant alpha from the architect verdict): a per-path
// background goroutine reads the inotify fd and accumulates events in a
// buffer; Check on each scheduler tick returns the accumulated event
// "window" plus state quiet/events. Read-only by construction (ADR-030):
// the kernel fd only observes, never writes.
//
// MVP limitations (see docs/module/core/beacon/README.md → core.beacon.inotify):
//   - recursive: false-only (param `recursive: true` is accepted by the
//     grammar, but the current implementation does NOT recursively register
//     subdirectories; deferred until an operator actually asks for it — a
//     likely source of bugs in new code).
//   - throttle is ignored (the field exists in params for forward-compat,
//     but in MVP all events are emitted; throttle is planned as a separate
//     slice).
//
// Singleton semantics: one InotifyBeacon instance serves all Vigils in this
// process (static Registry, like other core-beacons). Per-path state lives
// in the `watches` map. Multiple Vigils on different paths get independent
// kernel fds and independent buffers. See also "Lifecycle" below.
//
// Lifecycle (known MVP trade-off): the scheduler doesn't signal the beacon
// "this Vigil was removed", so the fd for a vanished path stays open until
// process shutdown (the kernel reclaims it). In a long-lived soul daemon
// this is a bounded leak (the set of unique paths is finite). An explicit
// Stop() hook on the Beacon interface is deferred (see V5-3 observations).
type InotifyBeacon struct {
	mu      sync.Mutex
	watches map[string]*inotifyWatch // path → active watch
}

// inotifyWatch is one registered observation of a path. Belongs to exactly
// one Vigil (one InotifyBeacon → many Vigils → many watches).
type inotifyWatch struct {
	fd        int
	wd        int
	path      string
	eventMask uint32

	mu     sync.Mutex // guards events
	events []inotifyEventBuf

	stopCh chan struct{}
	done   chan struct{}
}

// inotifyEventBuf is one event from the kernel fd, normalized Soul-side into
// a stable type string. The raw mask is never exposed (kernel constants
// shouldn't leak into where-CEL Decree).
type inotifyEventBuf struct {
	op   string
	name string
	at   int64
}

// NewInotify builds the beacon. No kernel fds at start — fds are created
// lazily on the first Check for each unique path.
func NewInotify() *InotifyBeacon {
	return &InotifyBeacon{watches: make(map[string]*inotifyWatch)}
}

// Check registers the kernel fd and spawns the read goroutine on the first
// call for a path; subsequent calls collect the events accumulated in the
// window since the previous Check. Empty window → state "quiet", otherwise
// "events". The scheduler emits an edge-triggered Portent on quiet↔events
// transitions.
//
// Params:
//   - `path` (string, required) — absolute path to a file or directory.
//   - `events` (list of string, optional) — event type filter: "created" /
//     "modified" / "deleted" / "moved" / "attrib". Default is all five. An
//     invalid element is ignored (forward-compat).
//   - `recursive` (bool, optional, default false) — MVP accepts only false;
//     true returns a validation error.
//   - `throttle` (string duration, optional) — accepted, ignored in MVP.
func (b *InotifyBeacon) Check(_ context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	path, err := util.StringParam(params, "path")
	if err != nil {
		return "", nil, err
	}
	eventsFilter, err := util.OptStringSliceParam(params, "events")
	if err != nil {
		return "", nil, err
	}
	recursive, err := util.OptBoolParam(params, "recursive")
	if err != nil {
		return "", nil, err
	}
	if recursive {
		return "", nil, fmt.Errorf("param %q: recursive watch не поддерживается в MVP (V5-3)", "recursive")
	}
	mask := resolveInotifyMask(eventsFilter)

	b.mu.Lock()
	w, ok := b.watches[path]
	b.mu.Unlock()

	// Lazy-init / restart on mask change.
	if !ok || w.eventMask != mask {
		nw, err := b.restartWatch(path, mask, w)
		if err != nil {
			return "", nil, err
		}
		w = nw
	}

	// Collect the event window (under self's lock).
	w.mu.Lock()
	flushed := w.events
	w.events = nil
	w.mu.Unlock()

	if len(flushed) == 0 {
		return stateInotifyQuiet, inotifyData(path, nil), nil
	}
	return stateInotifyEvents, inotifyData(path, flushed), nil
}

// restartWatch closes the old watch (if any) and registers a new one.
// Returns the new watch. Uses a separate internal B.mu lock (not held during
// startWatch — the registration syscall can block).
func (b *InotifyBeacon) restartWatch(path string, mask uint32, old *inotifyWatch) (*inotifyWatch, error) {
	if old != nil {
		old.stop()
	}
	w, err := b.startWatch(path, mask)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.watches[path] = w
	b.mu.Unlock()
	return w, nil
}

// startWatch creates an inotify fd, registers a watch on path, and spawns
// the read goroutine. ENOSPC (max_user_watches exhausted) comes from
// inotify_add_watch — converted into an operator-readable error.
func (b *InotifyBeacon) startWatch(path string, mask uint32) (*inotifyWatch, error) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("inotify_init: %w", err)
	}
	wd, err := unix.InotifyAddWatch(fd, path, mask)
	if err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, syscall.ENOSPC) {
			return nil, fmt.Errorf("inotify_add_watch %s: max_user_watches исчерпан (sysctl fs.inotify.max_user_watches)", path)
		}
		return nil, fmt.Errorf("inotify_add_watch %s: %w", path, err)
	}
	w := &inotifyWatch{
		fd:        fd,
		wd:        wd,
		path:      path,
		eventMask: mask,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// stop halts the read goroutine and closes the kernel fd. After stop the
// watch receives no more events; called from restartWatch (mask change) or
// on process shutdown (the kernel cleans up anyway, but defensive cleanup
// helps in tests). Idempotent.
func (w *inotifyWatch) stop() {
	select {
	case <-w.stopCh:
		return // already stopped
	default:
	}
	close(w.stopCh)
	// Closing fd causes EBADF in read → readLoop exits.
	_ = unix.Close(w.fd)
	<-w.done
}

// readLoop runs in the background: reads the inotify fd via unix.Read
// (non-blocking), parses inotify_event structs, and appends them to w.events
// under w.mu. EAGAIN (empty) → short sleep; EBADF (closed fd) or stopCh →
// exit.
func (w *inotifyWatch) readLoop() {
	defer close(w.done)
	buf := make([]byte, inotifyBufferSize)
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				select {
				case <-w.stopCh:
					return
				case <-time.After(inotifyReadIdle):
				}
				continue
			}
			// EBADF / EINTR / closed → exit.
			return
		}
		if n <= 0 {
			continue
		}
		w.parseAndAppend(buf[:n])
	}
}

// parseAndAppend parses a buffer of inotify_events. Each event has a fixed
// header (SizeofInotifyEvent) plus an optional name of length Len (the file
// name for a directory watch, empty for a single-file watch).
func (w *inotifyWatch) parseAndAppend(buf []byte) {
	now := time.Now().Unix()
	var batch []inotifyEventBuf
	for offset := 0; offset+unix.SizeofInotifyEvent <= len(buf); {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		nameLen := int(raw.Len)
		op := mapInotifyMaskToOp(raw.Mask)
		var name string
		if nameLen > 0 {
			end := offset + unix.SizeofInotifyEvent + nameLen
			if end > len(buf) {
				break // corrupt buffer — kernel never splits an event across reads, just in case
			}
			name = strings.TrimRight(string(buf[offset+unix.SizeofInotifyEvent:end]), "\x00")
		}
		if op != "" {
			batch = append(batch, inotifyEventBuf{op: op, name: name, at: now})
		}
		offset += unix.SizeofInotifyEvent + nameLen
	}
	if len(batch) == 0 {
		return
	}
	w.mu.Lock()
	w.events = append(w.events, batch...)
	w.mu.Unlock()
}

// inotifyData builds PortentEvent.data (legacy Struct branch). The typed
// payload is built separately via fillTypedPayload (typed_payload.go).
func inotifyData(path string, events []inotifyEventBuf) *structpb.Struct {
	fields := map[string]any{
		"path":  path,
		"count": len(events),
	}
	if len(events) > 0 {
		list := make([]any, 0, len(events))
		for _, e := range events {
			list = append(list, map[string]any{
				"type": e.op,
				"file": e.name,
				"at":   e.at,
			})
		}
		fields["events"] = list
	}
	s, _ := structpb.NewStruct(fields)
	return s
}

// resolveInotifyMask converts the operator's filter (`events: [...]`) into
// a kernel mask. Empty filter → all 5 supported event types. An unknown
// element is silently ignored (forward-compat: new types get added to the
// grammar, an old implementation just doesn't see them).
func resolveInotifyMask(events []string) uint32 {
	if len(events) == 0 {
		return unix.IN_CREATE | unix.IN_MODIFY | unix.IN_DELETE |
			unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB
	}
	var mask uint32
	for _, e := range events {
		switch e {
		case "created":
			mask |= unix.IN_CREATE
		case "modified":
			mask |= unix.IN_MODIFY
		case "deleted":
			mask |= unix.IN_DELETE
		case "moved":
			mask |= unix.IN_MOVED_FROM | unix.IN_MOVED_TO
		case "attrib":
			mask |= unix.IN_ATTRIB
		}
	}
	return mask
}

// mapInotifyMaskToOp projects a kernel mask into a stable type string. When
// multiple bits are set, the "primary" one wins by priority (created/deleted
// over modified — the typical edit→create→delete pattern on one file).
// System IN_IGNORED/IN_Q_OVERFLOW map to an empty string; parseAndAppend
// skips those (not user-facing events).
func mapInotifyMaskToOp(mask uint32) string {
	switch {
	case mask&unix.IN_CREATE != 0:
		return "created"
	case mask&unix.IN_DELETE != 0, mask&unix.IN_DELETE_SELF != 0:
		return "deleted"
	case mask&unix.IN_MOVED_FROM != 0, mask&unix.IN_MOVED_TO != 0, mask&unix.IN_MOVE_SELF != 0:
		return "moved"
	case mask&unix.IN_MODIFY != 0:
		return "modified"
	case mask&unix.IN_ATTRIB != 0:
		return "attrib"
	}
	return ""
}
