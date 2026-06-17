package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Session — узкая абстракция одной SSH-сессии, нужная диспетчеру: запустить
// `soul apply`, подать stdin (protojson ApplyRequest) и вернуть stdout-reader
// (NDJSON-поток) + ожидание завершения с exit-кодом.
//
// Интерфейс (а не конкретный *sshSession) вынесен, чтобы [SshDispatcher]
// тестировался без живого sshd: unit-тесты подставляют мок Session, проверяя
// stdin-feed → NDJSON-parse → RunResult. Live-sshd-интеграция — follow-up
// (docker занят другим прогоном, см. отчёт).
type Session interface {
	// Run выполняет команду на хосте, передавая stdinData в stdin процесса, и
	// блокирует до завершения. Возвращает stdout целиком и ошибку:
	//   - nil — процесс завершился с exit 0;
	//   - *ssh.ExitError — ненулевой exit (stdout всё равно возвращается: там
	//     может лежать NDJSON с FAILED-RunResult).
	// stdout возвращается строкой, а не reader-ом: oneshot `soul apply` пишет
	// ограниченный объём (NDJSON-поток без long-running progress, ADR-012),
	// потоковый разбор по мере поступления — оптимизация S3.
	Run(ctx context.Context, cmd string, stdinData []byte) (stdout string, err error)
	// Close закрывает SSH-соединение. Идемпотентен.
	Close() error
}

// HostKeyAuthority — публичный ключ CA, подписавшего host-сертификаты целевых
// хостов (Vault SSH CA host-key). Проверка host-cert идёт против него: хост
// предъявляет cert, подписанный этим CA → доверяем; чужой/самоподписанный →
// reject. Заменяет TOFU/known_hosts на CA-trust (PM-decision S0).
//
// S7-3 ввёл multi-CA через [NamedHostKeyAuthority] / [LoadHostCAs]: основная
// поверхность — [DialConfig.HostAuthorities]. `HostKeyAuthority` остаётся как
// типизированный value-объект (используется в [LoadHostCA] singular helper-е
// для backward-compat path) и для `ProxyHostAuthority` (отдельный proxy-CA,
// без multi-CA расширения).
type HostKeyAuthority struct {
	// CAPublicKey — публичная часть host-CA.
	CAPublicKey ssh.PublicKey
}

// DialConfig — параметры открытия одной SSH-сессии.
type DialConfig struct {
	// Host — FQDN/IP целевого хоста (= SID push-хоста).
	Host string
	// Port — TCP-порт sshd.
	Port int
	// User — SSH-пользователь для входа.
	User string
	// Auth — методы аутентификации Keeper-а на хосте (из SignReply: cert+key
	// либо ephemeral keypair). Готовятся диспетчером по результату Sign.
	//
	// Канонический Teleport-flow: один и тот же набор Auth (signed user-cert на
	// ephemeral keypair) используется на ОБА хопа — proxy и target. Cert от
	// Teleport/Vault SSH CA авторизует пользователя на обеих сторонах.
	Auth []ssh.AuthMethod
	// HostAuthorities — multi-CA-набор для verify host-cert целевого хоста
	// (S7-3, ADR-032 amendment 2026-05-26). Непустой; на handshake-е делается
	// OR-проверка по всем элементам через ssh.CertChecker.IsHostAuthority. При
	// непустом [ProxyJump] и nil [ProxyHostAuthority] этот же набор используется
	// и для верификации host-cert proxy-хопа.
	HostAuthorities []NamedHostKeyAuthority
	// OnHostCAMatch — опц. callback на матч host-CA (для observability:
	// debug-log + `keeper_push_host_ca_used_total{ca_name=...}`-метрики). nil —
	// callback не вызывается. Caller — [SshDispatcher]; live-`Dial`-флоу собирает
	// его сам, тесты могут оставлять nil.
	OnHostCAMatch func(caName string)
	// ProxyJump — bastion/proxy в формате `[user@]host:port`, через который
	// идёт SSH-туннель до target. Пустая строка — прямой коннект (S0-flow).
	// Источник — [pluginv1.SignReply.proxy_jump] от SshProvider (Teleport).
	//
	// Семантика: dispatcher открывает SSH-client к proxy_jump-хосту, на нём
	// запрашивает direct-tcpip-канал к [Host]:[Port], и поверх этого канала
	// проводит второй SSH-handshake с target. Это эквивалент `ssh -J <proxy>
	// <host>`. Аутентификация — теми же [Auth] на обоих хопах (Teleport-flow:
	// один user-cert проходит через proxy и аутентифицирует на target).
	ProxyJump string
	// ProxyHostAuthority — отдельный CA для host-cert верификации proxy-хопа.
	// nil → для proxy используется [HostAuthorities] (типовой случай: один host-CA
	// подписывает host-cert-ы и proxy, и target). Заполняется, когда оператор
	// явно разделяет proxy-CA и target-CA через params плагина.
	ProxyHostAuthority *HostKeyAuthority
	// Timeout — таймаут TCP+handshake фазы соединения. При proxy_jump
	// применяется к каждому хопу отдельно (proxy и target).
	Timeout time.Duration
}

// hostCertCallback строит ssh.HostKeyCallback, который доверяет ТОЛЬКО хостам,
// предъявившим host-сертификат, подписанный любым CA из набора (S7-3 multi-CA
// OR-проверка). Не-cert host-key (голый ed25519/rsa без подписи CA) и cert
// чужого CA — reject.
//
// Реализация через ssh.CertChecker:
//   - IsHostAuthority(auth, addr) == true ⟺ auth совпадает с одним из CA в
//     наборе (по marshaled-форме). CertChecker сам проверяет, что
//     предъявленный cert имеет CertType=HostCert, подписан этим authority,
//     валиден по времени и его principals покрывают addr.
//   - HostKeyFallback не задан → если предъявлен не-cert ключ, проверка падает
//     (нет доверенного пути для голого host-key) — это и есть «отказ от TOFU».
//   - `onMatch` (может быть nil) — callback с именем matched-CA для
//     observability (`keeper_push_host_ca_used_total{ca_name=...}` + debug-log).
//
// Замечание по performance hot-path-а: набор CA закрепляется оператором в
// keeper.yml (closed-set единиц), линейный bytes.Equal по marshaled-форме
// внутри handshake-callback-а не требует индекса — handshake и так делает
// больше системной работы (crypto/network), а ключ-сравнение остаётся O(n)
// по числу CA в наборе.
func hostCertCallback(cas []NamedHostKeyAuthority, onMatch func(caName string)) ssh.HostKeyCallback {
	marshaled := make([][]byte, len(cas))
	names := make([]string, len(cas))
	for i, ca := range cas {
		marshaled[i] = ca.CAPubKey.Marshal()
		names[i] = ca.Name
	}
	checker := &ssh.CertChecker{
		IsHostAuthority: func(auth ssh.PublicKey, _ string) bool {
			authMarshaled := auth.Marshal()
			for i, m := range marshaled {
				if bytesEqual(authMarshaled, m) {
					if onMatch != nil {
						onMatch(names[i])
					}
					return true
				}
			}
			return false
		},
	}
	return checker.CheckHostKey
}

// bytesEqual — constant-time-неважное сравнение marshaled-ключей (это публичные
// данные, timing-канала нет). Вынесено ради читаемости callback-а.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sshSession — production-реализация [Session] поверх golang.org/x/crypto/ssh.
//
// proxy — опционально удерживаемый client до bastion-а (когда target открыт
// через direct-tcpip-канал на proxy). Закрывается ПОСЛЕ target, чтобы не
// оборвать active канал.
type sshSession struct {
	client *ssh.Client
	proxy  *ssh.Client
}

// Dial открывает SSH-соединение по cfg с CA-signed host-cert verification.
// Возвращает [Session] либо ошибку connect/handshake (в т.ч. отказ host-cert
// проверки — host предъявил cert не от нашего CA или голый ключ).
//
// Если cfg.ProxyJump непуст — сначала открывается SSH-client к proxy-хопу, на
// нём запрашивается direct-tcpip до target, поверх канала проводится второй
// SSH-handshake (эквивалент `ssh -J <proxy> <host>`). Аутентификация — теми же
// cfg.Auth на обоих хопах (Teleport-flow: один signed user-cert).
func Dial(ctx context.Context, cfg DialConfig) (Session, error) {
	if len(cfg.HostAuthorities) == 0 {
		// fail-closed: без CA нет доверенного пути проверки host-key.
		// InsecureIgnoreHostKey в push НЕ допускается (PM-decision S0).
		return nil, errors.New("push: HostAuthorities is empty (CA-signed host-cert verification)")
	}
	if cfg.ProxyJump == "" {
		return dialDirect(ctx, cfg)
	}
	return dialViaProxy(ctx, cfg)
}

// dialDirect — прямой коннект (proxy_jump пуст).
func dialDirect(ctx context.Context, cfg DialConfig) (Session, error) {
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(cfg.HostAuthorities, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("push: TCP-соединение с %s: %w", addr, err)
	}
	client, err := newSSHClient(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		// Отказ host-cert проверки приходит сюда же (handshake fail) — это
		// штатный путь reject-а чужого/самоподписанного host-key.
		return nil, fmt.Errorf("push: SSH-handshake с %s: %w", addr, err)
	}
	return &sshSession{client: client}, nil
}

// dialViaProxy — Teleport-flow: SSH-client к proxy, direct-tcpip-канал до
// target, второй handshake поверх канала. Оба хопа проходят CA-signed host-cert
// verification: proxy — через cfg.ProxyHostAuthority (или cfg.HostAuthorities при
// nil); target — через cfg.HostAuthorities. Аутентификация — теми же cfg.Auth на
// обоих хопах. Cleanup при ошибке fail-closed: target дёргается раньше proxy,
// иначе direct-tcpip-канал умрёт раньше своего ssh-channel-а.
func dialViaProxy(ctx context.Context, cfg DialConfig) (Session, error) {
	proxyUser, proxyAddr, err := parseProxyJump(cfg.ProxyJump, cfg.User)
	if err != nil {
		return nil, fmt.Errorf("push: parse proxy_jump %q: %w", cfg.ProxyJump, err)
	}
	proxyCAs := cfg.HostAuthorities
	if cfg.ProxyHostAuthority != nil {
		if cfg.ProxyHostAuthority.CAPublicKey == nil {
			return nil, errors.New("push: ProxyHostAuthority задан, но CAPublicKey пуст")
		}
		// Отдельный proxy-CA — singleton-набор, multi-CA для proxy-хопа в MVP
		// не вводится (отдельный CA-bag здесь нужен под кейс «разные владельцы
		// CA для proxy и target», не под «несколько proxy-CA»).
		proxyCAs = []NamedHostKeyAuthority{{
			Name:     "proxy",
			CAPubKey: cfg.ProxyHostAuthority.CAPublicKey,
		}}
	}

	// Хоп 1: TCP+SSH-handshake до proxy_jump-хоста.
	proxyCfg := &ssh.ClientConfig{
		User:            proxyUser,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(proxyCAs, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	d := net.Dialer{Timeout: cfg.Timeout}
	tcpConn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("push: TCP-соединение с proxy %s: %w", proxyAddr, err)
	}
	proxyClient, err := newSSHClient(tcpConn, proxyAddr, proxyCfg)
	if err != nil {
		_ = tcpConn.Close()
		return nil, fmt.Errorf("push: SSH-handshake с proxy %s: %w", proxyAddr, err)
	}

	// Хоп 2: direct-tcpip-канал proxy → target + второй handshake.
	targetAddr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tunnelConn, err := proxyClient.Dial("tcp", targetAddr)
	if err != nil {
		_ = proxyClient.Close()
		return nil, fmt.Errorf("push: direct-tcpip через proxy %s до %s: %w", proxyAddr, targetAddr, err)
	}
	targetCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(cfg.HostAuthorities, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	targetClient, err := newSSHClient(tunnelConn, targetAddr, targetCfg)
	if err != nil {
		_ = tunnelConn.Close()
		_ = proxyClient.Close()
		return nil, fmt.Errorf("push: SSH-handshake с target %s через proxy %s: %w", targetAddr, proxyAddr, err)
	}
	return &sshSession{client: targetClient, proxy: proxyClient}, nil
}

// newSSHClient — обёртка ssh.NewClientConn+ssh.NewClient: один контракт для
// direct dial и для туннельного net.Conn (direct-tcpip-канал поверх proxy).
func newSSHClient(conn net.Conn, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// parseProxyJump разбирает SignReply.proxy_jump в `[user@]host:port`. user
// опционален: при отсутствии наследуется defaultUser (тот же, что для target —
// Teleport canonical-flow, один пользователь на оба хопа).
func parseProxyJump(raw, defaultUser string) (user, addr string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("пустая строка")
	}
	user = defaultUser
	hostPort := raw
	if at := strings.Index(raw, "@"); at >= 0 {
		user = raw[:at]
		hostPort = raw[at+1:]
		if user == "" {
			return "", "", errors.New("user пуст до '@'")
		}
	}
	host, port, splitErr := net.SplitHostPort(hostPort)
	if splitErr != nil {
		return "", "", fmt.Errorf("ожидался host:port: %w", splitErr)
	}
	if host == "" || port == "" {
		return "", "", errors.New("host или port пуст")
	}
	return user, net.JoinHostPort(host, port), nil
}

// Run открывает channel-сессию, подаёт stdin и собирает stdout команды.
func (s *sshSession) Run(ctx context.Context, cmd string, stdinData []byte) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("push: открытие SSH-сессии: %w", err)
	}
	defer sess.Close()

	stdinPipe, err := sess.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("push: stdin pipe: %w", err)
	}
	var stdout strings.Builder
	sess.Stdout = &stdout

	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("push: запуск %q: %w", cmd, err)
	}

	// Подаём stdin (ApplyRequest protojson) и закрываем — `soul apply` читает
	// stdin до EOF, иначе процесс не стартует прогон. Для команд без stdin
	// (delivery/cleanup-helpers) сразу закрываем pipe; EOF/уже-закрытый-канал
	// от Close() в этом случае — норма, не ошибка.
	var writeErr error
	if len(stdinData) == 0 {
		_ = stdinPipe.Close()
	} else {
		writeErr = writeAllAndClose(stdinPipe, stdinData)
	}

	// ctx-отмена: прерываем сессию, не дожидаясь Wait. soul-side guard и барьер
	// Keeper-а обрабатывают обрыв как fail (ParseStream вернёт ErrNoRunResult).
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		_ = sess.Close()
		<-done // дождаться завершения goroutine, не утечь
		return stdout.String(), fmt.Errorf("push: сессия прервана: %w", ctx.Err())
	case waitErr := <-done:
		if writeErr != nil && waitErr == nil {
			// stdin не доставлен, но процесс отчитался 0 — противоречие; вернём
			// write-ошибку как первичную.
			return stdout.String(), fmt.Errorf("push: подача stdin: %w", writeErr)
		}
		return stdout.String(), waitErr
	}
}

// Close закрывает target первым, потом proxy. Обратный порядок оборвал бы
// direct-tcpip-канал раньше его SSH-channel-а на target-стороне. Идемпотентен:
// двойной вызов даст nil после первого успешного закрытия.
func (s *sshSession) Close() error {
	var firstErr error
	if s.client != nil {
		if err := s.client.Close(); err != nil {
			firstErr = err
		}
		s.client = nil
	}
	if s.proxy != nil {
		if err := s.proxy.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.proxy = nil
	}
	return firstErr
}

// writeAllAndClose пишет data в stdin-pipe и закрывает его (EOF для soul apply).
// Close вызывается всегда, даже при write-ошибке (иначе процесс зависнет на
// чтении stdin).
func writeAllAndClose(w interface {
	Write([]byte) (int, error)
	Close() error
}, data []byte) error {
	_, werr := w.Write(data)
	cerr := w.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
