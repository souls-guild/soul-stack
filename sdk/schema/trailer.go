package schema

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The schema trailer: the document, appended after the executable.
//
// Keeper must read an artifact's schema at `plugin.allow` WITHOUT executing it — at
// that moment the binary is not yet approved and running it would be the very thing
// approval is meant to gate. So the document travels inside the artifact rather than
// beside it, and it has to be readable without understanding the executable.
//
// A trailer is the cheapest form that satisfies both: ELF, Mach-O and PE loaders all
// ignore bytes past the end of the image, and a reader finds the payload by seeking
// from the end — no object-file parsing, no format-specific section logic, one code
// path for every platform.
//
// Layout, at the very end of the file:
//
//	<payload> <uint64 big-endian payload length> <16-byte magic>
//
// The length sits between payload and magic so a reader can take a fixed-size footer
// in one read, then seek back exactly len bytes. The magic is last so the check is a
// single comparison at a known offset.
//
// Readers FAIL CLOSED. A missing or malformed trailer is an error, never a fallback to
// a sibling file and never an empty schema: an artifact whose disclosure cannot be read
// is an artifact whose disclosure has not been approved.

// TrailerMagic marks the end of a stamped artifact. The trailing `1` versions the
// trailer format itself — a format change makes it `SOULSTACKSCHEMA2`, and old readers
// stop recognizing new artifacts instead of misreading them.
const TrailerMagic = "SOULSTACKSCHEMA1"

// trailerFooterSize is the fixed tail: 8 bytes of length plus the magic.
const trailerFooterSize = 8 + len(TrailerMagic)

// MaxPayloadSize caps a trailer payload. A stamped document is a few tens of KiB at
// most; the cap turns a corrupt length field into an error rather than an allocation
// the size of whatever the field happened to contain.
const MaxPayloadSize = 16 << 20

var (
	// ErrNoTrailer — the file does not end with [TrailerMagic]: it was never
	// stamped, or something was appended after the trailer.
	ErrNoTrailer = errors.New("schema: artifact carries no schema trailer")
	// ErrTrailerMalformed — the magic is there but the length does not describe a
	// payload that fits in the file.
	ErrTrailerMalformed = errors.New("schema: schema trailer is malformed")
)

// ReadTrailerAt returns the raw payload of the trailer at the end of a file of size
// bytes. It does not parse or validate the payload.
func ReadTrailerAt(r io.ReaderAt, size int64) ([]byte, error) {
	if size < int64(trailerFooterSize) {
		return nil, ErrNoTrailer
	}
	footer := make([]byte, trailerFooterSize)
	if _, err := r.ReadAt(footer, size-int64(trailerFooterSize)); err != nil {
		return nil, fmt.Errorf("schema: read trailer footer: %w", err)
	}
	if string(footer[8:]) != TrailerMagic {
		return nil, ErrNoTrailer
	}
	length := binary.BigEndian.Uint64(footer[:8])
	if length == 0 {
		return nil, fmt.Errorf("%w: zero-length payload", ErrTrailerMalformed)
	}
	if length > MaxPayloadSize {
		return nil, fmt.Errorf("%w: payload length %d exceeds the %d-byte cap", ErrTrailerMalformed, length, MaxPayloadSize)
	}
	start := size - int64(trailerFooterSize) - int64(length)
	if start < 0 {
		return nil, fmt.Errorf("%w: payload length %d does not fit in a %d-byte file", ErrTrailerMalformed, length, size)
	}
	payload := make([]byte, length)
	if _, err := r.ReadAt(payload, start); err != nil {
		return nil, fmt.Errorf("%w: read payload: %v", ErrTrailerMalformed, err)
	}
	return payload, nil
}

// ReadTrailerFile returns the raw trailer payload of the artifact at path.
func ReadTrailerFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("schema: open artifact %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("schema: stat artifact %q: %w", path, err)
	}
	payload, err := ReadTrailerAt(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return payload, nil
}

// TrailerStartAt returns the offset at which the trailer begins, so a caller can
// truncate the file back to the bare artifact. ok is false when there is no trailer.
func TrailerStartAt(r io.ReaderAt, size int64) (offset int64, ok bool, err error) {
	if size < int64(trailerFooterSize) {
		return 0, false, nil
	}
	footer := make([]byte, trailerFooterSize)
	if _, err := r.ReadAt(footer, size-int64(trailerFooterSize)); err != nil {
		return 0, false, fmt.Errorf("schema: read trailer footer: %w", err)
	}
	if string(footer[8:]) != TrailerMagic {
		return 0, false, nil
	}
	length := binary.BigEndian.Uint64(footer[:8])
	if length > MaxPayloadSize {
		return 0, false, fmt.Errorf("%w: payload length %d exceeds the %d-byte cap", ErrTrailerMalformed, length, MaxPayloadSize)
	}
	start := size - int64(trailerFooterSize) - int64(length)
	if start < 0 {
		return 0, false, fmt.Errorf("%w: payload length %d does not fit in a %d-byte file", ErrTrailerMalformed, length, size)
	}
	return start, true, nil
}

// AppendTrailer appends the footer for payload to dst. Exposed for callers that build
// an artifact in memory; [WriteTrailerFile] is the usual entry point.
func AppendTrailer(dst, payload []byte) []byte {
	dst = append(dst, payload...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(payload)))
	dst = append(dst, n[:]...)
	return append(dst, TrailerMagic...)
}

// WriteTrailerFile stamps payload into the artifact at path, replacing any trailer
// already there. Re-stamping is therefore idempotent in size as well as in content —
// trailers do not accumulate.
func WriteTrailerFile(path string, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("schema: refusing to stamp an empty payload")
	}
	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("schema: payload of %d bytes exceeds the %d-byte cap", len(payload), MaxPayloadSize)
	}
	f, err := openForStamping(path)
	if err != nil {
		return fmt.Errorf("schema: open artifact %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("schema: stat artifact %q: %w", path, err)
	}
	base := st.Size()
	if start, ok, err := TrailerStartAt(f, st.Size()); err != nil {
		return err
	} else if ok {
		base = start
	}
	if err := f.Truncate(base); err != nil {
		return fmt.Errorf("schema: truncate artifact %q: %w", path, err)
	}
	if _, err := f.WriteAt(AppendTrailer(nil, payload), base); err != nil {
		return fmt.Errorf("schema: write trailer to %q: %w", path, err)
	}
	return f.Close()
}

// openForStamping opens the artifact for writing, waiting out ETXTBSY.
//
// Stamping runs immediately after `go build` and immediately after the artifact's own
// `schema` subcommand, and on Linux a just-executed image can stay busy for a moment
// past the exit of the process that ran it. Failing there would make `make build` flaky
// for reasons that have nothing to do with the artifact, so the open is retried for a
// short, bounded window and every other error is returned at once.
func openForStamping(path string) (*os.File, error) {
	const (
		attempts = 40
		delay    = 50 * time.Millisecond
	)
	var err error
	for i := range attempts {
		var f *os.File
		if f, err = os.OpenFile(path, os.O_RDWR, 0); err == nil {
			return f, nil
		}
		if !isTextFileBusy(err) {
			return nil, err
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, err
}
