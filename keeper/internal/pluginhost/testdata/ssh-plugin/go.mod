module github.com/souls-guild/soul-stack/keeper/internal/pluginhost/testdata/ssh-plugin

go 1.26.4

toolchain go1.26.6

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.1.0-beta.1
	github.com/souls-guild/soul-stack/sdk v0.0.0
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../../../sdk
)
