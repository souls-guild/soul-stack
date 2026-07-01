// user-state плагина community.mongo — createUser/dropUser (upsert) над живым
// mongod ЦЕЛИКОМ через go-mongo-driver. Юзеры MongoDB живут в admin.system.users
// (imperative), НЕ в конфиг-файле (в отличие от redis users.acl) — поэтому это
// verb-state, а не рендер файла.
//
// ★★ LOCALHOST-EXCEPTION BOOTSTRAP (mongo-механика, аналог redis default_admin):
// mongod с security.authorization: enabled разрешает коннект БЕЗ auth ТОЛЬКО через
// loopback (localhost) и ТОЛЬКО пока в admin-БД нет ни одного юзера. Первый admin
// (default_admin) создаётся именно так: коннект с auth ещё невозможен (юзера нет),
// поэтому плагин делает fallback на no-auth localhost-коннект. Как только первый
// юзер создан, exception закрывается — дальнейшие коннекты идут с auth.
//
// Механика fallback (внутри плагина, не в render — render передаёт addr+username+
// password, плагин решает auth-путь ПО ФАКТУ live-состояния, параллель с redis-
// плагином, решающим по INFO/CONFIG GET):
//   1. present + заданы username/password → пробуем коннект С AUTH;
//   2. коннект/usersInfo падает auth-ошибкой (Unauthorized/AuthenticationFailed) →
//      это ожидаемо для ПЕРВОГО admin (admin-БД пуста, auth ещё нет) → fallback на
//      NO-AUTH коннект (localhost-exception отработает, если addr — loopback);
//   3. createUser первого admin проходит по no-auth localhost-коннекту.
// Для НЕ-первых юзеров (admin уже есть) auth-коннект успешен → fallback не нужен.
//
// Идемпотентность: usersInfo(name) до операции. present + юзер есть → no-op
// (changed=false; смена пароля/ролей — day-2 update, вне PILOT). present + нет →
// createUser (changed=true). absent + есть → dropUser (changed=true). absent +
// нет → no-op.
//
// КРИТ ИБ (ADR-010): params.password НИКОГДА не попадает в события/ошибки. Пароль
// уходит ТОЛЬКО в createUser-документ (pwd) и в коннект-кредлы; ошибки
// санитизируются redactError.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// mongoRole — одна роль юзера ({role, db}). db пустой → роль в БД самого юзера
// (createUser выполняется в контексте database, роль без db наследует его).
type mongoRole struct {
	role string
	db   string
}

// applyUser — createUser/dropUser с localhost-exception bootstrap. Открывает
// коннект САМ (с auth → fallback no-auth для первого admin), поэтому не идёт через
// общий openConn Apply.
func (m *MongoModule) applyUser(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	name := stringOrEmpty(f["name"])
	database := stringOrEmpty(f["database"])
	if database == "" {
		database = "admin"
	}
	state := stringOrEmpty(f["state"])
	if state == "" {
		state = "present"
	}
	// pwd СОЗДАВАЕМОГО юзера — отдельно от коннект-пароля (admin). user_password задан →
	// он; иначе fallback на password (bootstrap-случай, где admin создаёт САМ СЕБЯ под тем
	// же паролем, что и коннект). Разведение нужно для operator-юзеров: коннект под admin-
	// паролем, а createUser — с паролем нового юзера.
	newUserPwd := stringOrEmpty(f["user_password"])
	if newUserPwd == "" {
		newUserPwd = stringOrEmpty(f["password"])
	}

	cfg, err := parseConnConfig(params)
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	// Коннект с localhost-exception fallback: present-путь допускает no-auth
	// bootstrap первого admin. absent-путь fallback НЕ делает (снятие юзера
	// требует прав — если auth не проходит, это не bootstrap-случай).
	conn, usedLocalhost, err := m.openUserConn(ctx, cfg, state == "present")
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	exists, err := userExists(ctx, conn, database, name)
	if err != nil {
		return sendFailure(stream, "usersInfo: "+redactError(err, newUserPwd, cfg.password))
	}

	switch state {
	case "present":
		if exists {
			// Юзер уже есть — no-op (смена пароля/ролей — day-2 update, вне PILOT).
			return sendOutcome(stream, false, fmt.Sprintf("user %q already present", name), map[string]any{
				"present":         true,
				"changed":         false,
				"used_localhost":  usedLocalhost,
				"bootstrap_admin": usedLocalhost,
			})
		}
		roles, err := parseRoles(f["roles"], database)
		if err != nil {
			return sendFailure(stream, err.Error())
		}
		if err := createUser(ctx, conn, database, name, newUserPwd, roles); err != nil {
			return sendFailure(stream, "createUser: "+redactError(err, newUserPwd, cfg.password))
		}
		return sendOutcome(stream, true, fmt.Sprintf("user %q created", name), map[string]any{
			"present":         true,
			"changed":         true,
			"used_localhost":  usedLocalhost,
			"bootstrap_admin": usedLocalhost,
		})
	case "absent":
		if !exists {
			return sendOutcome(stream, false, fmt.Sprintf("user %q already absent", name), map[string]any{
				"present": false,
				"changed": false,
			})
		}
		if err := dropUser(ctx, conn, database, name); err != nil {
			return sendFailure(stream, "dropUser: "+redactError(err, newUserPwd, cfg.password))
		}
		return sendOutcome(stream, true, fmt.Sprintf("user %q dropped", name), map[string]any{
			"present": false,
			"changed": true,
		})
	default:
		return sendFailure(stream, fmt.Sprintf("params.state: unknown %q (expected present|absent)", state))
	}
}

// openUserConn открывает коннект для user-state с localhost-exception fallback.
// Возвращает (conn, usedLocalhost, err): usedLocalhost=true когда сработал no-auth
// bootstrap-путь (первый admin). Алгоритм:
//   - auth заданы → пробуем коннект С AUTH + дешёвый usersInfo-ping (проверка, что
//     auth реально работает, а не только TCP-коннект);
//   - allowBootstrap И auth-коннект падает auth-ошибкой → fallback на NO-AUTH
//     коннект (localhost-exception отработает на loopback + пустой admin-БД);
//   - auth не заданы → сразу no-auth коннект (localhost-exception ожидаема).
func (m *MongoModule) openUserConn(ctx context.Context, cfg connConfig, allowBootstrap bool) (mongoConn, bool, error) {
	// Нет кредлов → сразу no-auth (оператор рассчитывает на localhost-exception).
	if cfg.username == "" && cfg.password == "" {
		conn, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
		return conn, true, err
	}

	authConn, authErr := m.openConn(ctx, cfg)
	if authErr == nil {
		// TCP-коннект открылся, но authorization enabled может отвергнуть auth
		// лениво (при первой команде). Дешёвый пробный usersInfo подтверждает, что
		// auth реально работает; auth-ошибка → fallback (bootstrap-случай).
		if _, probeErr := authConn.RunCommand(ctx, cfg.authDB, bson.D{{Key: "usersInfo", Value: 1}}); probeErr == nil {
			return authConn, false, nil
		} else if allowBootstrap && isAuthError(probeErr) {
			_ = authConn.Close(ctx)
			noAuth, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
			return noAuth, true, err
		} else {
			// Не auth-ошибка (или bootstrap запрещён) — коннект валиден, отдаём как есть
			// (последующая операция вернёт честную ошибку).
			return authConn, false, nil
		}
	}

	// TCP/auth-коннект не открылся. Если это auth-ошибка и bootstrap разрешён —
	// пробуем no-auth localhost-путь.
	if allowBootstrap && isAuthError(authErr) {
		noAuth, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
		return noAuth, true, err
	}
	return nil, false, authErr
}

// isAuthError распознаёт ошибку авторизации mongo (для localhost-exception
// fallback). mongo возвращает codeName Unauthorized (13) / AuthenticationFailed
// (18) на попытку auth к пустой admin-БД или с неверными кредлами. Проверяем
// типизированный mongo.CommandError по коду, с fallback на текст (для не-command
// путей коннекта).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		// 13 Unauthorized, 18 AuthenticationFailed.
		if cmdErr.Code == 13 || cmdErr.Code == 18 {
			return true
		}
		name := cmdErr.Name
		if name == "Unauthorized" || name == "AuthenticationFailed" {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "requires authentication") ||
		strings.Contains(msg, "not authorized")
}

// userExists проверяет наличие юзера через usersInfo. Возвращает true, если в
// ответе непустой массив users.
func userExists(ctx context.Context, conn mongoConn, db, name string) (bool, error) {
	raw, err := conn.RunCommand(ctx, db, bson.D{{Key: "usersInfo", Value: name}})
	if err != nil {
		return false, err
	}
	users, lookupErr := raw.LookupErr("users")
	if lookupErr != nil {
		return false, nil
	}
	arr, ok := users.ArrayOK()
	if !ok {
		return false, nil
	}
	vals, err := arr.Values()
	if err != nil {
		return false, nil
	}
	return len(vals) > 0, nil
}

// createUser выполняет команду createUser с ролями и паролем. pwd уходит в
// bson-документ (createUser-контракт mongo); в события/логи он не попадает.
func createUser(ctx context.Context, conn mongoConn, db, name, pwd string, roles []mongoRole) error {
	cmd := bson.D{
		{Key: "createUser", Value: name},
		{Key: "pwd", Value: pwd},
		{Key: "roles", Value: rolesToBSON(roles)},
	}
	_, err := conn.RunCommand(ctx, db, cmd)
	return err
}

// dropUser выполняет команду dropUser.
func dropUser(ctx context.Context, conn mongoConn, db, name string) error {
	_, err := conn.RunCommand(ctx, db, bson.D{{Key: "dropUser", Value: name}})
	return err
}

// rolesToBSON конвертирует роли в bson-массив createUser-контракта: каждая роль —
// {role, db}. Роль без db наследует БД юзера (createUser-контекст), но mongo
// требует явный db в документе роли → подставляем БД юзера при пустом db.
func rolesToBSON(roles []mongoRole) bson.A {
	arr := bson.A{}
	for _, r := range roles {
		arr = append(arr, bson.D{
			{Key: "role", Value: r.role},
			{Key: "db", Value: r.db},
		})
	}
	return arr
}

// parseRoles разбирает params.roles — массив {role, db}. Пустой db наследует БД
// юзера (userDB). Пустой массив на present → ошибка (юзер без ролей бессмыслен).
func parseRoles(v *structpb.Value, userDB string) ([]mongoRole, error) {
	items := listField(v)
	if len(items) == 0 {
		return nil, errors.New("params.roles: must be a non-empty list of {role, db} for present")
	}
	out := make([]mongoRole, 0, len(items))
	for _, it := range items {
		rf := structField(it)
		if rf == nil {
			return nil, fmt.Errorf("params.roles: each item must be an object {role, db}")
		}
		role := stringOrEmpty(rf["role"])
		if strings.TrimSpace(role) == "" {
			return nil, fmt.Errorf("params.roles: each item requires a non-empty role")
		}
		db := stringOrEmpty(rf["db"])
		if db == "" {
			db = userDB
		}
		out = append(out, mongoRole{role: role, db: db})
	}
	return out, nil
}

// validateUser — статические проверки user-state: addr + name обязательны, state
// (если задан) ∈ {present, absent}. roles/password проверяются в Apply (зависят
// от state present).
func validateUser(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	errs = append(errs, requireString(f, "name")...)
	if s := stringOrEmpty(f["state"]); s != "" && s != "present" && s != "absent" {
		errs = append(errs, fmt.Sprintf("params.state: unknown %q (expected present|absent)", s))
	}
	return errs
}
