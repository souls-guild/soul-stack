module github.com/souls-guild/soul-stack/tests/load

go 1.26.4

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/souls-guild/soul-stack/proto v0.0.0
	github.com/souls-guild/soul-stack/shared v0.0.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	cel.dev/expr v0.25.2 // indirect
	github.com/Masterminds/semver/v3 v3.5.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cyphar/filepath-securejoin v0.7.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/google/cel-go v0.29.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/oklog/ulid/v2 v2.1.1 // indirect
	github.com/souls-guild/soul-stack/proto/plugin v0.0.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260615183401-62b3387ff324 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
)

// Proto generation lives in the proto/ module; soul-legion pulls
// FromSoul/FromKeeper/KeeperClient types from it for the fake-Soul stream - exactly like
// tests/e2e/internal/soulstub, which also pulls the capability announcement from
// shared/ (a stub that announces nothing is rejected before dispatch, ADR-0076(i)).
// Project modules imported directly, without keeper/internal/* - Go internal rules.
// shared/ and its transitive project dependency proto/plugin are private modules
// with no proxy publication, so each needs an explicit replace to a local path,
// otherwise `go mod tidy` hits a 404 on sum.golang.org (same as tests/e2e).
replace (
	github.com/souls-guild/soul-stack/proto => ../../proto
	github.com/souls-guild/soul-stack/proto/plugin => ../../proto/plugin
	github.com/souls-guild/soul-stack/shared => ../../shared
)
