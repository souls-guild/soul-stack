package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// dirInputLoader — [InputScenarioLoader] поверх РЕАЛЬНОГО каталога снапшота на
// диске. В отличие от fakeInputLoader (in-memory один YAML), здесь нужен
// настоящий LocalDir: $type-резолв читает сиблинг types.yml из снапшота
// (artifact.LoadScenarioManifestResolved → readSnapshotFile по art.LocalDir), а
// не через ReadFile. ReadFile тоже идёт по диску, чтобы main.yml и types.yml
// читались из одного дерева.
type dirInputLoader struct{ root string }

func (l *dirInputLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref, LocalDir: l.root}, nil
}

func (l *dirInputLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return artifact.ReadSnapshotFile(l.root, file)
}

// writeServiceWithTypes материализует на диске минимальный снапшот сервиса:
// scenario/create/main.yml + types.yml в корне. Возвращает root снапшота.
func writeServiceWithTypes(t *testing.T, mainYAML, typesYAML string) string {
	t.Helper()
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatalf("mkdir scenario: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(mainYAML), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if typesYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "types.yml"), []byte(typesYAML), 0o644); err != nil {
			t.Fatalf("write types.yml: %v", err)
		}
	}
	return root
}

// typesAclUser — каталог типов с AclUser: object с required-полем `name`
// (required-список на object-узле) и опц. `read_only`. Форма провалидируется
// ТОЛЬКО после резолва $type на runtime-пути.
const typesAclUser = `types:
  AclUser:
    type: object
    required: [name]
    properties:
      name:
        type: string
      read_only:
        type: boolean
`

// scenarioUsersOfType — input.users = array of $type:AclUser. Узел items несёт
// ссылку на тип; до резолва его Type пуст → ResolveInputValues пропустил бы
// submitted-элемент БЕЗ проверки формы (это и есть дыра MAJOR).
const scenarioUsersOfType = `name: create
input:
  users:
    type: array
    items:
      $type: AclUser
tasks: []
`

// TestValidateInput_TypeRef_NonObjectElement_Rejected — ГЛАВНЫЙ guard MAJOR:
// submitted-элемент массива $type:AclUser, который НЕ object (строка), на
// RUNTIME-пути ValidateInput → ResolveInputValues ОТКЛОНЯЕТСЯ. Доказывает, что
// $type резолвится на загрузке (без резолва узел имел бы пустой Type и значение
// прошло бы молча).
func TestValidateInput_TypeRef_NonObjectElement_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{"not-an-object"}})
	if err == nil {
		t.Fatal("submitted не-object для $type:AclUser должен быть отклонён, got nil (резолв $type не подключён?)")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (value-валидация против резолвнутой type-формы), got %v", err)
	}
}

// TestValidateInput_TypeRef_MissingRequired_Rejected — элемент-object без
// required-поля `name` типа AclUser → отклоняется. Доказывает энфорсинг
// required-полей ВНУТРИ резолвнутого типа (а не только type-match верхнего узла).
func TestValidateInput_TypeRef_MissingRequired_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{map[string]any{"read_only": true}}})
	if err == nil {
		t.Fatal("AclUser без required name должен быть отклонён, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (required name внутри резолвнутого AclUser), got %v", err)
	}
}

// TestValidateInput_TypeRef_ValidElement_OK — корректный AclUser (object с name)
// проходит. Замыкает guard: резолв НЕ ломает валидный ввод, энфорсинг работает в
// обе стороны (отклоняет битое, принимает правильное).
func TestValidateInput_TypeRef_ValidElement_OK(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"users": []any{map[string]any{"name": "alice", "read_only": true}}})
	if err != nil {
		t.Fatalf("валидный AclUser должен проходить после резолва $type: %v", err)
	}
}

// TestValidateInput_TypeRef_StandaloneField_NonObject_Rejected — $type как
// САМОСТОЯТЕЛЬНОЕ поле (не под items): submitted non-object отклоняется. Покрывает
// вторую форму ссылки (`<param>: { $type: T }`), симметрично array-of-type.
func TestValidateInput_TypeRef_StandaloneField_NonObject_Rejected(t *testing.T) {
	const scn = `name: create
input:
  owner:
    $type: AclUser
tasks: []
`
	root := writeServiceWithTypes(t, scn, typesAclUser)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"owner": "not-an-object"})
	if err == nil {
		t.Fatal("самостоятельное $type:AclUser-поле с non-object должно быть отклонено, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid, got %v", err)
	}
}
