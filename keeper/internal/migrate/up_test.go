package migrate

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestUp_CancelledMidLadderIsAnError is the NIM-1060 guard: a ctx cancelled
// while steps are still pending must come back as an error. golang-migrate's
// graceful stop ends Up between steps with a nil error, and that nil used to
// reach `keeper init`, which then bootstrapped a partial schema.
//
// Step 2 blocks until the test releases it, so the stop lands with steps 3 and
// 4 pending.
func TestUp_CancelledMidLadderIsAnError(t *testing.T) {
	ladder := fstest.MapFS{
		"1_first.up.sql":  {Data: []byte("step 1")},
		"2_second.up.sql": {Data: []byte("step 2")},
		"3_third.up.sql":  {Data: []byte("step 3")},
		"4_fourth.up.sql": {Data: []byte("step 4")},
	}
	iofsSrc, err := iofs.New(ladder, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	src := &readEndSource{Driver: iofsSrc, end: make(chan struct{})}
	db := &blockingDriver{
		blockAt: 2,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		version: database.NilVersion,
	}
	defer db.unblock()

	m, err := migrate.NewWithInstance("mapfs", src, "blocking", db)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- up(ctx, m) }()

	waitFor(t, "step 2 to start", db.entered)
	// The reader goroutine also polls the stop signal; once it has run off
	// the end of the source it never does again, and the signal is left for
	// the loop that runs the steps.
	waitFor(t, "the source to be read to the end", src.end)

	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for len(m.GracefulStop) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("ctx cancelled, but no stop signal reached golang-migrate")
		}
		time.Sleep(time.Millisecond)
	}
	db.unblock()

	select {
	case err = <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("up did not return after step 2 was released")
	}

	if err == nil {
		t.Fatalf("up returned nil with steps %v applied of 4: an interrupted ladder reads as a finished one", db.ran)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "interrupted at version 2") {
		t.Errorf("err = %q, want it to name version 2, where Up stopped", err)
	}
	if want := []int{1, 2}; !equalInts(db.ran, want) {
		t.Errorf("steps run = %v, want %v: the stop did not land where the test placed it", db.ran, want)
	}
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readEndSource closes end when golang-migrate's reader asks past the last
// step — after that the reader returns without another look at the stop
// signal.
type readEndSource struct {
	source.Driver
	end  chan struct{}
	once sync.Once
}

func (s *readEndSource) Next(version uint) (uint, error) {
	next, err := s.Driver.Next(version)
	if errors.Is(err, os.ErrNotExist) {
		s.once.Do(func() { close(s.end) })
	}
	return next, err
}

// blockingDriver is an in-memory database.Driver whose step blockAt waits for
// release. version, dirty and ran are unsynchronized: Run, SetVersion and
// Version use them on the goroutine that called Up, and the test reads ran
// only after up has returned. Lock and Close, which golang-migrate calls from
// goroutines of its own, must stay stateless.
type blockingDriver struct {
	blockAt int
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	version int
	dirty   bool
	ran     []int
}

func (d *blockingDriver) unblock() { d.once.Do(func() { close(d.release) }) }

func (d *blockingDriver) Open(string) (database.Driver, error) { return d, nil }
func (d *blockingDriver) Close() error                         { return nil }
func (d *blockingDriver) Lock() error                          { return nil }
func (d *blockingDriver) Unlock() error                        { return nil }
func (d *blockingDriver) Drop() error                          { return nil }

func (d *blockingDriver) Run(migration io.Reader) error {
	if _, err := io.ReadAll(migration); err != nil {
		return err
	}
	if d.version == d.blockAt {
		close(d.entered)
		<-d.release
	}
	d.ran = append(d.ran, d.version)
	return nil
}

func (d *blockingDriver) SetVersion(version int, dirty bool) error {
	d.version, d.dirty = version, dirty
	return nil
}

func (d *blockingDriver) Version() (int, bool, error) { return d.version, d.dirty, nil }
