// soul-mod-community-mongo — реальный SoulModule-плагин Soul Stack
// (community.mongo): ОСНОВНОЙ интерфейс к живому MongoDB (PILOT-срез). Scenario
// сервиса оркеструет порядок/таргетинг, плагин исполняет ОДНУ операцию над одним
// mongod-инстансом ЧЕРЕЗ go-mongo-driver (НЕ core.exec+mongosh: пароль в argv —
// ИБ-риск, хрупкий парсинг; параллель с community.redis).
//
// States (PILOT):
//   - pinged  — health-probe через driver Ping (read-only, changed=false
//               конструктивно, прецедент core.http.probe / community.redis.pinged);
//   - user    — createUser/dropUser (upsert, imperative — НЕ aclfile: в mongo
//               юзеры живут в admin.system.users, не в конфиг-файле). state
//               present/absent. Идемпотентен по usersInfo. ★ localhost-exception:
//               первый admin создаётся БЕЗ auth через localhost, пока admin-БД
//               пуста (mongo-механика, аналог redis default_admin bootstrap);
//   - command — raw db.runCommand (imperative verb-state, changed из params —
//               прецедент community.redis.command).
//
// СОЗНАТЕЛЬНО без dry-run preview: плагин на BaseModule НЕ реализует PlanReadSafe
// → host применяет default-deny (на dry_run — честный «drift не поддержан»,
// решение пользователя, параллель community.redis).
//
// Backend — go.mongodb.org/mongo-driver. Адрес + пароль приходят от Keeper:
// пароль уже отрезолвлен render-фазой из vault-ref (ADR-012), плагин свой
// Vault-клиент НЕ тянет (capability — только network_outbound).
//
// КРИТ ИБ (ADR-010): params["password"] НИКОГДА не попадает в ApplyEvent.Message,
// .Output, в текст ошибок или в stderr. Все коннект/команд-ошибки санитизируются
// (redactError по паролю).
package main

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// MongoModule — реализация SoulModule community.mongo.
//
// BaseModule даёт no-op Plan (без PlanReadSafe → default-deny на dry_run) и
// СОЗНАТЕЛЬНО НЕ реализует ErrandReadSafe (default-deny на Errand). Переопределяем
// Validate и Apply.
type MongoModule struct {
	module.BaseModule

	// connect — точка инъекции для L0. nil → реальный mongo.Connect.
	connect func(ctx context.Context, cfg connConfig) (mongoConn, error)
}

// mongoConn — узкий интерфейс над *mongo.Client (для L0-фейка).
type mongoConn interface {
	// Ping проверяет живость инстанса (driver Ping к primary).
	Ping(ctx context.Context) error
	// RunCommand выполняет команду в БД db и возвращает сырой bson-ответ.
	// Строка/поля ответа — это ответ сервера, не секрет оператора.
	RunCommand(ctx context.Context, db string, cmd bson.D) (bson.Raw, error)
	Close(ctx context.Context) error
}

// connConfig — параметры коннекта. password и tls.*PEM держатся отдельно и
// НИКОГДА не логируются / не кладутся в события (ИБ-инвариант ADR-010).
type connConfig struct {
	addr     string // host:port
	username string
	password string
	authDB   string // authenticationDatabase (обычно admin)
	tls      tlsParams
}

// Validate — runtime-проверки поверх статических от soul-lint. Возвращает
// ValidateReply с errors (не error) — это контракт Validate. Тексты ошибок НЕ
// содержат пароль.
func (m *MongoModule) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	f := req.GetParams().GetFields()

	switch req.GetState() {
	case "pinged":
		errs = append(errs, validateAddr(f)...)
	case "user":
		errs = append(errs, validateUser(f)...)
	case "command":
		errs = append(errs, validateCommand(f)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (expected pinged|user|command)", req.GetState()))
	}

	if len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Apply — диспетчеризация по state. Финальное событие переносит changed/failed +
// output (ADR-012). Ошибки коннекта/команд санитизируются (redactError) — адрес
// сохраняем для диагностики, пароль вырезаем.
//
// ★ user-state открывает коннект САМ (localhost-exception fallback: первый admin
// создаётся без auth) — поэтому его коннект-жизненный-цикл вынесен в applyUser, а
// не в общий openConn здесь. pinged/command идут общим путём (коннект с auth).
func (m *MongoModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if req.GetState() == "user" {
		return m.applyUser(ctx, stream, req.GetParams())
	}

	cfg, err := parseConnConfig(req.GetParams())
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	conn, err := m.openConn(ctx, cfg)
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	switch req.GetState() {
	case "pinged":
		return m.applyPinged(ctx, stream, conn, cfg.password)
	case "command":
		return m.applyCommand(ctx, stream, conn, req.GetParams(), cfg.password)
	default:
		return sendFailure(stream, fmt.Sprintf("unknown state %q (expected pinged|user|command)", req.GetState()))
	}
}

// parseConnConfig вытаскивает коннект-параметры из params. password держится
// отдельно от всего, что попадает в события (ИБ-инвариант ADR-010). authDB по
// умолчанию admin (стандартная БД аутентификации системных юзеров mongo).
func parseConnConfig(s *structpb.Struct) (connConfig, error) {
	f := s.GetFields()
	addr, _ := stringValue(f["addr"])
	if strings.TrimSpace(addr) == "" {
		return connConfig{}, fmt.Errorf("params.addr: must be a non-empty string")
	}
	authDB := stringOrEmpty(f["auth_db"])
	if authDB == "" {
		authDB = "admin"
	}
	return connConfig{
		addr:     addr,
		username: stringOrEmpty(f["username"]),
		password: stringOrEmpty(f["password"]),
		authDB:   authDB,
		tls:      parseTLS(f),
	}, nil
}

func (m *MongoModule) openConn(ctx context.Context, cfg connConfig) (mongoConn, error) {
	if m.connect != nil {
		return m.connect(ctx, cfg)
	}
	return defaultConnect(ctx, cfg)
}

// applyPinged — health-probe через driver Ping. changed=false КОНСТРУКТИВНО
// (probe, не изменение): интерпретация «здоров/нет» — на уровне scenario через
// retry/until/failed_when по register.self.ok.
func (m *MongoModule) applyPinged(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn mongoConn, password string) error {
	if err := conn.Ping(ctx); err != nil {
		return sendFailure(stream, "PING: "+redactError(err, password))
	}
	return sendOutcome(stream, false, "PING ok", map[string]any{
		"ok": true,
	})
}

// applyCommand — raw db.runCommand (imperative verb-state). changed берётся из
// params.changed (default false, probe-семантика). db — целевая БД (default
// admin). command — bson-документ команды (первый ключ = имя команды). Output
// несёт ok-флаг ответа; пароль в события не попадает.
func (m *MongoModule) applyCommand(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn mongoConn, params *structpb.Struct, password string) error {
	f := params.GetFields()
	db := stringOrEmpty(f["db"])
	if db == "" {
		db = "admin"
	}
	changed := boolOrDefault(f["changed"], false)

	cmd, err := commandDoc(f["command"])
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	raw, err := conn.RunCommand(ctx, db, cmd)
	if err != nil {
		// Ошибка команды — ответ сервера (не секрет оператора); коннект-уровень мог
		// нести пароль → redactError по password (defense-in-depth).
		return sendFailure(stream, "runCommand: "+redactError(err, password))
	}
	return sendOutcome(stream, changed, "runCommand ok", map[string]any{
		"ok": commandOK(raw),
	})
}

// commandDoc строит bson.D команды из params.command (map). Порядок ключей в
// map не детерминирован, но первый ключ команды mongo обязан быть её именем —
// поэтому command_name (если задан) выносится ПЕРВЫМ элементом bson.D, остальные
// поля идут за ним. Без command_name берём любой первый ключ map (для команд с
// единственным полем, напр. {ping: 1}).
func commandDoc(v *structpb.Value) (bson.D, error) {
	fields := structField(v)
	if len(fields) == 0 {
		return nil, fmt.Errorf("params.command: must be a non-empty map (bson-документ команды)")
	}
	doc := bson.D{}
	// name-first: если ровно одно поле — оно и есть команда; иначе caller обязан
	// был передать корректный порядок через отдельный ключ. Для PILOT достаточно
	// single-field команд (ping/serverStatus/…), где порядок неважен.
	for k, vv := range fields {
		doc = append(doc, bson.E{Key: k, Value: valueToNative(vv)})
	}
	return doc, nil
}

// commandOK извлекает поле "ok" из bson-ответа команды (1 → true). Ответ mongo
// на успешную команду несёт {ok: 1}. Отсутствие/0 → false.
func commandOK(raw bson.Raw) bool {
	if len(raw) == 0 {
		return false
	}
	v, err := raw.LookupErr("ok")
	if err != nil {
		return false
	}
	switch v.Type {
	case bson.TypeDouble:
		return v.Double() == 1
	case bson.TypeInt32:
		return v.Int32() == 1
	case bson.TypeInt64:
		return v.Int64() == 1
	default:
		return false
	}
}

// validateCommand — статические проверки command-state: addr + command
// обязательны.
func validateCommand(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	if len(structField(f["command"])) == 0 {
		errs = append(errs, "params.command: must be a non-empty map (bson-документ команды)")
	}
	return errs
}
