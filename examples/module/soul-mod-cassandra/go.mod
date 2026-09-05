module github.com/souls-guild/soul-stack/examples/module/soul-mod-cassandra

go 1.26.4

toolchain go1.26.6

require (
	github.com/apache/cassandra-gocql-driver/v2 v2.1.2
	github.com/souls-guild/soul-stack/proto/plugin v0.1.0-beta.1
	github.com/souls-guild/soul-stack/sdk v0.0.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../sdk
)
