module github.com/souls-guild/soul-stack/keeper/internal/pluginhost/testdata/ssh-plugin

go 1.26.4

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.0.0
	github.com/souls-guild/soul-stack/sdk v0.0.0
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.81.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../../../sdk
)
