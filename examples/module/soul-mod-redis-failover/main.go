// Скелет custom-модуля Soul Stack на Go.
//
// Соберётся в один статический бинарь `soul-mod-redis-failover` через `go build`.
// Soul при apply Destiny-шага вида `module: wb.redis-failover.promoted` запустит
// этот бинарь как sub-process, сделает gRPC-stdio handshake (см. sdk/handshake)
// и будет звать RPC-методы SoulModule.
//
// Это иллюстрация — для production-кода добавьте резолв vault-ссылок, идемпотентность,
// OTel-трассировки шагов и реальную работу с redis-cli.

package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// RedisFailover — реализация SoulModule для state `promoted` (switchover Redis-replica).
//
// BaseModule даёт no-op-дефолты Validate/Plan; здесь переопределяем все три RPC,
// чтобы показать типовой набор шагов.
type RedisFailover struct {
	module.BaseModule
}

// Validate — runtime-проверки параметров поверх statических от soul-lint.
func (r *RedisFailover) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	// TODO: убедиться, что new_master_sid не равен текущему master-у; что replica
	// с таким SID есть в кластере; что vault-ссылка password резолвится.
	_ = req
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Plan — dry-run: какие шаги будут выполнены.
func (r *RedisFailover) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	newMaster := paramString(req.Params, "new_master_sid")
	for _, msg := range []string{
		"demote-current-master: redis-cli REPLICAOF <new-master> 6379",
		"promote-new-master: redis-cli REPLICAOF NO ONE on " + newMaster,
		"verify: ensure one master and N-1 replicas",
	} {
		if err := stream.Send(&pluginv1.PlanEvent{Message: msg}); err != nil {
			return err
		}
	}
	return nil
}

// Apply — реальная работа со стримингом прогресса. Финальное событие переносит
// changed/failed + output (см. ApplyEvent в proto/plugin/v1/soulmodule.proto).
func (r *RedisFailover) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	password := paramString(req.Params, "password")
	newMaster := paramString(req.Params, "new_master_sid")
	if password == "" || newMaster == "" {
		return fmt.Errorf("missing required params: new_master_sid / password")
	}

	for _, msg := range []string{"demote-current-master", "promote-new-master", "verify"} {
		if err := stream.Send(&pluginv1.ApplyEvent{Message: msg + ": running"}); err != nil {
			return err
		}
		// TODO: реальная работа redis-cli.
		if err := stream.Send(&pluginv1.ApplyEvent{Message: msg + ": ok"}); err != nil {
			return err
		}
	}

	output, _ := structpb.NewStruct(map[string]any{
		"new_master_sid": newMaster,
	})
	return stream.Send(&pluginv1.ApplyEvent{
		Message: "completed",
		Changed: true,
		Output:  output,
	})
}

func paramString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.Fields[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

func main() {
	if err := module.Serve(&RedisFailover{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-redis-failover:", err)
		os.Exit(1)
	}
}
