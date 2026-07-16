package main

import (
	"context"
	"fmt"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	"google.golang.org/grpc"
)

// ycAPI is the narrow subset of the yandex-cloud SDK used by the driver.
// Narrowing it (instead of using *ycsdk.SDK directly) gives mockability for L0
// unit tests without network: tests provide a fake implementation (see
// driver_test.go).
//
// WaitOperation is a separate method: yandex-cloud operations are returned as
// *operation.Operation with their own Wait(ctx). To let fake simulate instant
// completion (or an error), Wait is moved into the interface. The real
// implementation simply delegates to op.Wait and unwraps op.Response().
type ycAPI interface {
	CreateInstance(ctx context.Context, in *computev1.CreateInstanceRequest, opts ...grpc.CallOption) (*computev1.Instance, error)
	GetInstance(ctx context.Context, id string, opts ...grpc.CallOption) (*computev1.Instance, error)
	DeleteInstance(ctx context.Context, id string, opts ...grpc.CallOption) error
	ListInstances(ctx context.Context, in *computev1.ListInstancesRequest, opts ...grpc.CallOption) (*computev1.ListInstancesResponse, error)
}

// ycCredentials are provider credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver
// does not call Vault; Keeper has already resolved the secret.
//
// Yandex Cloud supports three authentication forms (the "authorization" docs):
//   - IAMToken is a short-lived (<=12h) bearer token issued by
//     `yc iam create-token` or the ServiceAccountKey flow;
//   - OAuthToken is a user Yandex Passport token;
//   - ServiceAccountKey is a service-account JSON key file
//     (id+private_key+service_account_id+key_algorithm).
//
// XOR fields: exactly one of iam_token / oauth_token / service_account_key must
// be non-empty. FolderID / Zone are provider-specific metadata placed next to
// auth fields for symmetry with awsCredentials.Region.
type ycCredentials struct {
	IAMToken          string
	OAuthToken        string
	ServiceAccountKey []byte // JSON bytes of iamkey.Key
	FolderID          string
	Zone              string
	Endpoint          string // base endpoint override for tests; empty = YC default
}

// credKeys are credentials Struct field names (contract with the Keeper-side
// CredentialsResolverPG: values from Vault KV plus folder_id/zone from the
// Provider registry).
const (
	credIAMToken          = "iam_token"
	credOAuthToken        = "oauth_token"
	credServiceAccountKey = "service_account_key"
	credFolderID          = "folder_id"
	credZone              = "zone"
	credEndpoint          = "endpoint"
)

// credsFromMap extracts [ycCredentials] from the decoded credentials Struct.
// service_account_key is accepted either as a string (JSON blob) or as an
// already decoded object (map[string]any marshaled back to JSON).
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
		// Map/struct variants will be re-encoded as JSON; unsupported types are
		// ignored (the validate phase returns "no credentials provided").
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

// resolveCredentials selects the XOR auth branch (IAM-token / OAuth / SA-key)
// and builds ycsdk.Credentials. It returns an error if none of the three forms
// is specified or more than one is specified.
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

// newYcClient builds ycAPI from the passed credentials. Static provider, not
// the default chain: credentials come explicitly from Keeper, and the driver
// must not pick up the host ambient environment / IAM-token.
//
// newYcClient is a variable so L0 tests can replace it with a fake factory
// without bringing up yc-config (see driver_test.go).
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

// ycRealClient is the real ycAPI implementation on top of *ycsdk.SDK. It
// delegates to Compute().Instance() and unwraps Operation: for Create it returns
// *Instance from response, for Delete it ignores the empty response.
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
