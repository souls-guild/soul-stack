module github.com/souls-guild/soul-stack/examples/module/vmlocal

go 1.26.4

toolchain go1.26.6

require (
	github.com/digitalocean/go-libvirt v0.0.0-20260814190004-1a83157e1858
	github.com/kdomanski/iso9660 v0.4.0
	github.com/souls-guild/soul-stack/proto/plugin v0.1.0-beta.1
	github.com/souls-guild/soul-stack/sdk v0.0.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
)

replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../../../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../../../sdk
)
