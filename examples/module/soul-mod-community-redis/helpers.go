package main

import (
	"fmt"
	"sort"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// parseConnConfig вытаскивает коннект-параметры из params. password держится
// отдельно от всего, что попадает в события (ИБ-инвариант ADR-010).
func parseConnConfig(s *structpb.Struct) (connConfig, error) {
	f := s.GetFields()
	addr, _ := stringValue(f["addr"])
	if strings.TrimSpace(addr) == "" {
		return connConfig{}, fmt.Errorf("params.addr: must be a non-empty string")
	}
	return connConfig{
		addr:     addr,
		username: firstString(f["username"]),
		password: firstString(f["password"]),
		db:       intOrDefault(f["db"], 0),
	}, nil
}

// validateAddr — общая проверка непустого params.addr (command/config).
func validateAddr(f map[string]*structpb.Value) []string {
	if s, _ := stringValue(f["addr"]); strings.TrimSpace(s) == "" {
		return []string{"params.addr: must be a non-empty string"}
	}
	return nil
}

// redactError вырезает пароль из текста ошибки. go-redis формирует ошибки
// коннекта по полям *redis.Options; на некоторых путях (dial-фейл с
// authentication) текст теоретически может содержать кредлы — fail-safe
// заменяем подстроку пароля на "***". Пустой пароль — no-op.
func redactError(err error, password string) string {
	msg := err.Error()
	if password != "" {
		msg = strings.ReplaceAll(msg, password, "***")
	}
	return msg
}

// sortedKeys — детерминированный порядок применения директив (воспроизводимый
// вывод applied и стабильные L0-asserts).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- structpb helpers ---

func stringValue(v *structpb.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return sv.StringValue, true
	}
	return "", false
}

// firstString — строковое значение или "" (для опциональных полей).
func firstString(v *structpb.Value) string {
	s, _ := stringValue(v)
	return s
}

func stringList(v *structpb.Value) []string {
	if v == nil {
		return nil
	}
	lv, ok := v.GetKind().(*structpb.Value_ListValue)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(lv.ListValue.GetValues()))
	for _, x := range lv.ListValue.GetValues() {
		out = append(out, valueToString(x))
	}
	return out
}

// stringMap — map[string]string из structpb-map. Значения приводятся к строке
// (CONFIG SET принимает строковые значения; число из YAML станет "256" и т.п.).
func stringMap(v *structpb.Value) map[string]string {
	if v == nil {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	fields := sv.StructValue.GetFields()
	out := make(map[string]string, len(fields))
	for k, val := range fields {
		out[k] = valueToString(val)
	}
	return out
}

// nodeSpecs — map[ключ -> вложенный struct] из structpb-map-of-maps (cluster
// nodes). Значение каждого ключа — спецификация ноды ({addr|ip+port}); НЕ
// стрингифицируется (в отличие от stringMap), доступ к полям остаётся типовым.
func nodeSpecs(v *structpb.Value) map[string]map[string]*structpb.Value {
	if v == nil {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	fields := sv.StructValue.GetFields()
	out := make(map[string]map[string]*structpb.Value, len(fields))
	for k, val := range fields {
		inner, ok := val.GetKind().(*structpb.Value_StructValue)
		if !ok {
			out[k] = nil
			continue
		}
		out[k] = inner.StructValue.GetFields()
	}
	return out
}

func boolOrDefault(v *structpb.Value, def bool) bool {
	if v == nil {
		return def
	}
	if bv, ok := v.GetKind().(*structpb.Value_BoolValue); ok {
		return bv.BoolValue
	}
	return def
}

func intOrDefault(v *structpb.Value, def int) int {
	if v == nil {
		return def
	}
	if nv, ok := v.GetKind().(*structpb.Value_NumberValue); ok {
		return int(nv.NumberValue)
	}
	return def
}

// valueToString приводит произвольный structpb.Value к строке. Числа без
// дробной части — без ".000000" (256, не 256.000000): config-директивы и args
// часто числовые в YAML, Redis ждёт строку.
func valueToString(v *structpb.Value) string {
	if v == nil {
		return ""
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return k.StringValue
	case *structpb.Value_NumberValue:
		if k.NumberValue == float64(int64(k.NumberValue)) {
			return fmt.Sprintf("%d", int64(k.NumberValue))
		}
		return fmt.Sprintf("%v", k.NumberValue)
	case *structpb.Value_BoolValue:
		return fmt.Sprintf("%t", k.BoolValue)
	default:
		return ""
	}
}

// --- ApplyEvent helpers ---

func sendOutcome(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, message string, output map[string]any) error {
	out, err := structpb.NewStruct(output)
	if err != nil {
		return fmt.Errorf("build output struct: %w", err)
	}
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Changed: changed, Output: out})
}

func sendFailure(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Failed: true})
}
