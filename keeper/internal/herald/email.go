package herald

// SMTP delivery axis for Herald (ADR-052 amendment): email type is a separate class, NOT
// [channelDriver] (no httpDelivery/HTTP transport). Own transport (net/smtp),
// own SSRF guard (resolve smtp_host -> block private IPs via [netguard.IsBlockedIP]),
// and own branch in [DeliveryWorker.deliver]. Password is vault-ref config.password_ref,
// resolved from Vault on delivery; does not leak into error text.
//
// Transport is standard library net/smtp (no third-party dependency): manual dial
// (SSRF guard by resolved IP) -> tls_mode (starttls/tls/none) -> optional
// PLAIN auth -> send. Body is minimal RFC5322 (From/To/Subject/Date + text).

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
	// emailTLSStartTLS is STARTTLS upgrade of open connection (submission 587).
	emailTLSStartTLS = "starttls"
	// emailTLSImplicit is implicit TLS from first byte (smtps 465).
	emailTLSImplicit = "tls"
	// emailTLSNone is without TLS (plain; only for trusted local relay).
	emailTLSNone = "none"

	// emailDialTimeout is timeout for establishing TCP connection to SMTP server.
	emailDialTimeout = 10 * time.Second
)

// emailFields is descriptor of email config fields for CRUD validator and catalog
// GET /v1/herald-types (SMTP axis outside channelDrivers, but catalog is unified).
func emailFields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "smtp_host", Label: "SMTP host", Required: true, Kind: KindString},
		{Name: "smtp_port", Label: "SMTP port", Required: true, Kind: KindInt},
		{Name: "from", Label: "Sender address", Required: true, Kind: KindString},
		{Name: "to", Label: "Recipients", Required: true, Kind: KindListString},
		{Name: "username", Label: "SMTP login", Kind: KindString},
		{Name: "password_ref", Label: "SMTP password Vault ref", Secret: true, Kind: KindVaultRef},
		{Name: "tls_mode", Label: "TLS mode", Kind: KindEnum, EnumValues: []string{"", emailTLSStartTLS, emailTLSImplicit, emailTLSNone}},
	}
}

// validateEmailConfig checks email config: field shape (generic walk by
// [emailFields]) + domain invariant for port (1..65535). Secret (password_ref) is NOT
// read from Vault on CRUD; it stays as vault-ref.
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

// emailTarget contains resolved parameters for sending one email.
type emailTarget struct {
	host     string
	port     int
	from     string
	to       []string
	username string
	password string
	tlsMode  string
}

// resolveEmailTarget extracts send parameters from Herald row and resolves
// password (if password_ref is set) from Vault. Missing required fields
// (config changed after create) -> terminal-no-retry; password Vault failure ->
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

// deliverEmail sends one email (SMTP branch of [DeliveryWorker.deliver]).
// SSRF guard: smtp_host is resolved, ALL IPs are checked with [netguard.IsBlockedIP]
// (private/loopback/metadata blocked; email has no allow_private opt-out).
// Resolved IP is used for dial (rebind-safe). tls_mode controls TLS
// upgrade. Errors: terminal-no-retry (bad config/blocked IP/invalid
// tls_mode) or transient (dial/TLS/SMTP failure, retry).
//
// resolver is injected for SSRF guard testability (in prod,
// [netguard.DefaultResolver]).
func deliverEmail(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader, resolver netguard.Resolver) error {
	target, err := resolveEmailTarget(ctx, h, kv)
	if err != nil {
		return err
	}
	if resolver == nil {
		resolver = netguard.DefaultResolver
	}

	// SSRF guard: resolve host, block private/metadata on all A records
	// (one blocked IP -> reject whole target, like GuardedDialContext), dial by
	// checked IP (rebind-safe).
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

// sendSMTP performs SMTP exchange using already validated dialAddr.
// tls_mode: implicit TLS (tls) is TLS from first byte; starttls is open
// connection + STARTTLS upgrade; none is plain. Auth (PLAIN) only if
// username is set. ServerName for TLS verification is domain host (not dial-IP).
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

// buildEmailMessage builds minimal RFC5322 email: From/To/Subject/Date +
// text part. Subject is event summary (event_type + tiding); text is same
// human-readable summary as messengers ([messageText], masking/projection
// already applied). Header separators are CRLF (RFC5322).
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

// sanitizeHeader removes CR/LF from header value (header-injection protection:
// event_type/tiding are domain-validated, but defense in depth).
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// configPort extracts smtp_port from config as int (JSON number arrives as float64).
// Error means missing, not a number, or fractional.
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

// configStringList extracts list of non-empty strings from config (email to). Non-strings
// and empty strings are dropped (shape already validated on CRUD).
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
