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

// pveAPI is the narrow subset of Proxmox VE API v2 (REST) used by the driver.
// Narrowing (instead of *pveRealClient directly) gives L0 unit tests mockability
// without network access: tests provide a fake implementation (see
// driver_test.go).
//
// Signatures are designed for the actual driver operation set (clone from
// template, status, delete, list by tag, guest-agent IP), not full API coverage -
// following the "narrow contract = simple mock" principle.
//
// Important about the vmid paradigm: Proxmox VMID is int and exists only in the
// context of a node (clone is done on a specific node, get/delete too). Therefore
// almost all methods accept a (node, vmid) pair. A "global vmid without node"
// approach is impossible in Proxmox: the same /nodes/<node>/qemu/<vmid> endpoint
// is not shared, and the route 404s for a wrong node.
type pveAPI interface {
	// CloneVM does POST /nodes/<source_node>/qemu/<template_vmid>/clone. Returns
	// task UPID. Source-node may differ from newnode; Proxmox performs live clone
	// between nodes when shared storage is available.
	CloneVM(ctx context.Context, params CloneParams) (taskUPID string, err error)

	// NextID is GET /cluster/nextid: atomically reserves the next free VMID in the
	// cluster. Used when profile.new_vmid_start is not set (or 0).
	NextID(ctx context.Context) (int, error)

	// SetVMConfig is POST /nodes/<node>/qemu/<vmid>/config: sets fields for
	// cloud-init (`ciuser`/`cipassword`/`sshkeys`/`cicustom`/…), tags, name.
	// Fields are passed as a map -> form-encoded body, Proxmox-style.
	SetVMConfig(ctx context.Context, node string, vmid int, fields map[string]string) error

	// StartVM is POST /nodes/<node>/qemu/<vmid>/status/start. Returns UPID.
	StartVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// StopVM is POST /nodes/<node>/qemu/<vmid>/status/stop (forced). Returns UPID.
	// If VM is already stopped/deleted, Proxmox returns 500/"does not exist" (the
	// classifier maps it to not_found/transient).
	StopVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// DeleteVM is DELETE /nodes/<node>/qemu/<vmid>. Returns UPID. It does not fail
	// locally if VM is running; Proxmox itself returns 500, and the driver performs
	// stop->delete.
	DeleteVM(ctx context.Context, node string, vmid int) (taskUPID string, err error)

	// GetVMStatus — GET /nodes/<node>/qemu/<vmid>/status/current.
	GetVMStatus(ctx context.Context, node string, vmid int) (VMStatus, error)

	// ListClusterVMs is GET /cluster/resources?type=vm. Used in Create
	// (idempotent find by tag) and in List. Tag filtering is driver-side: Proxmox
	// API has no server-side tag filter in /cluster/resources.
	ListClusterVMs(ctx context.Context) ([]ClusterVM, error)

	// GetGuestAgentInterfaces — POST /nodes/<node>/qemu/<vmid>/agent/network-
	// get-interfaces. Returns IP of the first non-loopback IPv4 interface. Returns
	// empty string (without error) if guest-agent responded but IP is not assigned
	// yet (DHCP handshake in flight); returns error if guest-agent is not
	// configured/not responding (502/timeout from Proxmox).
	GetGuestAgentInterfaces(ctx context.Context, node string, vmid int) (primaryIP string, err error)
}

// CloneParams are params for the Proxmox clone operation.
type CloneParams struct {
	SourceNode    string // node where template_vmid lives
	TemplateVMID  int    // source template VMID
	NewVMID       int    // new VM VMID (required; take NextID if 0)
	Name          string // new VM hostname (Proxmox /name)
	TargetNode    string // destination node (if different from SourceNode)
	TargetStorage string // storage for full-clone
	FullClone     bool   // true=copy, false=linked-snapshot
}

// VMStatus is the subset of /status/current fields needed by the driver. The
// full Proxmox schema is wider (mem/cpu/uptime/...), but wait/probe needs only
// `status` + qmpstatus + tags + name.
type VMStatus struct {
	Node      string   `json:"-"` // filled by driver, not from API
	VMID      int      `json:"vmid"`
	Status    string   `json:"status"`    // "running"|"stopped"
	QmpStatus string   `json:"qmpstatus"` // "running"|"paused"|... (details Status)
	Name      string   `json:"name"`
	Tags      string   `json:"tags"` // semicolon-separated
	Maxmem    int64    `json:"maxmem"`
	Maxdisk   int64    `json:"maxdisk"`
	Cpus      float64  `json:"cpus"`
	Lock      string   `json:"lock"` // during clone/migrate - "clone"/"migrate"
	Agent     int      `json:"agent"`
	NetIn     int64    `json:"netin"`
	NetOut    int64    `json:"netout"`
	IPS       []string `json:"-"` // not from API; driver fills separately from guest-agent
}

// ClusterVM is an item from /cluster/resources?type=vm. For cluster-resource,
// VMID, node, status, and tags are available without per-node calls.
type ClusterVM struct {
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Status string `json:"status"` // "running"|"stopped"
	Name   string `json:"name"`
	Tags   string `json:"tags"`
	Type   string `json:"type"` // "qemu"|"lxc"; qemu is what we need
}

// pveCredentials are provider credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver
// does NOT call Vault - Keeper has already resolved the secret.
//
// Proxmox VE supports two XOR forms:
//   - API-token (recommended, does not expire): claims in one string
//     `<user>@<realm>!<token-id>=<value>`. Placed in the Authorization header as
//     `PVEAPIToken=<token>`. Sufficient for read-only roles.
//   - Ticket-based: username+password+realm -> POST /access/ticket -> cookie +
//     CSRFPreventionToken. Cookie is valid for 2 hours. Used when the operator
//     has no API-token (legacy / dev).
//
// XOR fields: either token, or username+password+realm. Endpoint is required
// (https://<host>:8006/api2/json). insecure_tls is for self-signed cert (Proxmox
// generates one by default).
type pveCredentials struct {
	Endpoint    string // https://pve.example:8006 (without /api2/json - added here)
	Token       string // PVEAPIToken=<user>@<realm>!<tokenid>=<secret> (without prefix)
	Username    string // user@realm for ticket auth
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

// credsFromMap extracts [pveCredentials] from a decoded credentials Struct.
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

// resolveAuth chooses the XOR auth branch (token / ticket). Returns an error if
// neither form is set or both are set.
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

// newPveClient constructs pveAPI from the supplied credentials. Static provider
// (not ambient chain): the driver must NOT pick up env / token-file from host.
//
// newPveClient is a variable so L0 tests can replace it with a fake factory
// without creating an HTTP client (see driver_test.go).
var newPveClient = func(ctx context.Context, c pveCredentials) (pveAPI, error) {
	if err := resolveAuth(c); err != nil {
		return nil, err
	}
	tr := &http.Transport{
		// MinVersion: TLS 1.2 is the baseline floor. TLS 1.3 is not forced because
		// Proxmox API on older distros may not support it.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureTLS},
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second, // per-request; shared ctx is still in driver
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
	// For ticket-flow, authorize immediately; for token, Authorization header is
	// formed on every request (stateless).
	if c.Token == "" {
		if err := cli.login(ctx); err != nil {
			return nil, fmt.Errorf("pve ticket login: %w", err)
		}
	}
	return cli, nil
}

// pveRealClient is the real pveAPI implementation over Proxmox VE REST API v2.
// Documentation: https://pve.proxmox.com/pve-docs/api-viewer/.
type pveRealClient struct {
	hc    *http.Client
	base  string // https://pve.example:8006/api2/json
	creds pveCredentials

	// ticket-flow state (empty for token-auth):
	ticket    string
	csrfToken string
}

// login is POST /access/ticket for ticket-flow. Stores ticket+csrf that are sent
// in Cookie/CSRFPreventionToken on subsequent requests.
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

// authHeaders sets Authorization (token) or Cookie+CSRF (ticket).
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

// doJSON performs the request and parses JSON into out. For >=400 it builds
// pveHTTPError with telmate-style code/message so classify.go can map FailClass.
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

// pveHTTPError is an error with HTTP code that classifyProxmox relies on.
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
	// /cluster/nextid returns string VMID in .data (Proxmox convention).
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
// interfaces. QEMU guest-agent must be installed in the guest OS AND enabled in
// /config (agent=1).
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
