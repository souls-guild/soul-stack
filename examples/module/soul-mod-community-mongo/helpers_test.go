package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// secretPass — пароль, который НИКОГДА не должен утечь в события/ошибки/stderr
// (ИБ-инвариант ADR-010). Длинный/уникальный, чтобы поиск подстроки был надёжен.
const secretPass = "vault-resolved-supersecret-9f3a7c1e2b"

// cmdCall — записанный вызов RunCommand (db + документ команды) для проверки
// порядка/наличия команд и того, что пароль не утёк в аргументы.
type cmdCall struct {
	db  string
	cmd bson.D
}

// fakeConn — in-memory mongoConn: пишет каждый вызов, отдаёт скриптованные
// ответы. Позволяет доказать localhost-exception fallback, идемпотентность и
// секрет-инварианты без живого mongod.
type fakeConn struct {
	cfg    connConfig
	calls  []cmdCall
	pinged bool
	closed bool

	pingErr error
	// cmdErrByName — ошибка на команду с данным первым ключом (usersInfo/createUser/…).
	cmdErrByName map[string]error
	// rawByName — сырой ответ на команду с данным первым ключом.
	rawByName map[string]bson.Raw
}

func (f *fakeConn) Ping(_ context.Context) error {
	f.pinged = true
	return f.pingErr
}

func (f *fakeConn) RunCommand(_ context.Context, db string, cmd bson.D) (bson.Raw, error) {
	f.calls = append(f.calls, cmdCall{db: db, cmd: cmd})
	name := ""
	if len(cmd) > 0 {
		name = cmd[0].Key
	}
	if f.cmdErrByName != nil {
		if err, ok := f.cmdErrByName[name]; ok {
			return nil, err
		}
	}
	if f.rawByName != nil {
		if raw, ok := f.rawByName[name]; ok {
			return raw, nil
		}
	}
	// Дефолт — успешный {ok: 1}.
	return okRaw(), nil
}

func (f *fakeConn) Close(_ context.Context) error { f.closed = true; return nil }

// okRaw — bson-ответ {ok: 1} (успешная команда).
func okRaw() bson.Raw {
	b, _ := bson.Marshal(bson.D{{Key: "ok", Value: int32(1)}})
	return b
}

// usersRaw — ответ usersInfo с массивом users указанной длины (n>0 → юзер есть).
func usersRaw(n int) bson.Raw {
	arr := bson.A{}
	for i := 0; i < n; i++ {
		arr = append(arr, bson.D{{Key: "user", Value: "u"}})
	}
	b, _ := bson.Marshal(bson.D{{Key: "users", Value: arr}, {Key: "ok", Value: int32(1)}})
	return b
}

// applyStream — локальный fake grpc-stream (паритет с sdk fakeApplyStream).
type applyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	sent []*pluginv1.ApplyEvent
}

func (s *applyStream) Send(e *pluginv1.ApplyEvent) error { s.sent = append(s.sent, e); return nil }
func (s *applyStream) Context() context.Context          { return context.Background() }

func (s *applyStream) final() *pluginv1.ApplyEvent {
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[len(s.sent)-1]
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// hasCommand — был ли вызов RunCommand с первым ключом name (в любой БД).
func hasCommand(calls []cmdCall, name string) bool {
	for _, c := range calls {
		if len(c.cmd) > 0 && c.cmd[0].Key == name {
			return true
		}
	}
	return false
}

// commandValue — значение первого поля команды name (напр. имя юзера в createUser).
func commandValue(calls []cmdCall, name string) (any, bool) {
	for _, c := range calls {
		if len(c.cmd) > 0 && c.cmd[0].Key == name {
			return c.cmd[0].Value, true
		}
	}
	return nil, false
}

// commandField — значение поля field команды с первым ключом cmdName (напр. pwd в
// createUser-документе). Для проверки, что pwd = пароль СОЗДАВАЕМОГО юзера.
func commandField(calls []cmdCall, cmdName, field string) (any, bool) {
	for _, c := range calls {
		if len(c.cmd) == 0 || c.cmd[0].Key != cmdName {
			continue
		}
		for _, e := range c.cmd {
			if e.Key == field {
				return e.Value, true
			}
		}
	}
	return nil, false
}

// assertEventsNoSecret — ни в одном событии (Message/сериализованный Output) нет
// пароля (ИБ-инвариант ADR-010).
func assertEventsNoSecret(t *testing.T, s *applyStream) {
	t.Helper()
	for _, e := range s.sent {
		if strings.Contains(e.GetMessage(), secretPass) {
			t.Errorf("пароль утёк в Message события: %q", e.GetMessage())
		}
		if e.GetOutput() != nil {
			if strings.Contains(e.GetOutput().String(), secretPass) {
				t.Errorf("пароль утёк в Output события: %q", e.GetOutput().String())
			}
		}
	}
}

// commandCarriesSecretExcept — есть ли пароль в АРГУМЕНТАХ какой-либо команды,
// КРОМЕ createUser (где pwd — легитимная часть контракта mongo, но она уходит на
// провод, а НЕ в события). Для проверки «пароль не течёт в неожиданные места».
func commandCarriesSecretOutsideCreateUser(calls []cmdCall, secret string) bool {
	for _, c := range calls {
		name := ""
		if len(c.cmd) > 0 {
			name = c.cmd[0].Key
		}
		if name == "createUser" {
			continue // pwd тут ожидаем (createUser-контракт)
		}
		for _, e := range c.cmd {
			if s, ok := e.Value.(string); ok && strings.Contains(s, secret) {
				return true
			}
		}
	}
	return false
}
