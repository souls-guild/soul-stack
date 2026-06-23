// sentinel-state плагина community.redis — горячая реконсиляция конфигурации
// Redis Sentinel ЦЕЛИКОМ через go-redis: SENTINEL MASTERS/MASTER (diff) →
// SENTINEL MONITOR/REMOVE+MONITOR (монитор) → SENTINEL SET (per-master) →
// SENTINEL CONFIG GET/SET (globals). НИКАКОГО redis-cli/shell — capability
// плагина остаётся только network_outbound.
//
// Алгоритм перенесён 1:1 из Ansible library/redis_sentinel_update.py
// (classify_config / compute_monitor_action / compute_set_updates): top-level
// CONFIG в режиме Sentinel НЕ поддерживается, поэтому глобальные параметры идут
// через SENTINEL CONFIG SET, а параметры master-а — через SENTINEL SET.
//
// Идемпотентен: монитор не трогается, если master уже на нужном адресе; SET/
// CONFIG SET применяются только при реальном отличии от текущего.
//
// КРИТ ИБ (ADR-010): auth_pass / sentinel-pass НИКОГДА не попадают в
// ApplyEvent.Message/.Output/ошибки. Output несёт только имена применённых
// действий, не их секретные значения.
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// defaultMasterName — имя monitored master по умолчанию (как в роли: mymaster).
const defaultMasterName = "mymaster"

// sentinelGlobalParams — глобальные параметры Sentinel, принимаемые SENTINEL
// CONFIG SET (одно слово после "sentinel "). Перенос SENTINEL_GLOBAL_PARAMS.
var sentinelGlobalParams = map[string]bool{
	"announce-ip":        true,
	"announce-port":      true,
	"resolve-hostnames":  true,
	"announce-hostnames": true,
	"sentinel-user":      true,
	"sentinel-pass":      true,
}

// secretGlobalParams — глобальные параметры-секреты: SENTINEL CONFIG GET
// возвращает их пустыми, diff невозможен. В create-срезе rotate не делаем, поэтому
// они просто не применяются как globals (sentinel-pass задаётся через монитор
// при создании, как auth-pass). Перенос SECRET_GLOBAL_PARAMS.
var secretGlobalParams = map[string]bool{"sentinel-pass": true}

// validateSentinel — статические проверки sentinel-params (тексты без секретов).
func validateSentinel(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	if mon := nodeSpec(f["monitor"]); len(mon) > 0 {
		if strings.TrimSpace(stringOrEmpty(mon["ip"])) == "" {
			errs = append(errs, "params.monitor.ip: must be a non-empty string")
		}
		if intOrDefault(mon["port"], 0) <= 0 {
			errs = append(errs, "params.monitor.port: must be a positive integer")
		}
		// quorum опционален (default 1 в reconcileMonitor), но ЕСЛИ задан — должен
		// быть >=1: SENTINEL MONITOR ... 0 отвергается Redis (симметрично port>0).
		if q := mon["quorum"]; q != nil && intOrDefault(q, 1) < 1 {
			errs = append(errs, "params.monitor.quorum: must be a positive integer (>= 1)")
		}
	}
	return errs
}

// applySentinel реконсилирует один Sentinel-инстанс к желаемому состоянию.
func (m *RedisModule) applySentinel(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	authPass := stringOrEmpty(f["auth_pass"])
	authUser := stringOrEmpty(f["auth_user"])
	masterName := strings.TrimSpace(stringOrEmpty(f["master_name"]))
	if masterName == "" {
		masterName = defaultMasterName
	}
	redisVersion := stringOrEmpty(f["redis_version"])

	globalsAll, perMaster := classifyConfig(stringMap(f["config"]), masterName)
	globals := supportedGlobals(globalsAll, redisVersion)

	monitor := nodeSpec(f["monitor"])

	changed := false
	var actions []string

	if len(monitor) > 0 {
		mChanged, err := reconcileMonitor(ctx, conn, masterName, monitor, authUser, authPass, &actions)
		if err != nil {
			return sendFailure(stream, redactError(err, authPass))
		}
		changed = changed || mChanged

		pChanged, err := reconcileMasterParams(ctx, conn, masterName, perMaster, monitor, &actions)
		if err != nil {
			return sendFailure(stream, redactError(err, authPass))
		}
		changed = changed || pChanged
	}

	// SENTINEL CONFIG GET/SET (механизм reconcileGlobals целиком) появился в Redis
	// 7.0: на 6.2 ЛЮБОЙ global завалит apply на первом же CONFIG GET. На <7.0
	// пропускаем все globals целиком (no-op) — НЕ только loglevel; непустой набор
	// помечаем warning-действием, чтобы оператор видел незаполненное желаемое.
	if sentinelGlobalsSupported(redisVersion) {
		gChanged, err := reconcileGlobals(ctx, conn, globals, &actions)
		if err != nil {
			return sendFailure(stream, redactError(err, authPass))
		}
		changed = changed || gChanged
	} else if len(globals) > 0 {
		actions = append(actions, "globals_skipped_pre_7.0")
	}

	return sendOutcome(stream, changed, fmt.Sprintf("sentinel reconciled: %s", strings.Join(actions, ",")), map[string]any{
		"master_name": masterName,
		"actions":     strings.Join(actions, ","),
	})
}

// sentinelGlobalsSupported — умеет ли версия SENTINEL CONFIG GET/SET (>=7.0).
// Это механизм reconcileGlobals целиком, не отдельный параметр. Пустая/неизвестная
// версия → false (fail-closed: на неизвестной версии globals не трогаем).
func sentinelGlobalsSupported(redisVersion string) bool {
	return versionGE(redisVersion, [3]int{7, 0, 0})
}

// classifyConfig раскладывает директивы sentinel.conf на (globals, per_master).
// Чистая функция (перенос classify_config):
//   - plain "loglevel" -> global;
//   - "sentinel <param>" (одно слово) -> global;
//   - "sentinel <opt> <master>" -> per-master, только для нашего master_name;
//   - остальное (dir/port/tls-*/pidfile — startup-only) игнорируется.
func classifyConfig(config map[string]string, masterName string) (globals, perMaster map[string]string) {
	globals = map[string]string{}
	perMaster = map[string]string{}
	for key, value := range config {
		if key == "loglevel" {
			globals["loglevel"] = value
			continue
		}
		if !strings.HasPrefix(key, "sentinel ") {
			continue
		}
		tokens := strings.Fields(strings.TrimSpace(key[len("sentinel "):]))
		switch {
		case len(tokens) == 1:
			globals[tokens[0]] = value
		case len(tokens) == 2 && tokens[1] == masterName:
			perMaster[tokens[0]] = value
		}
	}
	return globals, perMaster
}

// supportedGlobals оставляет только глобальные параметры, которые умеет SENTINEL
// CONFIG SET на этой версии (перенос supported_globals). loglevel в Sentinel —
// только с Redis 7.0; на 6.2 отбрасываем. Секретные globals (sentinel-pass)
// отбрасываем тут же — diff по ним невозможен, в create-срезе rotate нет.
func supportedGlobals(globals map[string]string, redisVersion string) map[string]string {
	out := map[string]string{}
	for key, value := range globals {
		if key == "loglevel" {
			if versionGE(redisVersion, [3]int{7, 0, 0}) {
				out[key] = value
			}
			continue
		}
		if secretGlobalParams[key] {
			continue
		}
		if sentinelGlobalParams[key] {
			out[key] = value
		}
	}
	return out
}

// computeMonitorAction — что сделать с монитором: "add" (нет такого master),
// "readd" (адрес сменился), "none" (перенос compute_monitor_action). current —
// map поля->значение из SENTINEL MASTER (или nil, если master неизвестен).
func computeMonitorAction(current map[string]string, ip, port string) string {
	if current == nil {
		return "add"
	}
	if current["ip"] != ip || current["port"] != port {
		return "readd"
	}
	return "none"
}

// computeSetUpdates вычисляет опции, отличающиеся от текущего (перенос
// compute_set_updates): {опция: новое-значение} для тех, где cur отсутствует или
// не совпал. Возвращается отсортированный список ключей для детерминизма SET.
func computeSetUpdates(desired, current map[string]string) (keys []string, values map[string]string) {
	values = map[string]string{}
	for opt, value := range desired {
		if cur, ok := current[opt]; !ok || cur != value {
			values[opt] = value
		}
	}
	keys = make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, values
}

// reconcileMonitor приводит монитор к желаемому адресу (перенос
// reconcile_monitor). add/readd → SENTINEL MONITOR; readd сначала REMOVE. auth
// на master задаётся при создании/пересоздании монитора (у нового монитора их
// нет). Возвращает changed (создан/пересоздан монитор).
func reconcileMonitor(ctx context.Context, conn redisConn, masterName string, monitor map[string]*structpb.Value, authUser, authPass string, actions *[]string) (bool, error) {
	ip := strings.TrimSpace(stringOrEmpty(monitor["ip"]))
	port := strconv.Itoa(intOrDefault(monitor["port"], 0))
	quorum := intOrDefault(monitor["quorum"], 1)

	current, err := sentinelMaster(ctx, conn, masterName)
	if err != nil {
		return false, fmt.Errorf("SENTINEL MASTER %s: %w", masterName, err)
	}
	action := computeMonitorAction(current, ip, port)

	if action == "readd" {
		if _, err := conn.Do(ctx, "SENTINEL", "REMOVE", masterName); err != nil {
			return false, fmt.Errorf("SENTINEL REMOVE %s: %w", masterName, err)
		}
		*actions = append(*actions, "sentinel_remove")
	}
	if action == "add" || action == "readd" {
		if _, err := conn.Do(ctx, "SENTINEL", "MONITOR", masterName, ip, port, strconv.Itoa(quorum)); err != nil {
			return false, fmt.Errorf("SENTINEL MONITOR %s: %w", masterName, err)
		}
		*actions = append(*actions, "sentinel_monitor")

		// auth на master — только у нового/пересозданного монитора (у него их нет).
		if authUser != "" {
			if _, err := conn.Do(ctx, "SENTINEL", "SET", masterName, "auth-user", authUser); err != nil {
				return false, fmt.Errorf("SENTINEL SET %s auth-user: %w", masterName, err)
			}
		}
		if authPass != "" {
			if _, err := conn.Do(ctx, "SENTINEL", "SET", masterName, "auth-pass", authPass); err != nil {
				return false, fmt.Errorf("SENTINEL SET %s auth-pass: %w", masterName, err)
			}
		}
		if authUser != "" || authPass != "" {
			*actions = append(*actions, "sentinel_set_auth")
		}
	}

	return action == "add" || action == "readd", nil
}

// reconcileMasterParams обновляет параметры master-а через SENTINEL SET (перенос
// reconcile_master_params). quorum из monitor добавляется в желаемое. Применяются
// только реально отличающиеся (compute_set_updates). Возвращает changed.
func reconcileMasterParams(ctx context.Context, conn redisConn, masterName string, perMaster map[string]string, monitor map[string]*structpb.Value, actions *[]string) (bool, error) {
	desired := make(map[string]string, len(perMaster)+1)
	for k, v := range perMaster {
		desired[k] = v
	}
	if q := monitor["quorum"]; q != nil {
		desired["quorum"] = strconv.Itoa(intOrDefault(q, 1))
	}
	if len(desired) == 0 {
		return false, nil
	}

	current, err := sentinelMaster(ctx, conn, masterName)
	if err != nil {
		return false, fmt.Errorf("SENTINEL MASTER %s: %w", masterName, err)
	}
	keys, values := computeSetUpdates(desired, current)
	for _, opt := range keys {
		if _, err := conn.Do(ctx, "SENTINEL", "SET", masterName, opt, values[opt]); err != nil {
			return false, fmt.Errorf("SENTINEL SET %s %s: %w", masterName, opt, err)
		}
	}
	if len(keys) > 0 {
		*actions = append(*actions, "sentinel_set")
	}
	return len(keys) > 0, nil
}

// reconcileGlobals обновляет глобальные параметры через SENTINEL CONFIG SET
// (перенос reconcile_globals). Применяется только при отличии от текущего
// (SENTINEL CONFIG GET). Секретные globals сюда не доходят (отфильтрованы
// supportedGlobals). Возвращает changed. Имена директив детерминированы (sort).
func reconcileGlobals(ctx context.Context, conn redisConn, globals map[string]string, actions *[]string) (bool, error) {
	keys := make([]string, 0, len(globals))
	for k := range globals {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	changed := false
	for _, param := range keys {
		value := globals[param]
		current, err := sentinelConfigGet(ctx, conn, param)
		if err != nil {
			return changed, fmt.Errorf("SENTINEL CONFIG GET %s: %w", param, err)
		}
		if current != value {
			if _, err := conn.Do(ctx, "SENTINEL", "CONFIG", "SET", param, value); err != nil {
				return changed, fmt.Errorf("SENTINEL CONFIG SET %s: %w", param, err)
			}
			changed = true
		}
	}
	if changed {
		*actions = append(*actions, "sentinel_config_set")
	}
	return changed, nil
}

// sentinelMaster читает SENTINEL MASTER <name> в map поле->значение. Ответ Redis
// — плоский массив [field, value, ...]; redisConn.Do стрингифицирует его как
// join пробелом, поэтому разбираем пары токенов. Если master неизвестен (Redis
// отвечает ошибкой "No such master") — возвращаем nil (для action=add).
func sentinelMaster(ctx context.Context, conn redisConn, name string) (map[string]string, error) {
	res, err := conn.Do(ctx, "SENTINEL", "MASTER", name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such master") {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(res) == "" {
		return nil, nil
	}
	return pairsToMap(strings.Fields(res)), nil
}

// sentinelConfigGet читает SENTINEL CONFIG GET <param> → значение или "" (перенос
// sentinel_config_get). Ответ — пара [param, value]; "" если параметр пуст/нет.
func sentinelConfigGet(ctx context.Context, conn redisConn, param string) (string, error) {
	res, err := conn.Do(ctx, "SENTINEL", "CONFIG", "GET", param)
	if err != nil {
		return "", err
	}
	return pairsToMap(strings.Fields(res))[param], nil
}

// pairsToMap сворачивает плоский срез [k0, v0, k1, v1, ...] в map. Нечётный
// хвост игнорируется. Используется для разбора массив-ответов SENTINEL.
func pairsToMap(tokens []string) map[string]string {
	out := make(map[string]string, len(tokens)/2)
	for i := 0; i+1 < len(tokens); i += 2 {
		out[tokens[i]] = tokens[i+1]
	}
	return out
}

// versionGE — version >= target по (major, minor, patch). Перенос parse_version/
// version_ge: из каждого dot-чанка берём ведущие цифры (8.0.3, v=8.0.3, 6.2 →
// (8,0,3)/(8,0,3)/(6,2,0)). Пустая версия → (0,0,0): version-gated параметры
// на неизвестной версии отбрасываются fail-closed.
func versionGE(version string, target [3]int) bool {
	v := versionTuple(version)
	rank := func(t [3]int) int { return t[0]*1_000_000 + t[1]*1_000 + t[2] }
	return rank(v) >= rank(target)
}

// versionTuple разбирает строку версии в (major, minor, patch). Нормализует
// distro-native форму перед разбором:
//   - epoch "N:" срезается (deb-форма "5:7.0.15-1~deb12u7" → major=7, НЕ 5);
//   - revision-хвост "-..." срезается ("7.0.15-1~deb12u7" → "7.0.15");
//   - префикс "v" игнорируется (из каждого чанка берём ведущие цифры).
func versionTuple(version string) [3]int {
	version = strings.TrimSpace(version)
	if i := strings.IndexByte(version, ':'); i >= 0 {
		version = version[i+1:] // срезаем epoch "N:"
	}
	if i := strings.IndexByte(version, '-'); i >= 0 {
		version = version[:i] // срезаем revision "-..."
	}

	var out [3]int
	for i, chunk := range strings.Split(version, ".") {
		if i >= 3 {
			break
		}
		digits := ""
		for _, ch := range chunk {
			if ch >= '0' && ch <= '9' {
				digits += string(ch)
			} else if digits != "" {
				break
			}
		}
		if digits != "" {
			out[i], _ = strconv.Atoi(digits)
		}
	}
	return out
}
