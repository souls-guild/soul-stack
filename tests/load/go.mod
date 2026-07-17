module github.com/souls-guild/soul-stack/tests/load

go 1.26.4

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/souls-guild/soul-stack/proto v0.0.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260615183401-62b3387ff324 // indirect
)

// Proto generation lives in the proto/ module; soul-legion pulls
// FromSoul/FromKeeper/KeeperClient types from it for the fake-Soul stream - exactly like
// tests/e2e/internal/soulstub. The only project module imported
// directly (without keeper/internal/* - Go internal rules).
replace github.com/souls-guild/soul-stack/proto => ../../proto
