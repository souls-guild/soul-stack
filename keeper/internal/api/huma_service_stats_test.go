package api

import (
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func TestNewServiceStatsReply_WireShape(t *testing.T) {
	got, err := json.Marshal(newServiceStatsReply([]handlers.ServiceStatsView{
		{Service: "redis", Incarnations: 3, Hosts: 8},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"by_service":[{"service":"redis","incarnations":3,"hosts":8}]}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}

	empty, _ := json.Marshal(newServiceStatsReply([]handlers.ServiceStatsView{}))
	if string(empty) != `{"by_service":[]}` {
		t.Errorf("empty aggregate = %s, want an empty array, not null", empty)
	}
}
