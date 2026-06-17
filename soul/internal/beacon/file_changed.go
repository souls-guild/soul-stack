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

// FileChangedName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const FileChangedName = beaconaddr.FileChanged

// stateFileMissing — sentinel-state для отсутствующего файла. Появление/
// исчезновение файла так же edge-triggered, как смена содержимого: переход
// hash↔"missing" — это смена State, scheduler эмитит Portent.
const stateFileMissing State = "missing"

// FileChanged — core-beacon наблюдения за изменением файла (ADR-030 S1).
// Read-only: SHA-256 содержимого, без записи. State = hex-хеш файла либо
// "missing", если файла нет. Смена хеша (правка/ротация/удаление) → Portent.
//
// Param `path` (string, required) — абсолютный путь к наблюдаемому файлу.
type FileChanged struct{}

// NewFileChanged собирает beacon. Состояния у проверки нет — чистое чтение FS.
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

// hashFile считает SHA-256 файла потоково (io.Copy, без загрузки целиком в
// память — наблюдаемый файл может быть крупным). os.ErrNotExist прокидывается
// наружу для отдельной обработки "missing"-state. Параллель archive.hashFile,
// но тот приватен своему пакету — дублировать 8 строк дешевле кросс-импорта.
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
