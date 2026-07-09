package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// authError — mongo.CommandError с кодом Unauthorized (13), какой возвращает
// mongod на попытку auth к ПУСТОЙ admin-БД (первый admin ещё не создан).
func authError() error {
	return mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "command usersInfo requires authentication"}
}

// dualModule собирает MongoModule, различающий auth-коннект и no-auth-коннект по
// наличию username в cfg: authConn отдаётся при username!="", noAuthConn — при
// пустом. Это моделирует localhost-exception fallback (плагин при auth-fail
// переоткрывает коннект БЕЗ auth). Возвращает cfg последнего коннекта в поле cfg
// каждого fakeConn.
func dualModule(authConn, noAuthConn *fakeConn) *MongoModule {
	return &MongoModule{
		connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
			if cfg.username != "" || cfg.password != "" {
				authConn.cfg = cfg
				return authConn, nil
			}
			noAuthConn.cfg = cfg
			return noAuthConn, nil
		},
	}
}

// TestApplyUser_LocalhostExceptionBootstrap_FirstAdmin — ★★ КЛЮЧЕВОЙ mongo-тест.
// admin-БД пуста: auth-коннект открывается, но usersInfo-проба падает Unauthorized
// (localhost-exception ещё не пройден). Плагин делает fallback на NO-AUTH коннект
// и создаёт первого admin по нему (createUser). Проверяем:
//   - createUser вызван (на no-auth коннекте);
//   - used_localhost/bootstrap_admin = true;
//   - changed=true, present=true;
//   - пароль не утёк в события.
func TestApplyUser_LocalhostExceptionBootstrap_FirstAdmin(t *testing.T) {
	// auth-коннект: usersInfo-проба падает Unauthorized (admin пуста → auth не работает).
	authConn := &fakeConn{cmdErrByName: map[string]error{"usersInfo": authError()}}
	// no-auth коннект (localhost-exception): usersInfo → 0 юзеров (нет), createUser ok.
	noAuthConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}

	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "default_admin",
			"database": "admin",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "root", "db": "admin"}},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успешное создание первого admin, got %+v", fin)
	}
	if !fin.Changed {
		t.Error("ждали changed=true (создан первый admin)")
	}
	if !fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("★ used_localhost=false — ждали true (localhost-exception fallback сработал)")
	}
	if !fin.GetOutput().GetFields()["bootstrap_admin"].GetBoolValue() {
		t.Error("★ bootstrap_admin=false — ждали true")
	}
	// createUser выполнен на NO-AUTH коннекте (localhost-exception), не на auth.
	if !hasCommand(noAuthConn.calls, "createUser") {
		t.Errorf("★ createUser не выполнен на no-auth коннекте, got %v", noAuthConn.calls)
	}
	// Имя созданного юзера.
	if v, ok := commandValue(noAuthConn.calls, "createUser"); !ok || v != "default_admin" {
		t.Errorf("createUser name=%v, ждали default_admin", v)
	}
	assertEventsNoSecret(t, stream)
	if !noAuthConn.closed {
		t.Error("no-auth соединение не закрыто")
	}
}

// TestApplyUser_AuthWorks_NoBootstrap — admin УЖЕ есть: auth-коннект работает
// (usersInfo-проба ok), fallback не нужен. Создаём второго юзера через auth-коннект.
// used_localhost=false.
func TestApplyUser_AuthWorks_NoBootstrap(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		// usersInfo вызывается дважды: проба (openUserConn) и exists-чек. Оба
		// возвращают контекстно осмысленный ответ — для пробы важен УСПЕХ (не ошибка),
		// для exists — что запрашиваемого юзера НЕТ (0). usersRaw(0) годится обоим:
		// проба смотрит только на отсутствие ошибки.
		"usersInfo": usersRaw(0),
	}}
	noAuthConn := &fakeConn{}

	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("★ used_localhost=true — ждали false (auth работает, bootstrap не нужен)")
	}
	// createUser на AUTH коннекте, no-auth не трогается.
	if !hasCommand(authConn.calls, "createUser") {
		t.Errorf("createUser не выполнен на auth-коннекте, got %v", authConn.calls)
	}
	if len(noAuthConn.calls) != 0 {
		t.Errorf("★ no-auth коннект не должен использоваться (auth работает), got %v", noAuthConn.calls)
	}
}

// TestApplyUser_PresentIdempotent_NoOp — юзер уже есть → no-op (changed=false),
// createUser НЕ вызывается (idempotency; смена пароля/ролей — операционный сценарий).
func TestApplyUser_PresentIdempotent_NoOp(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(1), // юзер есть (и проба ok, и exists=true)
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("★ present-idempotent: юзер уже есть → changed=false")
	}
	if hasCommand(authConn.calls, "createUser") {
		t.Error("★ createUser вызван на существующем юзере — нарушена идемпотентность")
	}
}

// TestApplyUser_AbsentDropsExisting — state=absent, юзер есть → dropUser, changed=true.
func TestApplyUser_AbsentDropsExisting(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(1),
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if !fin.Changed {
		t.Error("absent + юзер есть → changed=true (dropUser)")
	}
	if !hasCommand(authConn.calls, "dropUser") {
		t.Errorf("ждали dropUser, got %v", authConn.calls)
	}
}

// TestApplyUser_AbsentIdempotent_NoOp — state=absent, юзера нет → no-op.
func TestApplyUser_AbsentIdempotent_NoOp(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(0),
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "ghost",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("★ absent-idempotent: юзера нет → changed=false")
	}
	if hasCommand(authConn.calls, "dropUser") {
		t.Error("★ dropUser вызван на отсутствующем юзере — нарушена идемпотентность")
	}
}

// TestApplyUser_AbsentDoesNotBootstrap — state=absent + auth падает Unauthorized →
// НЕ делаем no-auth fallback (снятие юзера — не bootstrap-случай). Возвращаем ошибку.
func TestApplyUser_AbsentDoesNotBootstrap(t *testing.T) {
	authConn := &fakeConn{cmdErrByName: map[string]error{"usersInfo": authError()}}
	noAuthConn := &fakeConn{}
	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("★ ждали failed=true (absent не делает localhost-exception fallback), got %+v", fin)
	}
	if len(noAuthConn.calls) != 0 {
		t.Errorf("★ no-auth коннект не должен использоваться на absent-пути, got %v", noAuthConn.calls)
	}
}

// TestApplyUser_NoCredentials_DirectLocalhost — кредлы не заданы → сразу no-auth
// (оператор рассчитывает на localhost-exception). used_localhost=true.
func TestApplyUser_NoCredentials_DirectLocalhost(t *testing.T) {
	noAuthConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	authConn := &fakeConn{}
	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"name":     "default_admin",
			"database": "admin",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "root", "db": "admin"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if !fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("★ used_localhost=false — без кредлов ждали прямой no-auth путь")
	}
	if len(authConn.calls) != 0 {
		t.Errorf("★ auth-коннект не должен использоваться без кредлов, got %v", authConn.calls)
	}
}

// TestApplyUser_PresentRejectsEmptyRoles — present + пустые roles → failed (юзер
// без ролей бессмыслен; проверка в Apply, зависит от state present).
func TestApplyUser_PresentRejectsEmptyRoles(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "norolls",
			"database": "appdb",
			"state":    "present",
		}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на present без roles, got %+v", fin)
	}
}

// TestApplyUser_CreateUserErrorDoesNotLeakPassword — ошибка createUser (текст
// эхает пароль) санитизируется redactError (ИБ-инвариант ADR-010). Пароль легитимно
// уходит в pwd createUser-документа (на провод), но НЕ в события/ошибки.
func TestApplyUser_CreateUserErrorDoesNotLeakPassword(t *testing.T) {
	authConn := &fakeConn{
		rawByName:    map[string]bson.Raw{"usersInfo": usersRaw(0)},
		cmdErrByName: map[string]error{"createUser": errors.New("write error for pwd " + secretPass)},
	}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyUser_PasswordDoesNotLeakOutsideCreateUser — пароль уходит ТОЛЬКО в pwd
// createUser, но НЕ в usersInfo/другие команды на проводе (ИБ, defense-in-depth).
func TestApplyUser_PasswordDoesNotLeakOutsideCreateUser(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	if commandCarriesSecretOutsideCreateUser(authConn.calls, secretPass) {
		t.Error("★ пароль утёк в аргументы команды вне createUser")
	}
}

// TestApplyUser_UserPasswordSeparateFromAdminPassword — ★ разведение паролей: коннект
// под admin-паролем (password), а createUser pwd — из user_password (пароль СОЗДАВАЕМОГО
// юзера). Для operator-юзеров это разные секреты. Проверяем: createUser.pwd == user_password,
// коннект.password == admin-password, ни один не утёк в события.
func TestApplyUser_UserPasswordSeparateFromAdminPassword(t *testing.T) {
	const adminPass = "admin-connect-pass-4b1c"
	const userPass = "new-user-pwd-a7f2"
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":          "127.0.0.1:27017",
			"username":      "default_admin",
			"password":      adminPass,
			"user_password": userPass,
			"name":          "appuser",
			"database":      "appdb",
			"state":         "present",
			"roles":         []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	// Коннект под admin-паролем.
	if authConn.cfg.password != adminPass {
		t.Errorf("★ коннект.password=%q, ждали admin-пароль %q", authConn.cfg.password, adminPass)
	}
	// createUser.pwd — пароль СОЗДАВАЕМОГО юзера (не admin).
	if pwd, ok := commandField(authConn.calls, "createUser", "pwd"); !ok || pwd != userPass {
		t.Errorf("★ createUser.pwd=%v, ждали user_password %q (разведение паролей)", pwd, userPass)
	}
	// Ни admin-пароль, ни user-пароль не в событиях.
	for _, e := range stream.sent {
		if strings.Contains(e.String(), adminPass) || strings.Contains(e.String(), userPass) {
			t.Errorf("★ пароль утёк в событие: %q", e.String())
		}
	}
}
