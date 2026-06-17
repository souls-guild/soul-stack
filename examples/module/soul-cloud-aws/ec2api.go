package main

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ec2API — узкое подмножество ec2-клиента, которое использует драйвер. Сужение
// (а не *ec2.Client напрямую) даёт mockability для L0-unit-тестов без сети:
// тест подсовывает fake-реализацию (см. driver_test.go). Сигнатуры дословно
// повторяют ec2.Client-методы — присвоение реального клиента проходит без
// адаптера.
type ec2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
}

// awsCredentials — credentials провайдера, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет за него.
//
// region кладётся Keeper-ом в credentials рядом с access-ключами (provider-
// specific, см. docs/keeper/cloud.md → Credentials-flow). Endpoint —
// опциональный override для LocalStack/тестов (L2).
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Endpoint        string // override base-URL EC2 (LocalStack); пусто = AWS-дефолт
}

// credKeys — имена полей credentials-Struct (контракт с Keeper-side
// CredentialsResolverPG: значения из Vault KV + region из Provider-реестра).
const (
	credAccessKeyID     = "access_key_id"
	credSecretAccessKey = "secret_access_key"
	credSessionToken    = "session_token"
	credRegion          = "region"
	credEndpoint        = "endpoint"
)

// credsFromMap извлекает [awsCredentials] из decoded credentials-Struct.
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

// newEC2Client конструирует ec2-клиент из переданных credentials. Static-
// провайдер (не дефолтная chain): credentials приходят от Keeper-а явно,
// драйвер НЕ должен подхватывать ambient-окружение/IMDS хоста (безопасность:
// один драйвер не должен использовать чужие/host-credentials).
//
// newEC2Client вынесен в переменную, чтобы L0-тесты подменяли его fake-фабрикой
// без поднятия aws-config (см. driver_test.go).
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
