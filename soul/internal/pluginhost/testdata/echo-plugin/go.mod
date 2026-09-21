module github.com/souls-guild/soul-stack/soul/internal/pluginhost/testdata/echo-plugin

go 1.26.4

toolchain go1.26.6

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.1.0-beta.1
	github.com/souls-guild/soul-stack/sdk v0.0.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../../../sdk
)
