module github.com/souls-guild/soul-stack/sdk

go 1.26.4

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.0.0
	google.golang.org/grpc v1.82.0
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
)

replace github.com/souls-guild/soul-stack/proto/plugin => ../proto/plugin
