package beacon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// PortClosedName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const PortClosedName = beaconaddr.PortClosed

const (
	statePortOpen   State = "open"
	statePortClosed State = "closed"
)

// portClosedDefaultHost — целевой хост по умолчанию: локальный TCP-порт сервиса
// (типичный случай — наблюдать, что локальный демон слушает свой порт).
const portClosedDefaultHost = "127.0.0.1"

// portClosedDefaultTimeout — таймаут одного TCP-dial. Короткий: beacon-проверка
// должна укладываться в тик scheduler-а, висящий dial — это уже наблюдаемое
// «недоступно».
const portClosedDefaultTimeout = 3 * time.Second

// PortClosed — core-beacon наблюдения за доступностью TCP-порта (ADR-030).
// Read-only: один TCP-dial, без отправки данных в сокет. State: "open" если
// соединение установилось, "closed" если порт не принял (refused/timeout/host
// недоступен) — с точки зрения наблюдателя порт закрыт, это событие интереса
// (а не ошибка проверки).
//
// Params:
//   - `port` (int, required) — TCP-порт 1..65535;
//   - `host` (string, optional, default "127.0.0.1") — целевой хост/IP;
//   - `timeout` (string duration, optional, default "3s") — таймаут dial.
type PortClosed struct {
	// Dial вынесен в поле для подмены в unit-тестах (детерминированный fake без
	// реального сокета). В проде — net.Dialer.DialContext.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewPortClosed собирает beacon с production-dialer-ом (net.Dialer).
func NewPortClosed() *PortClosed {
	return &PortClosed{Dial: (&net.Dialer{}).DialContext}
}

func (b *PortClosed) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	port, err := parseBeaconPort(params)
	if err != nil {
		return "", nil, err
	}
	host, err := util.OptStringParam(params, "host")
	if err != nil {
		return "", nil, err
	}
	if host == "" {
		host = portClosedDefaultHost
	}
	timeout, err := optBeaconTimeout(params, portClosedDefaultTimeout)
	if err != nil {
		return "", nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, derr := b.Dial(dialCtx, "tcp", address)
	if derr != nil {
		// refused/timeout/no-route — порт недоступен наблюдателю → "closed".
		// Это валидное состояние, а не ошибка Check.
		return statePortClosed, portData(host, port), nil
	}
	_ = conn.Close()
	return statePortOpen, portData(host, port), nil
}

func portData(host string, port int) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"host": host,
		"port": port,
	})
	return s
}

// parseBeaconPort извлекает обязательный TCP-порт (1..65535). Принимает число
// (proto-json маршалит числа во float64) или строку (на случай ${...}-
// интерполяции, дающей строку) — паттерн core.firewall.parsePort.
func parseBeaconPort(params *structpb.Struct) (int, error) {
	if n, ok, err := util.OptIntParam(params, "port"); err == nil && ok {
		if n < 1 || n > 65535 {
			return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
		}
		return int(n), nil
	}
	s, serr := util.OptStringParam(params, "port")
	if serr != nil {
		return 0, fmt.Errorf("param %q: must be an integer", "port")
	}
	if s == "" {
		return 0, fmt.Errorf("param %q: required", "port")
	}
	n, cerr := strconv.Atoi(s)
	if cerr != nil {
		return 0, fmt.Errorf("param %q: invalid integer %q", "port", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
	}
	return n, nil
}

// optBeaconTimeout разбирает опциональный param timeout по convention `duration`
// Soul Stack (shared/config — Go ParseDuration + суффикс `<N>d`). Пустой param →
// def. Единый парсер для port_closed / http_unhealthy.
func optBeaconTimeout(params *structpb.Struct, def time.Duration) (time.Duration, error) {
	s, err := util.OptStringParam(params, "timeout")
	if err != nil {
		return 0, err
	}
	if s == "" {
		return def, nil
	}
	d, perr := config.ParseDuration(s)
	if perr != nil {
		return 0, fmt.Errorf("param %q: invalid duration %q", "timeout", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("param %q: must be positive, got %q", "timeout", s)
	}
	return d, nil
}
