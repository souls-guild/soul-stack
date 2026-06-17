package migrate

import (
	"strings"
	"testing"
)

func TestToMigrateURL_PostgresScheme(t *testing.T) {
	got, err := toMigrateURL("postgres://keeper:keeper@localhost:5432/keeper?sslmode=disable")
	if err != nil {
		t.Fatalf("toMigrateURL: %v", err)
	}
	if got != "pgx5://keeper:keeper@localhost:5432/keeper?sslmode=disable" {
		t.Errorf("got %q", got)
	}
}

func TestToMigrateURL_PostgresqlScheme(t *testing.T) {
	got, err := toMigrateURL("postgresql://k:k@h:5432/db")
	if err != nil {
		t.Fatalf("toMigrateURL: %v", err)
	}
	if got != "pgx5://k:k@h:5432/db" {
		t.Errorf("got %q", got)
	}
}

func TestToMigrateURL_AlreadyPGX5(t *testing.T) {
	got, err := toMigrateURL("pgx5://k:k@h/db")
	if err != nil {
		t.Fatalf("toMigrateURL: %v", err)
	}
	if got != "pgx5://k:k@h/db" {
		t.Errorf("got %q", got)
	}
}

func TestToMigrateURL_RejectsUnsupportedScheme(t *testing.T) {
	_, err := toMigrateURL("host=localhost user=keeper password=keeper dbname=keeper")
	if err == nil {
		t.Fatal("toMigrateURL with keyvalue DSN returned nil err")
	}
	if !strings.Contains(err.Error(), "must be postgres://") {
		t.Errorf("err = %v, want hint about schemes", err)
	}
}
