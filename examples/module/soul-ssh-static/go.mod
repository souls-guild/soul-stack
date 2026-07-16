module github.com/souls-guild/soul-stack/examples/module/soul-ssh-static

go 1.26.4

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.0.0
	github.com/souls-guild/soul-stack/sdk v0.0.0
	golang.org/x/crypto v0.53.0
	google.golang.org/grpc v1.81.1
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../sdk
)
