package main

import (
	"context"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcpInstancesAPI is the narrow subset of the compute Instances client used by the driver.
// Narrowing (instead of *compute.InstancesClient directly) gives
// mockability for offline L0 unit tests: tests inject a fake implementation
// (see driver_test.go).
//
// Signatures match compute.InstancesClient methods by shape, but return
// gcpOperation (interface) instead of *compute.Operation, so fake can return
// a synthetic operation without a zone/region operation client.
type gcpInstancesAPI interface {
	Insert(ctx context.Context, in *computepb.InsertInstanceRequest) (gcpOperation, error)
	Delete(ctx context.Context, in *computepb.DeleteInstanceRequest) (gcpOperation, error)
	Get(ctx context.Context, in *computepb.GetInstanceRequest) (*computepb.Instance, error)
	List(ctx context.Context, in *computepb.ListInstancesRequest) ([]*computepb.Instance, error)
}

// gcpOperation is a narrow handle over long-running compute Operation. It needs
// Wait to block until DONE; in production it wraps *compute.Operation.Wait,
// in tests it is a synchronous fake.
type gcpOperation interface {
	Wait(ctx context.Context) error
}

// gcpCredentials are provider credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver does
// NOT call Vault — Keeper has already resolved the secret for it.
//
// service_account_key is a service-account JSON blob (full JSON file from GCP IAM,
// same as emitted by `gcloud iam service-accounts keys create`). project and
// zone sit next to the key (provider-specific, see docs/keeper/cloud.md →
// Credentials-flow).
type gcpCredentials struct {
	ServiceAccountKey []byte // JSON service-account key
	Project           string
	Zone              string
	Endpoint          string // override base-URL Compute (test/emulator); empty = GCP default
}

// credKeys are credentials-Struct field names (contract with Keeper-side
// CredentialsResolverPG: values from Vault KV + project/zone from Provider registry).
const (
	credServiceAccountKey = "service_account_key"
	credProject           = "project"
	credZone              = "zone"
	credEndpoint          = "endpoint"
)

// credsFromMap extracts [gcpCredentials] from decoded credentials-Struct.
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

// newGcpInstancesClient constructs a real compute client from credentials.
// Static provider (not the default application-default chain): credentials
// come from Keeper explicitly, and the driver MUST NOT pick up ambient environment
// (GOOGLE_APPLICATION_CREDENTIALS / metadata-server / gcloud-config) — one
// driver must not use foreign/host credentials (security).
//
// Kept in a variable so L0 tests can replace it with a fake factory without loading
// the real library (see driver_test.go).
var newGcpInstancesClient = func(ctx context.Context, c gcpCredentials) (gcpInstancesAPI, error) {
	opts := []option.ClientOption{}
	if len(c.ServiceAccountKey) > 0 {
		opts = append(opts, option.WithCredentialsJSON(c.ServiceAccountKey))
	} else {
		// Empty credentials — explicitly disable ambient-auth pickup, otherwise
		// GCP client will use the application-default chain. The "no creds" failure
		// is converted to FailAuth by the classifier.
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

// realInstancesClient is a production wrapper of [*compute.InstancesClient] for
// [gcpInstancesAPI]. Thin adapter: Insert/Delete wrap returned
// *compute.Operation into realOperation; List collects iterator into a slice (simpler
// for us — labels are filtered on the driver side, no page streaming needed).
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

// realOperation is a production wrapper of [*compute.Operation] for [gcpOperation].
type realOperation struct {
	op *compute.Operation
}

func (r *realOperation) Wait(ctx context.Context) error {
	return r.op.Wait(ctx)
}
