// soul-cloud-openstack — реальный CloudDriver-плагин Soul Stack для OpenStack
// (ADR-016 Фаза 4 cloud parity; тираж по pattern-у soul-cloud-aws).
//
// Собирается в статический бинарь `soul-cloud-openstack`. Keeper-side модуль
// `core.cloud.provisioned` (ADR-017) запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC CloudDriver.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper резолвит секрет из Vault и
// кладёт plain в CreateRequest.credentials / DestroyRequest.credentials; драйвер
// в Vault НЕ ходит. Auth-форма — Keystone v3 password-scoped (auth_url +
// username + password + project + user/project domain), region опционален
// (приватные облака без regions). Cloud-init userdata прокидывается в
// servers.CreateOpts.UserData (gophercloud base64-кодирует сам, в отличие от
// EC2-драйвера).
//
// Shared-каркас (error-таксономия / wait-until-ready / retry-backoff) — из
// sdk/clouddriver, общий для всех драйверов тиража. Provider-specific здесь —
// только вызовы compute/v2/servers и OpenStack-классификатор ошибок
// (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON — profile_schema (JSON Schema draft 2020-12), embedded
// рядом с бинарём. Та же техника, что в soul-cloud-aws — отдельный файл,
// не хардкод в Go (легче держать в синхроне с manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runMetaKey — server.metadata-ключ идемпотентности: значение = идентификатор
// прогона/incarnation (из profile.labels). Имя без двоеточия (некоторые
// OpenStack-инсталляции урезают `:` в metadata-ключах). Повторный Create по
// тому же ключу не плодит дубли — существующие живые VM
// (BUILD/ACTIVE/REBUILD) переиспользуются.
const runMetaKey = "soulstack-run"

// Активные/переходные статусы OpenStack-инстанса, в которых VM считается
// «живой» для идемпотентности и list-фильтрации. Терминальные (ERROR/DELETED/
// SHUTOFF) сюда не входят: их драйвер либо помечает failed (probe-Err), либо
// игнорирует при поиске идемпотент-конфликта.
const (
	statusActive  = "ACTIVE"
	statusBuild   = "BUILD"
	statusRebuild = "REBUILD"
)

// defaultBackoff — фабрика [clouddriver.BackoffConfig] для wait/retry-фаз.
// Вынесена в переменную, чтобы L0-тесты могли подменить (быстрый MaxAttempts,
// короткие задержки) без поднятия таймера 1s→2s→4s. Та же техника, что и
// `newOsClient` (см. osapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// OpenstackDriver — реализация CloudDriver для OpenStack.
type OpenstackDriver struct {
	clouddriver.BaseDriver
}

// Schema публикует embedded profile_schema.
func (o *OpenstackDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	var raw map[string]any
	if err := json.Unmarshal(profileSchemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse embedded schema.json: %w", err)
	}
	s, err := structpb.NewStruct(raw)
	if err != nil {
		return nil, fmt.Errorf("encode profile_schema: %w", err)
	}
	return &pluginv1.SchemaReply{ProfileSchema: s}, nil
}

// Validate не несёт credentials (ValidateProfileRequest), structural-проверки
// здесь; auth — на фазе Create. region НЕ обязательное поле — приватные облака
// без regions запускают Keystone без `RegionName` в catalog-е.
func (o *OpenstackDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "image_id") == "" {
		errs = append(errs, "profile.image_id is required")
	}
	if stringField(p, "flavor_id") == "" {
		errs = append(errs, "profile.flavor_id is required")
	}
	if stringField(p, "network_id") == "" {
		errs = append(errs, "profile.network_id is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile — параметры профиля, разобранные для servers.Create.
type vmProfile struct {
	region           string
	availabilityZone string
	imageID          string
	flavorID         string
	networkID        string
	keyName          string
	securityGroups   []string
	labels           map[string]string
	runLabel         string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		region:           stringField(p, "region"),
		availabilityZone: stringField(p, "availability_zone"),
		imageID:          stringField(p, "image_id"),
		flavorID:         stringField(p, "flavor_id"),
		networkID:        stringField(p, "network_id"),
		keyName:          stringField(p, "key_name"),
		labels:           map[string]string{},
	}
	if sgs, ok := p["security_groups"].([]any); ok {
		for _, sg := range sgs {
			if s, ok := sg.(string); ok {
				prof.securityGroups = append(prof.securityGroups, s)
			}
		}
	}
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runLabel = prof.labels[runMetaKey]
	return prof
}

// Create: проверка идемпотентности по metadata → servers.Create → wait-until-
// ready → финальное событие с VmInfo (fqdn = server.AccessIPv4/floating IP /
// первый адрес из server.Addresses; если ничего нет — Fqdn пустой, attributes
// несут пометку no_address — Keeper-side увидит и решит сам).
func (o *OpenstackDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	if creds.Region == "" {
		creds.Region = prof.region
	}

	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "os-client", err)
	}

	backoff := defaultBackoff()

	// Идемпотентность: если по runLabel уже есть живые VM — переиспользуем,
	// добиваем только недостающие. Без runLabel idempotency-проверку не делаем
	// (некому соотнести прогон).
	var existing []servers.Server
	if prof.runLabel != "" {
		existing, err = o.findByRunLabel(ctx, cli, backoff, prof.runLabel)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyOS, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runLabel),
			})
			return o.finalizeCreate(ctx, cli, stream, backoff, serverIDs(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("servers.Create count=%d flavor=%s image=%s", count, prof.flavorID, prof.imageID),
	}); err != nil {
		return err
	}

	newServers, err := o.createServers(ctx, cli, backoff, prof, req.GetUserdata(), count)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyOS, err), "servers.Create", err)
	}

	allIDs := append(serverIDs(existing), serverIDs(newServers)...)
	return o.finalizeCreate(ctx, cli, stream, backoff, allIDs)
}

// createServers вызывает servers.Create count раз. OpenStack создаёт по одному
// серверу за вызов (нет batch-Create как у EC2.RunInstances). Имя каждого
// сервера — слегка рандомизированный slug по runLabel, чтобы избежать nova-
// конфликта по имени, если конкретное облако его требует уникальным.
func (o *OpenstackDriver) createServers(ctx context.Context, cli osAPI, backoff clouddriver.BackoffConfig, prof vmProfile, userdata string, count int32) ([]servers.Server, error) {
	out := make([]servers.Server, 0, count)
	for i := int32(0); i < count; i++ {
		opts := buildCreateOpts(prof, userdata, i)
		var srv *servers.Server
		err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
			var rerr error
			srv, rerr = cli.CreateServer(ctx, opts)
			return rerr
		})
		if err != nil {
			return out, err
		}
		if srv != nil {
			out = append(out, *srv)
		}
	}
	return out, nil
}

// buildCreateOpts формирует servers.CreateOpts из vmProfile. seq — индекс VM
// в текущем создании (0..count-1), используется для дельты имени.
//
// Userdata: gophercloud servers.CreateOpts кодирует UserData в base64 САМ
// (см. servers.CreateOpts.ToServerCreateMap → base64.StdEncoding.EncodeToString).
// Драйвер кладёт plain []byte cloud-init blob — в отличие от EC2-драйвера,
// где SDK НЕ кодирует и base64 делает author.
func buildCreateOpts(prof vmProfile, userdata string, seq int32) servers.CreateOpts {
	metadata := make(map[string]string, len(prof.labels))
	for k, v := range prof.labels {
		metadata[k] = v
	}
	opts := servers.CreateOpts{
		Name:             vmName(prof.runLabel, seq),
		FlavorRef:        prof.flavorID,
		ImageRef:         prof.imageID,
		AvailabilityZone: prof.availabilityZone,
		Networks:         []servers.Network{{UUID: prof.networkID}},
		SecurityGroups:   prof.securityGroups,
		Metadata:         metadata,
	}
	if userdata != "" {
		opts.UserData = []byte(userdata)
	}
	return opts
}

// vmName — имя инстанса OpenStack: `soul-<runLabel-or-anon>-<seq>`. Без
// runLabel используется time-based-семя, чтобы избежать коллизии между двумя
// независимыми run-ами; в проде оператор обязан задать labels[soulstack-run]
// явно.
func vmName(runLabel string, seq int32) string {
	if runLabel == "" {
		return fmt.Sprintf("soul-anon-%d-%d", time.Now().UnixNano(), seq)
	}
	return fmt.Sprintf("soul-%s-%d", runLabel, seq)
}

// finalizeCreate ждёт готовности VM (ACTIVE + IP) и шлёт финальное событие.
// Anti-orphan: при ctx-cancel/timeout недоехавшие VM попадают в финальное
// событие с failed=true, но с заполненным vm_id — Keeper сможет их Destroy.
func (o *OpenstackDriver) finalizeCreate(ctx context.Context, cli osAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, vmIDs []string) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		srv, perr := cli.GetServer(pctx, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyOS, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		switch srv.Status {
		case statusActive:
			if primaryAddress(srv) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			// ACTIVE без адреса — продолжаем ждать (Neutron мог ещё не привязать
			// порт). Не Ready и не Err: поллер повторит.
			return clouddriver.ProbeResult{}
		case statusBuild, statusRebuild:
			return clouddriver.ProbeResult{}
		default:
			// ERROR / SHUTOFF / DELETED / PAUSED / SUSPENDED / … — terminal.
			return clouddriver.ProbeResult{Err: fmt.Errorf("server %s entered terminal status %q", vmID, srv.Status)}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if srv, derr := cli.GetServer(ctx, wr.VMID); derr == nil {
				vi.Fqdn = preferredFqdn(srv)
				vi.PrimaryIp = primaryAddress(srv)
				vi.Attributes = serverAttributes(srv)
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		// ctx-cancel / deadline: финальное событие НЕСЁТ vm_id всех созданных VM
		// с failed=true (anti-orphan) — Keeper увидит instance-id и сможет Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyOS, waitErr), "wait-until-ready", waitErr),
			Vms:     vms,
			Failed:  true,
		})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: "completed",
		Vms:     vms,
		Failed:  anyFailed,
	})
}

// findByRunLabel перечисляет живые (ACTIVE/BUILD/REBUILD) VM с заданным
// runLabel в текущем project-е. servers.List поддерживает filter по metadata
// через ListOpts.Metadata в более новых API; в качестве переносимого варианта
// фильтруем уже после: вытащить всё и отсеять Python-сайд.
//
// PageSize не задаём — gophercloud Pager сам пейджит. AllTenants=false:
// драйвер живёт в scope одного project-а (по credentials).
func (o *OpenstackDriver) findByRunLabel(ctx context.Context, cli osAPI, backoff clouddriver.BackoffConfig, runLabel string) ([]servers.Server, error) {
	var all []servers.Server
	err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
		var rerr error
		all, rerr = cli.ListServers(ctx, servers.ListOpts{})
		return rerr
	})
	if err != nil {
		return nil, err
	}
	live := make([]servers.Server, 0, len(all))
	for _, s := range all {
		if !isLiveStatus(s.Status) {
			continue
		}
		if s.Metadata[runMetaKey] != runLabel {
			continue
		}
		live = append(live, s)
	}
	return live, nil
}

func isLiveStatus(status string) bool {
	switch status {
	case statusActive, statusBuild, statusRebuild:
		return true
	}
	return false
}

// Destroy: per-vm servers.Delete, стрим per-vm событий. OpenStack DeleteServer
// не батчевый, поэтому идём списком; 404 (not_found) — идемпотент-успех.
func (o *OpenstackDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "os-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, id := range req.GetVmIds() {
		vmID := id
		err := clouddriver.Retry(ctx, backoff, classifyOS, func() error {
			return cli.DeleteServer(ctx, vmID)
		})
		if err == nil {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "deleted"})
			continue
		}
		class := clouddriver.Classify(classifyOS, err)
		if class == clouddriver.FailNotFound {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "already absent"})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{
			VmId:    vmID,
			Message: clouddriver.FailMessage(class, "servers.Delete", err),
			Failed:  true,
		})
	}
	return nil
}

// Status — опрос одной VM (servers.Get). credentials приходят отдельным полем
// StatusRequest.credentials (A-flow, симметрично Create/Destroy).
func (o *OpenstackDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("os-client: %w", err)
	}
	srv, err := cli.GetServer(ctx, req.GetVmId())
	if err != nil {
		return nil, fmt.Errorf("servers.Get %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      srv.Status,
		Attributes: serverAttributes(srv),
	}, nil
}

// List — стрим инвентаря VM в project-е (опц. отфильтрованный по runLabel).
// credentials приходят отдельным полем ListRequest.credentials (A-flow,
// симметрично Create/Destroy/Status).
func (o *OpenstackDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newOsClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("os-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	runLabel := stringField(filter, runMetaKey)

	all, err := cli.ListServers(ctx, servers.ListOpts{})
	if err != nil {
		return fmt.Errorf("servers.List: %w", err)
	}
	for _, s := range all {
		if runLabel != "" && s.Metadata[runMetaKey] != runLabel {
			continue
		}
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       s.ID,
			Fqdn:       preferredFqdn(&s),
			PrimaryIp:  primaryAddress(&s),
			Attributes: serverAttributes(&s),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&OpenstackDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-openstack:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

func serverIDs(ss []servers.Server) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

// preferredFqdn — что использовать как Fqdn для Soul-host-а. OpenStack не
// гарантирует FQDN в API (server.Name — короткое имя, не DNS-name), поэтому
// fqdn-канон такой:
//  1. AccessIPv4 (если задан floating IP) — внешне адресуемое имя/IP;
//  2. primaryAddress() — первый IPv4 из server.Addresses, internal-сеть;
//  3. пусто — Keeper-side примет решение (возможна ошибка provisioned, если
//     IP не появился — задокументировано в manifest profile_schema).
func preferredFqdn(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if s.AccessIPv4 != "" {
		return s.AccessIPv4
	}
	return primaryAddress(s)
}

// primaryAddress — первый IPv4 из server.Addresses. Структура Addresses в
// gophercloud — map[net-name][]Address; здесь нужен plain IPv4, не floating
// (для floating используется AccessIPv4). Сетевое имя выбирается
// детерминированно по сортировке ключей (стабильный inventory вывод).
func primaryAddress(s *servers.Server) string {
	if s == nil || len(s.Addresses) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.Addresses))
	for k := range s.Addresses {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, net := range keys {
		addrs, ok := s.Addresses[net].([]any)
		if !ok {
			continue
		}
		for _, a := range addrs {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			ver, _ := am["version"].(float64)
			if ver != 4 {
				continue
			}
			if addr, ok := am["addr"].(string); ok && addr != "" {
				return addr
			}
		}
	}
	return ""
}

// sortStrings — компактная сортировка (без зависимости от sort.Strings, чтобы
// helpers модуля оставались плоскими; n тут — число сетей у одной VM, обычно
// 1-3, O(n²) безопасно).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func serverAttributes(s *servers.Server) *structpb.Struct {
	if s == nil {
		return nil
	}
	m := map[string]any{
		"status":   s.Status,
		"flavor":   flavorRef(s),
		"image":    imageRef(s),
		"hostname": s.Name,
	}
	if !s.Created.IsZero() {
		m["created_at"] = s.Created.UTC().Format(time.RFC3339)
	}
	if s.AccessIPv4 != "" {
		m["access_ipv4"] = s.AccessIPv4
	}
	if primaryAddress(s) == "" {
		m["no_address"] = true
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return st
}

// flavorRef / imageRef — gophercloud парсит Flavor/Image как map[string]any
// (Nova возвращает либо строковый ID, либо объект с href). Драйвер сводит к
// плоскому "id" для attributes.
func flavorRef(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if id, ok := s.Flavor["id"].(string); ok {
		return id
	}
	return ""
}

func imageRef(s *servers.Server) string {
	if s == nil {
		return ""
	}
	if id, ok := s.Image["id"].(string); ok {
		return id
	}
	return ""
}
