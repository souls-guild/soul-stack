package schema

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeArtifact writes a file that stands in for a built binary: some opaque bytes that
// a loader would map, and nothing else.
func fakeArtifact(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "redis")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

func TestTrailer_RoundTrip(t *testing.T) {
	body := []byte("\x7fELF not really, but opaque to the reader")
	path := fakeArtifact(t, body)
	payload := []byte(`{"kind":"soul_module","protocol_version":1}`)

	if err := WriteTrailerFile(path, payload); err != nil {
		t.Fatalf("WriteTrailerFile: %v", err)
	}
	got, err := ReadTrailerFile(path)
	if err != nil {
		t.Fatalf("ReadTrailerFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round trip: got %q want %q", got, payload)
	}
	// The artifact itself must be untouched: a loader maps the image and ignores
	// what we appended.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.HasPrefix(onDisk, body) {
		t.Fatal("stamping modified the artifact body")
	}
	if want := len(body) + len(payload) + trailerFooterSize; len(onDisk) != want {
		t.Fatalf("artifact size: got %d want %d", len(onDisk), want)
	}
}

func TestTrailer_RestampDoesNotAccumulate(t *testing.T) {
	body := []byte("artifact body")
	path := fakeArtifact(t, body)

	first := []byte(`{"kind":"soul_module","protocol_version":1}`)
	if err := WriteTrailerFile(path, first); err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	second := []byte(`{"kind":"soul_module","modules":[],"protocol_version":1}`)
	if err := WriteTrailerFile(path, second); err != nil {
		t.Fatalf("second stamp: %v", err)
	}

	got, err := ReadTrailerFile(path)
	if err != nil {
		t.Fatalf("ReadTrailerFile: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("re-stamp: got %q want %q", got, second)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if want := len(body) + len(second) + trailerFooterSize; len(onDisk) != want {
		t.Fatalf("trailers accumulated: size %d want %d", len(onDisk), want)
	}
}

func TestTrailer_AbsentFailsClosed(t *testing.T) {
	// An unstamped artifact, a file too short to hold a footer, and an empty file
	// must all be refusals - never an empty schema a caller could mistake for
	// "declares nothing".
	for name, body := range map[string][]byte{
		"unstamped": []byte("a plain artifact with no trailer at all"),
		"too_short": []byte("tiny"),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			path := fakeArtifact(t, body)
			got, err := ReadTrailerFile(path)
			if err == nil {
				t.Fatalf("expected a refusal, got payload %q", got)
			}
			if !errors.Is(err, ErrNoTrailer) {
				t.Fatalf("want ErrNoTrailer, got %v", err)
			}
		})
	}
}

func TestTrailer_CorruptFailsClosed(t *testing.T) {
	payload := []byte(`{"kind":"soul_module","protocol_version":1}`)
	body := []byte("artifact body")

	corrupt := map[string]struct {
		mutate func(stamped []byte) []byte
		want   error
	}{
		"magic_damaged": {
			mutate: func(b []byte) []byte { b[len(b)-1] = 'X'; return b },
			want:   ErrNoTrailer,
		},
		"length_longer_than_file": {
			mutate: func(b []byte) []byte {
				binary.BigEndian.PutUint64(b[len(b)-trailerFooterSize:], uint64(len(b))+1024)
				return b
			},
			want: ErrTrailerMalformed,
		},
		"length_zero": {
			mutate: func(b []byte) []byte {
				binary.BigEndian.PutUint64(b[len(b)-trailerFooterSize:], 0)
				return b
			},
			want: ErrTrailerMalformed,
		},
		"length_over_cap": {
			mutate: func(b []byte) []byte {
				binary.BigEndian.PutUint64(b[len(b)-trailerFooterSize:], MaxPayloadSize+1)
				return b
			},
			want: ErrTrailerMalformed,
		},
		"appended_after_trailer": {
			// Something concatenated onto a stamped artifact moves the magic off
			// the end; a reader must not go hunting for it.
			mutate: func(b []byte) []byte { return append(b, "extra"...) },
			want:   ErrNoTrailer,
		},
	}

	for name, tc := range corrupt {
		t.Run(name, func(t *testing.T) {
			stamped := AppendTrailer(append([]byte(nil), body...), payload)
			path := fakeArtifact(t, tc.mutate(stamped))
			got, err := ReadTrailerFile(path)
			if err == nil {
				t.Fatalf("expected a refusal, got payload %q", got)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestTrailer_TruncatedPayloadFailsClosed(t *testing.T) {
	// The footer promises more payload than the file still holds - the classic
	// half-copied artifact.
	stamped := AppendTrailer([]byte("body"), []byte(`{"kind":"soul_module"}`))
	stamped = append(stamped[:4], stamped[10:]...)
	path := fakeArtifact(t, stamped)
	if _, err := ReadTrailerFile(path); err == nil {
		t.Fatal("expected a truncated payload to be refused")
	}
}

func TestWriteTrailerFile_RejectsEmptyPayload(t *testing.T) {
	path := fakeArtifact(t, []byte("body"))
	if err := WriteTrailerFile(path, nil); err == nil {
		t.Fatal("expected an empty payload to be refused")
	}
}

func TestTrailerStartAt_ReportsBareArtifact(t *testing.T) {
	body := []byte("artifact body")
	stamped := AppendTrailer(append([]byte(nil), body...), []byte("payload"))
	offset, ok, err := TrailerStartAt(bytes.NewReader(stamped), int64(len(stamped)))
	if err != nil {
		t.Fatalf("TrailerStartAt: %v", err)
	}
	if !ok {
		t.Fatal("TrailerStartAt did not find the trailer")
	}
	if offset != int64(len(body)) {
		t.Fatalf("offset: got %d want %d", offset, len(body))
	}

	_, ok, err = TrailerStartAt(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("TrailerStartAt on a bare artifact: %v", err)
	}
	if ok {
		t.Fatal("TrailerStartAt found a trailer in a bare artifact")
	}
}
