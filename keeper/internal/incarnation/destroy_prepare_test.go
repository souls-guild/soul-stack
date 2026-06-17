package incarnation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// Compile-time: реальный загрузчик удовлетворяет узкой поверхности pre-check-а.
var _ DestroyScenarioReader = (*artifact.ServiceLoader)(nil)

// fakeDestroyReader — мок DestroyScenarioReader: Load отдаёт пустой артефакт,
// ReadFile симулирует наличие/отсутствие scenario/destroy/main.yml или I/O-фейл.
type fakeDestroyReader struct {
	loadErr error

	hasScenario bool  // true → ReadFile(destroy main) отдаёт content
	readErr     error // не-nil → ReadFile возвращает эту ошибку (приоритет над hasScenario)
	loadCalls   int
	readCalls   int
	readFile    string // последний запрошенный путь (для проверки, что читался именно destroy main)
}

func (f *fakeDestroyReader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	f.loadCalls++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{Ref: ref}, nil
}

func (f *fakeDestroyReader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	f.readCalls++
	f.readFile = file
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.hasScenario {
		return []byte("on: keeper\ntasks: []\n"), nil
	}
	// Симулируем loader-обёртку над os.ReadFile отсутствующего файла.
	return nil, fmt.Errorf("artifact: чтение main.yml: %w", os.ErrNotExist)
}

func destroyInc() *Incarnation {
	return &Incarnation{Name: "redis-prod", Service: "redis", ServiceVersion: "v1", StateSchemaVersion: 1}
}

func TestPrepareDestroy_ScenarioPresent(t *testing.T) {
	reader := &fakeDestroyReader{hasScenario: true}
	if _, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), false); err != nil {
		t.Fatalf("PrepareDestroy: %v", err)
	}
	if reader.loadCalls != 1 || reader.readCalls != 1 {
		t.Errorf("loadCalls=%d readCalls=%d, want 1/1", reader.loadCalls, reader.readCalls)
	}
	if reader.readFile != destroyScenarioMainFile {
		t.Errorf("read path = %q, want %q", reader.readFile, destroyScenarioMainFile)
	}
}

func TestPrepareDestroy_MissingScenario_NoForce(t *testing.T) {
	reader := &fakeDestroyReader{hasScenario: false}
	_, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), false)
	if !errors.Is(err, ErrDestroyScenarioMissing) {
		t.Fatalf("err = %v, want ErrDestroyScenarioMissing", err)
	}
}

func TestPrepareDestroy_MissingScenario_Force(t *testing.T) {
	// force=true → отсутствие teardown-сценария НЕ блокирует destroy.
	reader := &fakeDestroyReader{hasScenario: false}
	if _, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), true); err != nil {
		t.Fatalf("PrepareDestroy force: %v", err)
	}
}

func TestPrepareDestroy_PresentScenario_Force(t *testing.T) {
	// force=true при наличии scenario тоже ok (force не зависит от наличия).
	reader := &fakeDestroyReader{hasScenario: true}
	if _, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), true); err != nil {
		t.Fatalf("PrepareDestroy force+scenario: %v", err)
	}
}

func TestPrepareDestroy_ServiceNotRegistered(t *testing.T) {
	reader := &fakeDestroyReader{hasScenario: true}
	_, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: false}, reader, destroyInc(), false)
	if !errors.Is(err, ErrServiceNotRegistered) {
		t.Fatalf("err = %v, want ErrServiceNotRegistered", err)
	}
	if reader.loadCalls != 0 {
		t.Errorf("loadCalls = %d, want 0 (resolve fail must short-circuit)", reader.loadCalls)
	}
}

func TestPrepareDestroy_LoadFailed(t *testing.T) {
	reader := &fakeDestroyReader{loadErr: errors.New("git: ref not found")}
	_, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), false)
	if !errors.Is(err, ErrLoadTargetSnapshot) {
		t.Fatalf("err = %v, want ErrLoadTargetSnapshot", err)
	}
}

func TestPrepareDestroy_ReadFileError(t *testing.T) {
	// I/O-ошибка чтения (НЕ os.ErrNotExist) прокидывается как ErrLoadTargetSnapshot,
	// не маскируется под «нет сценария».
	reader := &fakeDestroyReader{readErr: errors.New("permission denied")}
	_, err := PrepareDestroy(context.Background(), fakePrepResolver{ok: true}, reader, destroyInc(), false)
	if !errors.Is(err, ErrLoadTargetSnapshot) {
		t.Fatalf("err = %v, want ErrLoadTargetSnapshot (I/O fail, not missing)", err)
	}
}
