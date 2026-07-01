package herald

// SMTP-ось доставки Herald (ADR-052 amendment): тип email — отдельный класс, НЕ
// [channelDriver] (нет httpDelivery/HTTP-транспорта). Свой транспорт (net/smtp),
// свой SSRF-guard (резолв smtp_host → блок приватных IP по [netguard.IsBlockedIP])
// и своя ветка в [DeliveryWorker.deliver]. Пароль — vault-ref config.password_ref,
// резолвится из Vault на доставке; в текст ошибок не утекает.
//
// Транспорт — стандартная библиотека net/smtp (без сторонней зависимости): dial
// вручную (SSRF-guard по резолвнутому IP) → tls_mode (starttls/tls/none) → опц.
// PLAIN-auth → отправка. Тело — минимальное RFC5322 (From/To/Subject/Date + text).

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

const (
	// emailTLSStartTLS — STARTTLS-апгрейд открытого соединения (submission 587).
	emailTLSStartTLS = "starttls"
	// emailTLSImplicit — implicit TLS с первого байта (smtps 465).
	emailTLSImplicit = "tls"
	// emailTLSNone — без TLS (plain; только для доверенного локального relay).
	emailTLSNone = "none"

	// emailDialTimeout — таймаут установления TCP-соединения с SMTP-сервером.
	emailDialTimeout = 10 * time.Second
)

// emailFields — дескриптор config-полей email для CRUD-валидатора и каталога
// GET /v1/herald-types (SMTP-ось вне channelDrivers, но каталог единый).
func emailFields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "smtp_host", Label: "SMTP-хост", Required: true, Kind: KindString},
		{Name: "smtp_port", Label: "SMTP-порт", Required: true, Kind: KindInt},
		{Name: "from", Label: "Адрес отправителя", Required: true, Kind: KindString},
		{Name: "to", Label: "Получатели", Required: true, Kind: KindListString},
		{Name: "username", Label: "SMTP-логин", Kind: KindString},
		{Name: "password_ref", Label: "Vault-ref пароля SMTP", Secret: true, Kind: KindVaultRef},
		{Name: "tls_mode", Label: "Режим TLS", Kind: KindEnum, EnumValues: []string{"", emailTLSStartTLS, emailTLSImplicit, emailTLSNone}},
	}
}

// validateEmailConfig проверяет config email: форма полей (generic-обход по
// [emailFields]) + доменный инвариант порта (1..65535). Секрет (password_ref) НЕ
// читается из Vault на CRUD — держится как vault-ref.
func validateEmailConfig(config map[string]any) error {
	if err := validateBySpec(HeraldEmail, emailFields(), config); err != nil {
		return err
	}
	port, err := configPort(config)
	if err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("herald: email config %q must be in 1..65535, got %d", "smtp_port", port)
	}
	return nil
}

// emailTarget — резолвнутые параметры отправки одного письма.
type emailTarget struct {
	host     string
	port     int
	from     string
	to       []string
	username string
	password string
	tlsMode  string
}

// resolveEmailTarget извлекает параметры отправки из Herald-записи и резолвит
// пароль (если задан password_ref) из Vault. Отсутствие обязательных полей
// (config изменён после create) → terminal-no-retry; Vault-сбой пароля →
// transient (retry).
func resolveEmailTarget(ctx context.Context, h *Herald, kv KVReader) (*emailTarget, error) {
	host, ok := configString(h.Config, "smtp_host")
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: email channel %q has no smtp_host", h.Name)}
	}
	port, err := configPort(h.Config)
	if err != nil {
		return nil, errTerminalNoRetry{err}
	}
	from, ok := configString(h.Config, "from")
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: email channel %q has no from", h.Name)}
	}
	to := configStringList(h.Config, "to")
	if len(to) == 0 {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: email channel %q has no recipients", h.Name)}
	}

	tlsMode := emailTLSStartTLS
	if m, _ := h.Config["tls_mode"].(string); m != "" {
		tlsMode = m
	}
	t := &emailTarget{host: host, port: port, from: from, to: to, tlsMode: tlsMode}
	t.username, _ = h.Config["username"].(string)

	if ref, ok := configString(h.Config, "password_ref"); ok {
		pw, err := resolveVaultString(ctx, kv, ref)
		if err != nil {
			return nil, err
		}
		t.password = pw
	}
	return t, nil
}

// deliverEmail отправляет одно письмо (SMTP-ветка [DeliveryWorker.deliver]).
// SSRF-guard: smtp_host резолвится, ВСЕ IP проверяются на [netguard.IsBlockedIP]
// (приватка/loopback/metadata блокируются — email не имеет allow_private opt-out).
// Резолвнутый IP используется для dial (rebind-safe). tls_mode управляет TLS-
// апгрейдом. Ошибки: terminal-no-retry (битый config/заблокированный IP/невалидный
// tls_mode) либо transient (dial/TLS/SMTP-сбой — retry).
//
// resolver инжектируется ради тестируемости SSRF-guard-а (в проде —
// [netguard.DefaultResolver]).
func deliverEmail(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader, resolver netguard.Resolver) error {
	target, err := resolveEmailTarget(ctx, h, kv)
	if err != nil {
		return err
	}
	if resolver == nil {
		resolver = netguard.DefaultResolver
	}

	// SSRF-guard: резолвим host, блокируем приватку/metadata по всем A-записям
	// (один заблокированный IP → отказ целиком, как GuardedDialContext), dial по
	// проверенному IP (rebind-safe).
	ipAddrs, err := resolver.LookupIPAddr(ctx, target.host)
	if err != nil {
		return fmt.Errorf("herald: email resolve %q: %w", target.host, err)
	}
	if len(ipAddrs) == 0 {
		return errTerminalNoRetry{fmt.Errorf("herald: email host %q resolved to no addresses", target.host)}
	}
	for _, a := range ipAddrs {
		if netguard.IsBlockedIP(a.IP) {
			return errTerminalNoRetry{fmt.Errorf("herald: email host %q resolves to blocked address", target.host)}
		}
	}
	dialAddr := net.JoinHostPort(ipAddrs[0].IP.String(), strconv.Itoa(target.port))

	msg := buildEmailMessage(target, job)
	return sendSMTP(ctx, target, dialAddr, msg)
}

// sendSMTP выполняет SMTP-обмен по уже-провалидированному адресу dialAddr.
// tls_mode: implicit TLS (tls) — TLS с первого байта; starttls — открытое
// соединение + STARTTLS-апгрейд; none — plain. Auth (PLAIN) — только если задан
// username. ServerName для TLS-верификации — доменное имя host (не dial-IP).
func sendSMTP(ctx context.Context, target *emailTarget, dialAddr string, msg []byte) error {
	dialer := &net.Dialer{Timeout: emailDialTimeout}
	tlsConf := &tls.Config{ServerName: target.host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	switch target.tlsMode {
	case emailTLSImplicit:
		conn, err = tls.DialWithDialer(dialer, "tcp", dialAddr, tlsConf)
	case emailTLSStartTLS, emailTLSNone:
		conn, err = dialer.DialContext(ctx, "tcp", dialAddr)
	default:
		return errTerminalNoRetry{fmt.Errorf("herald: email unknown tls_mode %q", target.tlsMode)}
	}
	if err != nil {
		return fmt.Errorf("herald: email dial: %w", err)
	}

	client, err := smtp.NewClient(conn, target.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("herald: email smtp handshake: %w", err)
	}
	defer client.Close()

	if target.tlsMode == emailTLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("herald: email server does not support STARTTLS")
		}
		if err := client.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("herald: email starttls: %w", err)
		}
	}

	if target.username != "" {
		auth := smtp.PlainAuth("", target.username, target.password, target.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("herald: email auth: %w", err)
		}
	}

	if err := client.Mail(target.from); err != nil {
		return fmt.Errorf("herald: email MAIL FROM: %w", err)
	}
	for _, rcpt := range target.to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("herald: email RCPT TO: %w", err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("herald: email DATA: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		wc.Close()
		return fmt.Errorf("herald: email write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("herald: email finalize body: %w", err)
	}
	return client.Quit()
}

// buildEmailMessage собирает минимальное RFC5322-письмо: From/To/Subject/Date +
// text-часть. Subject — сводка события (event_type + tiding); text — та же
// человекочитаемая сводка, что мессенджеры ([messageText], маскинг/projection
// уже применены). CRLF-разделители заголовков (RFC5322).
func buildEmailMessage(target *emailTarget, job *DeliveryJob) []byte {
	subject := string(job.EventType)
	if job.Tiding != "" {
		subject += " — " + job.Tiding
	}
	var b strings.Builder
	b.WriteString("From: " + target.from + "\r\n")
	b.WriteString("To: " + strings.Join(target.to, ", ") + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(messageText(job))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// sanitizeHeader вырезает CR/LF из значения заголовка (header-injection-защита:
// event_type/tiding — доменно-валидированные, но defence in depth).
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// configPort извлекает smtp_port из config как int (JSON-число приходит float64).
// Ошибка — отсутствует, не число или дробное.
func configPort(config map[string]any) (int, error) {
	raw, ok := config["smtp_port"]
	if !ok {
		return 0, fmt.Errorf("herald: email config requires %q", "smtp_port")
	}
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("herald: email config %q must be an integer", "smtp_port")
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("herald: email config %q must be a number", "smtp_port")
	}
}

// configStringList извлекает список непустых строк из config (email to). Не-строки
// и пустые отбрасываются (форма уже провалидирована на CRUD).
func configStringList(config map[string]any, key string) []string {
	raw, ok := config[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, el := range raw {
		if s, ok := el.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
