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

// typesAclUserPerms — каталог с AclUser, несущим pattern на perms (token-shape-
// фильтр Redis-ACL, examples/service/redis/types.yml). pattern проверяется на
// RUNTIME-пути ValidateInput → ResolveInputValues → validateValueAt для каждого
// элемента массива users. Источник pattern — types.yml сервиса redis; здесь
// продублирован 1:1 как канон guard-теста.
const typesAclUserPerms = `types:
  AclUser:
    type: object
    required: [name, perms]
    properties:
      name:
        type: string
        pattern: "^[a-z][a-z0-9_-]*$"
      perms:
        type: string
        pattern: "^(?:(?:(?:on|off|nopass|resetpass|reset|clearselectors|sanitize-payload|skip-sanitize-payload|nosanitize-payload|allkeys|allchannels|allcommands|nocommands)|(?:%R~|%W~|%RW~|~)[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|&[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|[+-](?:@[a-z][a-z0-9-]*|[a-z][a-z0-9-]*(?:\\|[a-z][a-z0-9-]*)?)|[><][A-Za-z0-9:_.@%/+=-]+|[#!][0-9a-fA-F]{64}|\\([^()]*\\))(?: (?:(?:on|off|nopass|resetpass|reset|clearselectors|sanitize-payload|skip-sanitize-payload|nosanitize-payload|allkeys|allchannels|allcommands|nocommands)|(?:%R~|%W~|%RW~|~)[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|&[A-Za-z0-9:_.*?\\[\\]{}\\\\-]+|[+-](?:@[a-z][a-z0-9-]*|[a-z][a-z0-9-]*(?:\\|[a-z][a-z0-9-]*)?)|[><][A-Za-z0-9:_.@%/+=-]+|[#!][0-9a-fA-F]{64}|\\([^()]*\\)))*)?$"
      state:
        type: string
        default: "on"
        enum: [on, off]
`

// TestValidateInput_AclUserPerms_GarbageRejected — token-shape pattern на
// AclUser.perms отшивает мусор/инъекции на RUNTIME-пути ValidateInput. Каждый
// кейс — операторский input.users с одним элементом; perms — не-Redis-ACL-строка.
// Доказывает, что pattern энфорсится на элементах массива $type:AclUser (прод-путь
// резолва $type → ResolveInputValues → validateValueAt), а не только в lint.
func TestValidateInput_AclUserPerms_GarbageRejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	cases := map[string]string{
		"произвольный текст":        "произвольный текст",
		"shell-инъекция через ;":    "~* +@all; rm -rf /",
		"перевод строки":            "~app:* +@read\n~evil:* +@all",
		"неизвестный сигил =":       "~* =foo",
		"pipe вне сабкоманды":       "~* +@all | cat",
		"backtick":                  "~* `whoami`",
		"command substitution":      "~* $(id)",
		"ключевое-слово без сигила": "hello",
	}
	for name, perms := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
				map[string]any{"users": []any{map[string]any{"name": "app", "perms": perms, "state": "on"}}})
			if err == nil {
				t.Fatalf("мусорная perms %q должна быть отклонена pattern-ом, got nil", perms)
			}
			if !errors.Is(err, ErrInputInvalid) {
				t.Fatalf("ожидался ErrInputInvalid (perms не соответствует pattern), got %v", err)
			}
		})
	}
}

// TestValidateInput_AclUserPerms_ValidAccepted — валидные Redis-ACL-строки
// ПРОХОДЯТ pattern: точечные права, широкие (allkeys +@all) и РЕАЛЬНЫЕ системные
// строки из essence (replica/monitoring/sentinel/haproxy, с дефисными сабкомандами
// типа sentinel|is-master-down-by-addr). Замыкает guard — pattern не ложно-режет.
func TestValidateInput_AclUserPerms_ValidAccepted(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioUsersOfType, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	valid := []string{
		"~app:* +@read +@write -@dangerous",
		"~* +@all",
		"allkeys +@all",
		"+psync +replconf +ping", // системный replica
		"-@all +@connection +client +ping +info +config|get +cluster|info +slowlog +latency +memory +select +command|count +command|docs",   // системный monitoring
		"allchannels +multi +slaveof +ping +exec +subscribe +config|rewrite +role +publish +info +client|setname +client|kill +script|kill", // системный sentinel
		"-@all +auth +client|getname +client|id +client|setname +command +hello +ping +role +info +cluster|info",                            // системный haproxy
		"allchannels -@all +auth +sentinel|is-master-down-by-addr +sentinel|get-master-addr-by-name +sentinel|myid",                         // дефисные сабкоманды
		// NB: пустая строка pattern-ом разрешена (бесправный off-юзер), но perms в
		// required:[name,perms] → пустая отвергается required-логикой ДО pattern.
		// Бесправный юзер выражается токеном `off`, не пустой строкой.
		"off",
	}
	for _, perms := range valid {
		err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
			map[string]any{"users": []any{map[string]any{"name": "app", "perms": perms, "state": "on"}}})
		if err != nil {
			t.Fatalf("валидная perms %q должна проходить pattern, got %v", perms, err)
		}
	}
}

// scenarioRequiredTypeField — самостоятельное поле `user: {$type: AclUser}` с
// field-level `required: true`. После $type-резолва узел несёт форму типа
// (object + RequiredProps) И перенесённый overlay-ссылкой Required=true
// (applyRefOverlay, ADR-062). Омит `user` обязан упасть на requireInputValues.
const scenarioRequiredTypeField = `name: create
input:
  user:
    $type: AclUser
    required: true
tasks: []
`

// scenarioOptionalTypeField — тот же $type:AclUser-узел БЕЗ required. Омит
// `user` проходит: required не выставлен, default нет → отсутствие легально.
const scenarioOptionalTypeField = `name: create
input:
  user:
    $type: AclUser
tasks: []
`

// TestValidateInput_TypeRef_RequiredField_Omitted_Rejected — регресс-гард
// backend-энфорсмента required на $type-резолвнутом object-узле (NIM-72):
// омитнутое поле `user` с `required: true` даёт ErrInputInvalid на RUNTIME-пути
// ValidateInput → ResolveInputValues → requireInputValues. Откат энфорсмента
// (чтение s.Required на пострезолвном узле, input_value.go) молча пропустил бы
// создание инкарнации без обязательного поля — этот тест ловит откат.
func TestValidateInput_TypeRef_RequiredField_Omitted_Rejected(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioRequiredTypeField, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{}) // user омитнут
	if err == nil {
		t.Fatal("required $type:AclUser-поле без значения должно быть отклонено, got nil (энфорсмент required снят?)")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("ожидался ErrInputInvalid (required на резолвнутом object-узле), got %v", err)
	}
}

// TestValidateInput_TypeRef_OptionalField_Omitted_OK — парный кейс: тот же
// $type:AclUser-узел БЕЗ required, омитнутый, ПРОХОДИТ. Замыкает гард — энфорсмент
// не ложно-режет опциональные type-поля (required срабатывает ровно от Required=true,
// не от одного лишь наличия RequiredProps резолвнутого типа).
func TestValidateInput_TypeRef_OptionalField_Omitted_OK(t *testing.T) {
	root := writeServiceWithTypes(t, scenarioOptionalTypeField, typesAclUserPerms)
	loader := &dirInputLoader{root: root}

	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{}) // user омитнут — легально для опционального поля
	if err != nil {
		t.Fatalf("опциональное $type:AclUser-поле без значения должно проходить, got %v", err)
	}
}
