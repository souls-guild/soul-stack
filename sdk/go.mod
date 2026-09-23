module github.com/souls-guild/soul-stack/sdk

go 1.26.4

require (
	github.com/souls-guild/soul-stack/proto/plugin v0.1.0-beta.1
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
)

// The replace keeps LOCAL development working against the checkout next door.
// It does NOT reach a consumer: Go ignores a replace in a dependency's go.mod,
// so the `require` above must name a version that really exists — which is why
// it is a tag and not v0.0.0. With v0.0.0 there, `go get` on this module failed
// outright for anyone outside this tree (NIM-799).
replace github.com/souls-guild/soul-stack/proto/plugin => ../proto/plugin
