package main

import (
	"context"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcpInstancesAPI — узкое подмножество compute Instances-клиента, которое
// использует драйвер. Сужение (а не *compute.InstancesClient напрямую) даёт
// mockability для L0-unit-тестов без сети: тест подсовывает fake-реализацию
// (см. driver_test.go).
//
// Сигнатуры повторяют compute.InstancesClient-методы по форме, но возвращают
// gcpOperation (interface) вместо *compute.Operation, чтобы fake мог отдавать
// синтетическую operation без zone/region-операционного клиента.
type gcpInstancesAPI interface {
	Insert(ctx context.Context, in *computepb.InsertInstanceRequest) (gcpOperation, error)
	Delete(ctx context.Context, in *computepb.DeleteInstanceRequest) (gcpOperation, error)
	Get(ctx context.Context, in *computepb.GetInstanceRequest) (*computepb.Instance, error)
	List(ctx context.Context, in *computepb.ListInstancesRequest) ([]*computepb.Instance, error)
}

// gcpOperation — узкий handle над long-running compute Operation. Нужен метод
// Wait для блокировки до DONE; в реале — обёртка над *compute.Operation.Wait,
// в тестах — синхронный фейк.
type gcpOperation interface {
	Wait(ctx context.Context) error
}

// gcpCredentials — credentials провайдера, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет за него.
//
// service_account_key — JSON-blob сервис-аккаунта (полный JSON-файл от GCP IAM,
// тот же, что выдают `gcloud iam service-accounts keys create`). project и
// zone лежат рядом с ключом (provider-specific, см. docs/keeper/cloud.md →
// Credentials-flow).
type gcpCredentials struct {
	ServiceAccountKey []byte // JSON service-account key
	Project           string
	Zone              string
	Endpoint          string // override base-URL Compute (test/emulator); пусто = GCP-дефолт
}

// credKeys — имена полей credentials-Struct (контракт с Keeper-side
// CredentialsResolverPG: значения из Vault KV + project/zone из Provider-реестра).
const (
	credServiceAccountKey = "service_account_key"
	credProject           = "project"
	credZone              = "zone"
	credEndpoint          = "endpoint"
)

// credsFromMap извлекает [gcpCredentials] из decoded credentials-Struct.
func credsFromMap(m map[string]any) gcpCredentials {
	c := gcpCredentials{
		Project:  stringField(m, credProject),
		Zone:     stringField(m, credZone),
		Endpoint: stringField(m, credEndpoint),
	}
	if s := stringField(m, credServiceAccountKey); s != "" {
		c.ServiceAccountKey = []byte(s)
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

// newGcpInstancesClient конструирует реальный compute-клиент из credentials.
// Static-провайдер (не дефолтная application-default chain): credentials
// приходят от Keeper-а явно, драйвер НЕ должен подхватывать ambient-окружение
// (GOOGLE_APPLICATION_CREDENTIALS / metadata-server / gcloud-config) — один
// драйвер не должен использовать чужие/host-credentials (безопасность).
//
// Вынесен в переменную, чтобы L0-тесты подменяли fake-фабрикой без поднятия
// реальной библиотеки (см. driver_test.go).
var newGcpInstancesClient = func(ctx context.Context, c gcpCredentials) (gcpInstancesAPI, error) {
	opts := []option.ClientOption{}
	if len(c.ServiceAccountKey) > 0 {
		opts = append(opts, option.WithCredentialsJSON(c.ServiceAccountKey))
	} else {
		// Пустые credentials — явно отключаем подхват ambient-auth, иначе
		// GCP-client пойдёт по application-default chain. Падение «no creds»
		// классификатор переведёт в FailAuth.
		opts = append(opts, option.WithoutAuthentication())
	}
	if c.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.Endpoint))
	}
	cli, err := compute.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &realInstancesClient{cli: cli}, nil
}

// realInstancesClient — production-обёртка [*compute.InstancesClient] под
// [gcpInstancesAPI]. Тонкий адаптер: Insert/Delete заворачивают возвращаемую
// *compute.Operation в realOperation; List собирает iterator в slice (так нам
// проще — мы фильтруем по labels на стороне драйвера, страниц не ждём).
type realInstancesClient struct {
	cli *compute.InstancesClient
}

func (r *realInstancesClient) Insert(ctx context.Context, in *computepb.InsertInstanceRequest) (gcpOperation, error) {
	op, err := r.cli.Insert(ctx, in)
	if err != nil {
		return nil, err
	}
	return &realOperation{op: op}, nil
}

func (r *realInstancesClient) Delete(ctx context.Context, in *computepb.DeleteInstanceRequest) (gcpOperation, error) {
	op, err := r.cli.Delete(ctx, in)
	if err != nil {
		return nil, err
	}
	return &realOperation{op: op}, nil
}

func (r *realInstancesClient) Get(ctx context.Context, in *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return r.cli.Get(ctx, in)
}

func (r *realInstancesClient) List(ctx context.Context, in *computepb.ListInstancesRequest) ([]*computepb.Instance, error) {
	it := r.cli.List(ctx, in)
	var out []*computepb.Instance
	for {
		inst, err := it.Next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
}

// realOperation — production-обёртка [*compute.Operation] под [gcpOperation].
type realOperation struct {
	op *compute.Operation
}

func (r *realOperation) Wait(ctx context.Context) error {
	return r.op.Wait(ctx)
}
