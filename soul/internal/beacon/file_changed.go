package beacon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// FileChangedName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const FileChangedName = beaconaddr.FileChanged

// stateFileMissing is the sentinel state for a missing file. File
// appearance/disappearance is edge-triggered just like content change: a
// hash↔"missing" transition is a State change, so the scheduler emits a
// Portent.
const stateFileMissing State = "missing"

// FileChanged is the core-beacon for watching file changes (ADR-030 S1).
// Read-only: SHA-256 of content, no writes. State is the file's hex hash, or
// "missing" if the file is absent. A hash change (edit/rotation/deletion)
// emits a Portent.
//
// Param `path` (string, required) — absolute path of the watched file.
type FileChanged struct{}

// NewFileChanged builds the beacon. The check itself is stateless — a pure FS read.
func NewFileChanged() *FileChanged { return &FileChanged{} }

func (b *FileChanged) Check(_ context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	path, err := util.StringParam(params, "path")
	if err != nil {
		return "", nil, err
	}

	hash, err := hashFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return stateFileMissing, fileData(path, ""), nil
	}
	if err != nil {
		return "", nil, err
	}
	return hash, fileData(path, hash), nil
}

// hashFile streams the file's SHA-256 (io.Copy, no full in-memory load — the
// watched file may be large). os.ErrNotExist propagates for separate
// "missing"-state handling. Parallels archive.hashFile, which is private to
// its own package — duplicating 8 lines is cheaper than a cross-import.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileData(path, hash string) *structpb.Struct {
	fields := map[string]any{"path": path}
	if hash == "" {
		fields["state"] = stateFileMissing
	} else {
		fields["sha256"] = hash
	}
	s, _ := structpb.NewStruct(fields)
	return s
}
