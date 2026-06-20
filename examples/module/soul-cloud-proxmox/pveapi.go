package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pveAPI — узкое подмножество Proxmox VE API v2 (REST), которое использует
// драйвер. Сужение (а не *pveRealClient напрямую) даёт mockability для L0-unit-
// тестов без сети: тест подсовывает fake-реализацию (см. driver_test.go).
//
// Сигнатуры спроектированы под фактический набор операций драйвера (clone из
// template, статус, удаление, list по тегу, guest-agent IP), а не под полное
// покрытие API — следуем принципу «узкий contract = простой mock».
//
// Важно про vmid-paradigm: Proxmox VMID — int, существует только в контексте
// node (clone делается на конкретной node, get/delete тоже). Поэтому почти все
// методы принимают пару (node, vmid). Подход «глобальный vmid без node»
// невозможен в Proxmox: один и тот же endpoint /nodes/<node>/qemu/<vmid> не
// общий, route 404-ит при неверной node.
type pveAPI interface {
	// CloneVM делает POST /nodes/<source_node>/qemu/<template_vmid>/clone.
	// Возвращает UPID-задачи. Source-node может отличаться от newnode — Proxmox
	// сделает live-clone между node при наличии shared-storage.
	CloneVM(ctx context.Context, params CloneParams) (taskUPID string, err error)

	// NextID — GET /cluster/nextid: атомарно резервирует следующий свободный
	// VMID в рамках кластера. Используется когда profile.new_vmid_start не
	// задан (или 0).
	NextID(ctx context.Context) (int, error)

	// SetVMConfig — POST /nodes/<node>/qemu/<vmid>/config: задание полей
	// cloud-init (`ciuser`/`cipassword`/`sshkeys`/`cicustom`/…), tags, name.
	// Поля передаются map-ом → form-encoded body Proxmox-style.
	SetVMConfig(ctx context.Context, node string, vmid int, fields map[string]string) error

	// StartVM — POST /nodes/<node>/qemu/<vmid>/status/start. Возвращает UPID.
	StartVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// StopVM — POST /nodes/<node>/qemu/<vmid>/status/stop (forced). Возвращает
	// UPID. Если VM уже остановлена/удалена — Proxmox вернёт 500/«does not
	// exist» (классификатор переведёт в not_found/transient).
	StopVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// DeleteVM — DELETE /nodes/<node>/qemu/<vmid>. Возвращает UPID. Не падает
	// если VM running — Proxmox сам ругнётся 500; драйвер делает stop→delete.
	DeleteVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// GetVMStatus — GET /nodes/<node>/qemu/<vmid>/status/current.
	GetVMStatus(ctx context.Context, node string, vmid int) (VMStatus, error)

	// ListClusterVMs — GET /cluster/resources?type=vm. Используется в Create
	// (idempotent-find по tag) и в List. tag-фильтрация на стороне драйвера —
	// Proxmox-API не имеет server-side filter по tag в /cluster/resources.
	ListClusterVMs(ctx context.Context) ([]ClusterVM, error)

	// GetGuestAgentInterfaces — POST /nodes/<node>/qemu/<vmid>/agent/network-
	// get-interfaces. Возвращает IP первой не-loopback IPv4-интерфейс.
	// Возвращает пустую строку (без ошибки) если guest-agent ответил, но IP
	// ещё не назначен (DHCP-handshake в полёте); ошибку — если guest-agent не
	// настроен/не отвечает (502/timeout от Proxmox).
	GetGuestAgentInterfaces(ctx context.Context, node string, vmid int) (primaryIP string, err error)
}

// CloneParams — параметры clone-операции Proxmox.
type CloneParams struct {
	SourceNode    string // на какой node живёт template_vmid
	TemplateVMID  int    // VMID шаблона-источника
	NewVMID       int    // VMID новой VM (обязателен; берём NextID если 0)
	Name          string // hostname новой VM (Proxmox /name)
	TargetNode    string // node назначения (если отличается от SourceNode)
	TargetStorage string // storage для full-clone
	FullClone     bool   // true=copy, false=linked-snapshot
}

// VMStatus — поднабор полей /status/current, который нужен драйверу.
// Полная схема Proxmox шире (mem/cpu/uptime/…), но wait/probe нужен только
// `status` + qmpstatus + tags + name.
type VMStatus struct {
	Node      string   `json:"-"` // заполняется драйвером, не приходит из API
	VMID      int      `json:"vmid"`
	Status    string   `json:"status"`    // "running"|"stopped"
	QmpStatus string   `json:"qmpstatus"` // "running"|"paused"|… (детализирует Status)
	Name      string   `json:"name"`
	Tags      string   `json:"tags"` // semicolon-separated
	Maxmem    int64    `json:"maxmem"`
	Maxdisk   int64    `json:"maxdisk"`
	Cpus      float64  `json:"cpus"`
	Lock      string   `json:"lock"` // во время clone/migrate — "clone"/"migrate"
	Agent     int      `json:"agent"`
	NetIn     int64    `json:"netin"`
	NetOut    int64    `json:"netout"`
	IPS       []string `json:"-"` // не из API; драйвер заполняет из guest-agent отдельно
}

// ClusterVM — элемент /cluster/resources?type=vm. У cluster-resource VMID,
// node, status, tags доступны без per-node-вызовов.
type ClusterVM struct {
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Status string `json:"status"` // "running"|"stopped"
	Name   string `json:"name"`
	Tags   string `json:"tags"`
	Type   string `json:"type"` // "qemu"|"lxc"; нас интересует qemu
}

// pveCredentials — credentials провайдера, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет.
//
// Proxmox VE поддерживает две формы XOR:
//   - API-token (рекомендован, не expires): claims в одной строке формата
//     `<user>@<realm>!<token-id>=<value>`. Кладётся в Authorization-заголовок
//     как `PVEAPIToken=<token>`. Для read-only ролей достаточен.
//   - Ticket-based: username+password+realm → POST /access/ticket → cookie +
//     CSRFPreventionToken. Cookie действует 2 часа. Используется когда у
//     оператора нет API-token (legacy / dev).
//
// Поля XOR: либо token, либо username+password+realm. Endpoint — обязателен
// (https://<host>:8006/api2/json). insecure_tls — для self-signed cert (по
// умолчанию Proxmox его генерирует сам).
type pveCredentials struct {
	Endpoint    string // https://pve.example:8006 (без /api2/json — добавим сами)
	Token       string // PVEAPIToken=<user>@<realm>!<tokenid>=<secret> (без префикса)
	Username    string // user@realm для ticket-auth
	Password    string
	Realm       string
	InsecureTLS bool
}

const (
	credEndpoint    = "endpoint"
	credToken       = "token"
	credUsername    = "username"
	credPassword    = "password"
	credRealm       = "realm"
	credInsecureTLS = "insecure_tls"
)

// credsFromMap извлекает [pveCredentials] из decoded credentials-Struct.
func credsFromMap(m map[string]any) pveCredentials {
	c := pveCredentials{
		Endpoint: stringField(m, credEndpoint),
		Token:    stringField(m, credToken),
		Username: stringField(m, credUsername),
		Password: stringField(m, credPassword),
		Realm:    stringField(m, credRealm),
	}
	if m != nil {
		if v, ok := m[credInsecureTLS].(bool); ok {
			c.InsecureTLS = v
		}
	}
	return c
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// resolveAuth выбирает XOR-ветку auth (token / ticket). Возвращает ошибку,
// если не указано ни одной из форм либо указано обе.
func resolveAuth(c pveCredentials) error {
	hasToken := c.Token != ""
	hasTicket := c.Username != "" && c.Password != "" && c.Realm != ""
	if !hasToken && !hasTicket {
		return errors.New("no credentials provided: one of `token` or (`username`+`password`+`realm`) required")
	}
	if hasToken && hasTicket {
		return errors.New("ambiguous credentials: exactly one of `token` or ticket-based (`username`+`password`+`realm`) must be set")
	}
	if c.Endpoint == "" {
		return errors.New("endpoint is required (https://<host>:8006)")
	}
	return nil
}

// newPveClient конструирует pveAPI из переданных credentials. Static-провайдер
// (не ambient chain): драйвер НЕ должен подхватывать env / token-file хоста.
//
// newPveClient вынесен в переменную, чтобы L0-тесты подменяли его fake-фабрикой
// без поднятия HTTP-клиента (см. driver_test.go).
var newPveClient = func(ctx context.Context, c pveCredentials) (pveAPI, error) {
	if err := resolveAuth(c); err != nil {
		return nil, err
	}
	tr := &http.Transport{
		// MinVersion: TLS 1.2 — базовый floor. TLS 1.3 не выставляем —
		// Proxmox API на старых дистрах может не поддерживать.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureTLS},
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second, // per-request; общий ctx ещё в драйвере
	}
	base := strings.TrimRight(c.Endpoint, "/")
	if !strings.HasSuffix(base, "/api2/json") {
		base += "/api2/json"
	}
	cli := &pveRealClient{
		hc:    hc,
		base:  base,
		creds: c,
	}
	// Для ticket-flow сразу авторизуемся; для token — Authorization-заголовок
	// формируется на каждый запрос (stateless).
	if c.Token == "" {
		if err := cli.login(ctx); err != nil {
			return nil, fmt.Errorf("pve ticket login: %w", err)
		}
	}
	return cli, nil
}

// pveRealClient — реальная реализация pveAPI поверх Proxmox VE REST API v2.
// Документация: https://pve.proxmox.com/pve-docs/api-viewer/.
type pveRealClient struct {
	hc    *http.Client
	base  string // https://pve.example:8006/api2/json
	creds pveCredentials

	// ticket-flow state (пустые для token-auth):
	ticket    string
	csrfToken string
}

// login — POST /access/ticket для ticket-flow. Записывает ticket+csrf, которые
// будут отправляться в Cookie/CSRFPreventionToken последующих запросов.
func (c *pveRealClient) login(ctx context.Context) error {
	form := url.Values{
		"username": []string{c.creds.Username + "@" + c.creds.Realm},
		"password": []string{c.creds.Password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/access/ticket",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var reply struct {
		Data struct {
			Ticket              string `json:"ticket"`
			CSRFPreventionToken string `json:"CSRFPreventionToken"`
		} `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return err
	}
	if reply.Data.Ticket == "" {
		return errors.New("pve /access/ticket: empty ticket in response")
	}
	c.ticket = reply.Data.Ticket
	c.csrfToken = reply.Data.CSRFPreventionToken
	return nil
}

// authHeaders проставляет Authorization (token) или Cookie+CSRF (ticket).
func (c *pveRealClient) authHeaders(req *http.Request) {
	if c.creds.Token != "" {
		req.Header.Set("Authorization", "PVEAPIToken="+c.creds.Token)
		return
	}
	req.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: c.ticket})
	if c.csrfToken != "" && (req.Method == http.MethodPost ||
		req.Method == http.MethodPut || req.Method == http.MethodDelete) {
		req.Header.Set("CSRFPreventionToken", c.csrfToken)
	}
}

// doJSON делает запрос и парсит JSON в out. При ≥400 строит pveHTTPError с
// telmate-style code/message, чтобы classify.go мог разложить по FailClass.
func (c *pveRealClient) doJSON(req *http.Request, out any) error {
	c.authHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &pveHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// pveHTTPError — ошибка с HTTP-кодом, на которую опирается classifyProxmox.
type pveHTTPError struct {
	Status int
	Body   string
}

func (e *pveHTTPError) Error() string {
	return fmt.Sprintf("pve api: status %d: %s", e.Status, e.Body)
}

// CloneVM — POST /nodes/<source>/qemu/<vmid>/clone.
func (c *pveRealClient) CloneVM(ctx context.Context, p CloneParams) (string, error) {
	form := url.Values{
		"newid": []string{strconv.Itoa(p.NewVMID)},
		"name":  []string{p.Name},
		"full":  []string{boolForm(p.FullClone)},
	}
	if p.TargetNode != "" && p.TargetNode != p.SourceNode {
		form.Set("target", p.TargetNode)
	}
	if p.TargetStorage != "" {
		form.Set("storage", p.TargetStorage)
	}
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/clone", c.base, p.SourceNode, p.TemplateVMID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var reply struct {
		Data string `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return "", err
	}
	return reply.Data, nil
}

// NextID — GET /cluster/nextid.
func (c *pveRealClient) NextID(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/cluster/nextid", nil)
	if err != nil {
		return 0, err
	}
	// /cluster/nextid возвращает строку-VMID в .data (Proxmox-конвенция).
	var reply struct {
		Data string `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return 0, err
	}
	return strconv.Atoi(reply.Data)
}

// SetVMConfig — POST /nodes/<node>/qemu/<vmid>/config.
func (c *pveRealClient) SetVMConfig(ctx context.Context, node string, vmid int, fields map[string]string) error {
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/config", c.base, node, vmid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.doJSON(req, nil)
}

// StartVM — POST /nodes/<node>/qemu/<vmid>/status/start.
func (c *pveRealClient) StartVM(ctx context.Context, node string, vmid int) (string, error) {
	return c.postStatus(ctx, node, vmid, "start")
}

// StopVM — POST /nodes/<node>/qemu/<vmid>/status/stop.
func (c *pveRealClient) StopVM(ctx context.Context, node string, vmid int) (string, error) {
	return c.postStatus(ctx, node, vmid, "stop")
}

func (c *pveRealClient) postStatus(ctx context.Context, node string, vmid int, verb string) (string, error) {
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/status/%s", c.base, node, vmid, verb)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	var reply struct {
		Data string `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return "", err
	}
	return reply.Data, nil
}

// DeleteVM — DELETE /nodes/<node>/qemu/<vmid>.
func (c *pveRealClient) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d", c.base, node, vmid)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return "", err
	}
	var reply struct {
		Data string `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return "", err
	}
	return reply.Data, nil
}

// GetVMStatus — GET /nodes/<node>/qemu/<vmid>/status/current.
func (c *pveRealClient) GetVMStatus(ctx context.Context, node string, vmid int) (VMStatus, error) {
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/status/current", c.base, node, vmid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return VMStatus{}, err
	}
	var reply struct {
		Data VMStatus `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return VMStatus{}, err
	}
	reply.Data.Node = node
	return reply.Data, nil
}

// ListClusterVMs — GET /cluster/resources?type=vm.
func (c *pveRealClient) ListClusterVMs(ctx context.Context) ([]ClusterVM, error) {
	endpoint := c.base + "/cluster/resources?type=vm"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Data []ClusterVM `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return nil, err
	}
	return reply.Data, nil
}

// GetGuestAgentInterfaces — POST /nodes/<node>/qemu/<vmid>/agent/network-get-
// interfaces. QEMU guest-agent должен быть установлен в гостевой OS И включён
// в /config (agent=1).
func (c *pveRealClient) GetGuestAgentInterfaces(ctx context.Context, node string, vmid int) (string, error) {
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/agent/network-get-interfaces", c.base, node, vmid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	var reply struct {
		Data struct {
			Result []struct {
				Name        string `json:"name"`
				IPAddresses []struct {
					IPAddressType string `json:"ip-address-type"`
					IPAddress     string `json:"ip-address"`
					Prefix        int    `json:"prefix"`
				} `json:"ip-addresses"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.doJSON(req, &reply); err != nil {
		return "", err
	}
	for _, iface := range reply.Data.Result {
		if iface.Name == "lo" || strings.HasPrefix(iface.Name, "lo:") {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.IPAddressType == "ipv4" && addr.IPAddress != "" && !strings.HasPrefix(addr.IPAddress, "127.") {
				return addr.IPAddress, nil
			}
		}
	}
	return "", nil
}

func boolForm(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
