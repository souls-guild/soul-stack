// Package consoletest provides an in-memory [console.RecordingStore] for tests
// of anything that opens a console (the audittest pattern).
//
// It fakes only the STORAGE. Tests build the real recorder over it with
// [console.NewRecorder], so the cast encoding, the secret masking and the size
// cap under test are the ones that run in production — and no second
// [console.Recorder] implementation exists anywhere in the tree, which is part
// of how "a console is always recorded" stays true (ADR-0074(g)).
package consoletest

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
)

// ErrStoreDown is what [Store] returns when it is set to fail, standing in for
// an unreachable Postgres.
var ErrStoreDown = errors.New("consoletest: recording store is down")

// Store is an in-memory recording store.
type Store struct {
	mu         sync.Mutex
	recordings map[string]*Recording
	order      []string

	// failBegin refuses to start a recording — the fail-closed case where a
	// console is never opened.
	failBegin bool
	// failAppend refuses to persist the body — the fail-closed case where an
	// open console must be closed.
	failAppend bool
}

// Recording is one captured recording.
type Recording struct {
	Meta   console.RecordingMeta
	Parts  map[int64]string
	Result console.RecordingResult
	Closed bool
}

// NewStore builds an empty store that accepts everything.
func NewStore() *Store {
	return &Store{recordings: make(map[string]*Recording)}
}

// FailBegin makes every [Store.Begin] fail from now on.
func (s *Store) FailBegin(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failBegin = fail
}

// FailAppend makes every [Store.Append] fail from now on.
func (s *Store) FailAppend(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAppend = fail
}

func (s *Store) Begin(_ context.Context, meta console.RecordingMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBegin {
		return ErrStoreDown
	}
	s.recordings[meta.RecordingID] = &Recording{Meta: meta, Parts: make(map[int64]string)}
	s.order = append(s.order, meta.RecordingID)
	return nil
}

func (s *Store) Append(_ context.Context, recordingID string, seq int64, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAppend {
		return ErrStoreDown
	}
	rec, ok := s.recordings[recordingID]
	if !ok {
		return errors.New("consoletest: append to an unknown recording")
	}
	rec.Parts[seq] = body
	return nil
}

func (s *Store) Finish(_ context.Context, recordingID string, res console.RecordingResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recordings[recordingID]
	if !ok {
		return errors.New("consoletest: finish of an unknown recording")
	}
	rec.Result = res
	rec.Closed = true
	return nil
}

// Count returns how many recordings were started.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recordings)
}

// All returns every recording in the order it was started.
func (s *Store) All() []*Recording {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Recording, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.recordings[id])
	}
	return out
}

// Only returns the single recording in the store, or nil when there is not
// exactly one — the shape most tests want.
func (s *Store) Only() *Recording {
	all := s.All()
	if len(all) != 1 {
		return nil
	}
	return all[0]
}

// Get returns one recording by id.
func (s *Store) Get(recordingID string) *Recording {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordings[recordingID]
}

// Body concatenates the parts in order — the cast body as a reader would see
// it, header excluded.
func (r *Recording) Body() string {
	if r == nil {
		return ""
	}
	seqs := make([]int64, 0, len(r.Parts))
	for seq := range r.Parts {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var b strings.Builder
	for _, seq := range seqs {
		b.WriteString(r.Parts[seq])
	}
	return b.String()
}

// Lines returns the cast body split into its event lines.
func (r *Recording) Lines() []string {
	body := strings.TrimSuffix(r.Body(), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}
