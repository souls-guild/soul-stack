package main

import (
	"context"
	"fmt"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	"google.golang.org/grpc"
)

// ycAPI — узкое подмножество yandex-cloud SDK, которое использует драйвер.
// Сужение (а не *ycsdk.SDK напрямую) даёт mockability для L0-unit-тестов без
// сети: тест подсовывает fake-реализацию (см. driver_test.go).
//
// WaitOperation — отдельный метод: yandex-cloud-операции возвращаются как
// *operation.Operation, у которого свой Wait(ctx). Чтобы fake мог симулировать
// мгновенное завершение (или ошибку), Wait вынесен в интерфейс. Реальная
// реализация просто делегирует op.Wait и распаковывает op.Response().
type ycAPI interface {
	CreateInstance(ctx context.Context, in *computev1.CreateInstanceRequest, opts ...grpc.CallOption) (*computev1.Instance, error)
	GetInstance(ctx context.Context, id string, opts ...grpc.CallOption) (*computev1.Instance, error)
	DeleteInstance(ctx context.Context, id string, opts ...grpc.CallOption) error
	ListInstances(ctx context.Context, in *computev1.ListInstancesRequest, opts ...grpc.CallOption) (*computev1.ListInstancesResponse, error)
}

// ycCredentials — credentials провайдера, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет.
//
// Yandex Cloud поддерживает три формы аутентификации (стр. документации
// «authorization»):
//   - IAMToken      — короткоживущий (≤12h) bearer-токен, выпускается через
//                     yc iam create-token или ServiceAccountKey-flow;
//   - OAuthToken    — токен Яндекс-паспорта пользователя;
//   - ServiceAccountKey — JSON-key файла сервис-аккаунта (id+private_key+
//                     service_account_id+key_algorithm).
//
// Поля XOR: ровно одно из iam_token / oauth_token / service_account_key должно
// быть непусто. FolderID / Zone — provider-specific метаданные, лежат рядом с
// auth-полями для симметрии с awsCredentials.Region.
type ycCredentials struct {
	IAMToken          string
	OAuthToken        string
	ServiceAccountKey []byte // JSON-байты iamkey.Key
	FolderID          string
	Zone              string
	Endpoint          string // override базового endpoint-а для тестов; пусто = YC-дефолт
}

// credKeys — имена полей credentials-Struct (контракт с Keeper-side
// CredentialsResolverPG: значения из Vault KV + folder_id/zone из Provider-
// реестра).
const (
	credIAMToken          = "iam_token"
	credOAuthToken        = "oauth_token"
	credServiceAccountKey = "service_account_key"
	credFolderID          = "folder_id"
	credZone              = "zone"
	credEndpoint          = "endpoint"
)

// credsFromMap извлекает [ycCredentials] из decoded credentials-Struct.
// service_account_key принимается либо как string (JSON-blob), либо как
// уже декодированный объект (map[string]any → json-marshal обратно).
func credsFromMap(m map[string]any) ycCredentials {
	c := ycCredentials{
		IAMToken:   stringField(m, credIAMToken),
		OAuthToken: stringField(m, credOAuthToken),
		FolderID:   stringField(m, credFolderID),
		Zone:       stringField(m, credZone),
		Endpoint:   stringField(m, credEndpoint),
	}
	if m != nil {
		if v, ok := m[credServiceAccountKey]; ok {
			c.ServiceAccountKey = saKeyBytes(v)
		}
	}
	return c
}

func saKeyBytes(v any) []byte {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []byte(t)
	case []byte:
		return t
	default:
		// map/struct варианты будем перекодировать в JSON; неподдерживаемые
		// типы — игнорим (validate-фаза вернёт «no credentials provided»).
		return nil
	}
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

// resolveCredentials — выбор XOR-ветки auth (IAM-token / OAuth / SA-key) и
// конструирование ycsdk.Credentials. Возвращает ошибку, если не указано ни
// одной из трёх форм либо указано больше одной.
func resolveCredentials(c ycCredentials) (ycsdk.Credentials, error) {
	n := 0
	if c.IAMToken != "" {
		n++
	}
	if c.OAuthToken != "" {
		n++
	}
	if len(c.ServiceAccountKey) > 0 {
		n++
	}
	switch n {
	case 0:
		return nil, fmt.Errorf("no credentials provided: one of iam_token / oauth_token / service_account_key required")
	case 1:
		// ok
	default:
		return nil, fmt.Errorf("ambiguous credentials: exactly one of iam_token / oauth_token / service_account_key must be set")
	}

	switch {
	case c.IAMToken != "":
		return ycsdk.NewIAMTokenCredentials(c.IAMToken), nil
	case c.OAuthToken != "":
		return ycsdk.OAuthToken(c.OAuthToken), nil
	default:
		key, err := iamkey.ReadFromJSONBytes(c.ServiceAccountKey)
		if err != nil {
			return nil, fmt.Errorf("parse service_account_key json: %w", err)
		}
		creds, err := ycsdk.ServiceAccountKey(key)
		if err != nil {
			return nil, fmt.Errorf("build service-account credentials: %w", err)
		}
		return creds, nil
	}
}

// newYcClient конструирует ycAPI из переданных credentials. Static-провайдер
// (не дефолтная chain): credentials приходят от Keeper-а явно, драйвер НЕ
// должен подхватывать ambient-окружение/IAM-token хоста.
//
// newYcClient вынесен в переменную, чтобы L0-тесты подменяли его fake-фабрикой
// без поднятия yc-config (см. driver_test.go).
var newYcClient = func(ctx context.Context, c ycCredentials) (ycAPI, error) {
	creds, err := resolveCredentials(c)
	if err != nil {
		return nil, err
	}
	conf := ycsdk.Config{Credentials: creds}
	if c.Endpoint != "" {
		conf.Endpoint = c.Endpoint
	}
	sdk, err := ycsdk.Build(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("yc-sdk build: %w", err)
	}
	return &ycRealClient{sdk: sdk}, nil
}

// ycRealClient — реальная реализация ycAPI поверх *ycsdk.SDK. Делегирует в
// Compute().Instance() и распаковывает Operation: для Create — возвращает
// *Instance из response, для Delete — игнорирует пустой response.
type ycRealClient struct {
	sdk *ycsdk.SDK
}

func (c *ycRealClient) CreateInstance(ctx context.Context, in *computev1.CreateInstanceRequest, opts ...grpc.CallOption) (*computev1.Instance, error) {
	op, err := c.sdk.WrapOperation(c.sdk.Compute().Instance().Create(ctx, in, opts...))
	if err != nil {
		return nil, err
	}
	if err := op.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := op.Response()
	if err != nil {
		return nil, err
	}
	inst, ok := resp.(*computev1.Instance)
	if !ok {
		return nil, fmt.Errorf("unexpected Create response type %T", resp)
	}
	return inst, nil
}

func (c *ycRealClient) GetInstance(ctx context.Context, id string, opts ...grpc.CallOption) (*computev1.Instance, error) {
	return c.sdk.Compute().Instance().Get(ctx, &computev1.GetInstanceRequest{
		InstanceId: id,
		View:       computev1.InstanceView_FULL,
	}, opts...)
}

func (c *ycRealClient) DeleteInstance(ctx context.Context, id string, opts ...grpc.CallOption) error {
	op, err := c.sdk.WrapOperation(c.sdk.Compute().Instance().Delete(ctx, &computev1.DeleteInstanceRequest{
		InstanceId: id,
	}, opts...))
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (c *ycRealClient) ListInstances(ctx context.Context, in *computev1.ListInstancesRequest, opts ...grpc.CallOption) (*computev1.ListInstancesResponse, error) {
	return c.sdk.Compute().Instance().List(ctx, in, opts...)
}
