module github.com/souls-guild/soul-stack/tests/load

go 1.26.4

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/souls-guild/soul-stack/proto v0.0.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
)

// Proto-генерация лежит в proto/-модуле; soul-legion тащит из него типы
// FromSoul/FromKeeper/KeeperClient для fake-Soul-стрима — ровно как
// tests/e2e/internal/soulstub. Единственный проектный модуль, импортируемый
// напрямую (без keeper/internal/* — Go-internal-rules).
replace github.com/souls-guild/soul-stack/proto => ../../proto
