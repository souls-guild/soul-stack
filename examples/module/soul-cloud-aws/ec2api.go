package main

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ec2API is the narrow subset of the EC2 client used by the driver. Narrowing
// (instead of using *ec2.Client directly) gives mockability for offline L0 unit tests:
// tests inject a fake implementation (see driver_test.go). Signatures match
// ec2.Client methods verbatim, so assigning a real client works without an adapter.
type ec2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
}

// awsCredentials are provider credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver does
// NOT call Vault — Keeper has already resolved the secret for it.
//
// region is placed by Keeper in credentials next to access keys (provider-
// specific, see docs/keeper/cloud.md → Credentials-flow). Endpoint is an
// optional override for LocalStack/tests (L2).
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Endpoint        string // override base-URL EC2 (LocalStack); empty = AWS default
}

// credKeys are credentials-Struct field names (contract with Keeper-side
// CredentialsResolverPG: values from Vault KV + region from Provider registry).
const (
	credAccessKeyID     = "access_key_id"
	credSecretAccessKey = "secret_access_key"
	credSessionToken    = "session_token"
	credRegion          = "region"
	credEndpoint        = "endpoint"
)

// credsFromMap extracts [awsCredentials] from decoded credentials-Struct.
func credsFromMap(m map[string]any) awsCredentials {
	return awsCredentials{
		AccessKeyID:     stringField(m, credAccessKeyID),
		SecretAccessKey: stringField(m, credSecretAccessKey),
		SessionToken:    stringField(m, credSessionToken),
		Region:          stringField(m, credRegion),
		Endpoint:        stringField(m, credEndpoint),
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

// newEC2Client constructs an EC2 client from the provided credentials. Static
// provider (not the default chain): credentials come from Keeper explicitly,
// and the driver MUST NOT pick up ambient host environment/IMDS (security:
// one driver must not use foreign/host credentials).
//
// newEC2Client is a variable so L0 tests can replace it with a fake factory
// without loading aws-config (see driver_test.go).
var newEC2Client = func(ctx context.Context, c awsCredentials) (ec2API, error) {
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(c.Region),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			c.AccessKeyID, c.SecretAccessKey, c.SessionToken)),
	)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	}), nil
}
