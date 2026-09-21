package group_test

import (
	"context"
	osuser "os/user"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/group"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestApply_Present_AlreadyExists(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return &osuser.Group{Name: name, Gid: "200"}, nil
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-existing group")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("unexpected calls: %v", r.Calls)
	}
}

func TestApply_Present_CreatesWithGID(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("groupadd -g 5000 redis", util.Result{ExitCode: 0})
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "gid": float64(5000)}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on create")
	}
}

func TestApply_Present_System(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("groupadd -r redis", util.Result{ExitCode: 0})
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": true}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on system group create")
	}
}

func TestApply_Present_System_WithGID(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("groupadd -r -g 5000 redis", util.Result{ExitCode: 0})
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": true, "gid": float64(5000)}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on system+gid group create")
	}
}

func TestApply_Present_SystemFalse_NoFlag(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("groupadd redis", util.Result{ExitCode: 0})
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": false}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on create")
	}
}

func TestApply_Present_System_AlreadyExists_NoOp(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return &osuser.Group{Name: name, Gid: "200"}, nil
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": true}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-existing system group")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("unexpected calls: %v", r.Calls)
	}
}

func TestValidate_System_WrongType(t *testing.T) {
	m := group.New()
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": "yes"}),
	})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if reply.Ok {
		t.Fatal("Ok=true for non-bool system param")
	}
}

func TestValidate_OptionalParams_OK(t *testing.T) {
	m := group.New()
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": true, "gid": float64(5000)}),
	})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !reply.Ok {
		t.Fatalf("Ok=false for valid optional params: %v", reply.Errors)
	}
}

func TestApply_Absent_NotExists(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for not-existing group absent")
	}
}

func TestApply_Absent_Exists_Removes(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("groupdel redis", util.Result{ExitCode: 0})
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return &osuser.Group{Name: name}, nil
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on delete")
	}
}
