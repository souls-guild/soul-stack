package user_test

import (
	"context"
	"errors"
	"fmt"
	osuser "os/user"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/user"
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
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return &osuser.User{Username: name, Uid: "1001", Gid: "1001"}, nil
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	ev := stream.Last()
	if ev.Changed {
		t.Fatal("changed=true for already-existing user")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("unexpected runner calls: %v", r.Calls)
	}
}

func TestApply_Present_CreatesWithFlags(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("useradd -M -u 1001 -s /bin/bash -d /var/redis -G wheel,docker -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":   "redis",
			"uid":    float64(1001),
			"shell":  "/bin/bash",
			"home":   "/var/redis",
			"groups": []any{"wheel", "docker"},
		}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on create")
	}
}

func TestApply_Present_System_PrimaryGroup_AllFlags(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("useradd -M -r -g redis -s /sbin/nologin -G wheel,docker -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":   "redis",
			"system": true,
			"group":  "redis",
			"groups": []any{"wheel", "docker"},
			"shell":  "/sbin/nologin",
		}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatalf("changed=false on system service-account create; calls=%v", r.Calls)
	}
}

func TestApply_Present_PrimaryGroup_NoSystem(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("useradd -M -g redis -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "redis",
			"group": "redis",
		}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatalf("changed=false on primary-group create; calls=%v", r.Calls)
	}
}

func TestApply_Present_BackCompat_NoNewParams(t *testing.T) {
	// Без system/group argv должен остаться прежним (обратная совместимость).
	r := internaltest.NewRunner()
	r.On("useradd -M -u 1001 -s /bin/bash -G wheel -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":   "redis",
			"uid":    float64(1001),
			"shell":  "/bin/bash",
			"groups": []any{"wheel"},
		}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatalf("changed=false on back-compat create; calls=%v", r.Calls)
	}
}

func TestApply_Present_Exists_NewParams_NoReconcile(t *testing.T) {
	// Существующий пользователь + новые params (system/group) → no-op,
	// reconcile НЕ запускается (present-or-create семантика).
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return &osuser.User{Username: name, Uid: "999", Gid: "999"}, nil
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":   "redis",
			"system": true,
			"group":  "redis",
		}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true: new params must not trigger reconcile for existing user")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("unexpected runner calls (reconcile leaked): %v", r.Calls)
	}
}

func TestValidate_System_WrongType(t *testing.T) {
	m := user.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "system": "yes"}),
	})
	if reply.Ok {
		t.Fatal("Ok=true for non-bool system param")
	}
}

func TestValidate_Group_WrongType(t *testing.T) {
	m := user.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "group": float64(5)}),
	})
	if reply.Ok {
		t.Fatal("Ok=true for non-string group param")
	}
}

func TestValidate_OptionalParams_OK(t *testing.T) {
	m := user.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":   "redis",
			"system": true,
			"group":  "redis",
			"groups": []any{"wheel"},
			"shell":  "/sbin/nologin",
			"uid":    float64(1001),
		}),
	})
	if !reply.Ok {
		t.Fatalf("Ok=false for valid optional params: %v", reply.Errors)
	}
}

func TestApply_Present_CreateFails(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("useradd -M -- redis", util.Result{ExitCode: 1, Stderr: "boom"})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false on useradd error")
	}
}

func TestApply_Absent_NotExists(t *testing.T) {
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for not-existing absent")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("unexpected runner calls: %v", r.Calls)
	}
}

func TestApply_Absent_Exists_Removes(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("userdel -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return &osuser.User{Username: name}, nil
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

func TestApply_InvalidUID(t *testing.T) {
	m := &user.Module{
		Runner: internaltest.NewRunner(),
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, errors.New(fmt.Sprintf("unknown %s", name))
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis", "uid": float64(0.5)}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false on non-integer uid")
	}
}

func validateReply(t *testing.T, params map[string]any) *pluginv1.ValidateReply {
	t.Helper()
	reply, err := user.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, params),
	})
	if err != nil {
		t.Fatalf("Validate returned err: %v", err)
	}
	return reply
}

func TestValidate_Name_Invalid(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"leading dash":  "-x",                                // arg-injection: имя-опция
		"space":         "bad name",                          // пробел недопустим
		"slash":         "ev/il",                             // path-разделитель
		"uppercase":     "Redis",                             // NAME_REGEX — нижний регистр
		"leading digit": "1redis",                            // имя не может начинаться с цифры
		"too long":      "u23456789012345678901234567890123", // 33 символа
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			reply := validateReply(t, map[string]any{"name": name})
			if reply.Ok {
				t.Fatalf("Ok=true for invalid name %q", name)
			}
			if len(reply.Errors) == 0 {
				t.Fatal("expected a descriptive error")
			}
		})
	}
}

func TestValidate_Name_Valid(t *testing.T) {
	for _, name := range []string{"redis", "_svc", "app-svc", "app_svc1", "machine$", "u2345678901234567890123456789012"} {
		t.Run(name, func(t *testing.T) {
			reply := validateReply(t, map[string]any{"name": name})
			if !reply.Ok {
				t.Fatalf("Ok=false for valid name %q: %v", name, reply.Errors)
			}
		})
	}
}

func TestValidate_UID_OutOfRange(t *testing.T) {
	for label, uid := range map[string]float64{"negative": -1, "too big": 2147483648} {
		t.Run(label, func(t *testing.T) {
			reply := validateReply(t, map[string]any{"name": "redis", "uid": uid})
			if reply.Ok {
				t.Fatalf("Ok=true for uid out of range (%v)", uid)
			}
		})
	}
}

func TestValidate_UID_BoundsOK(t *testing.T) {
	for _, uid := range []float64{0, 2147483647} {
		reply := validateReply(t, map[string]any{"name": "redis", "uid": uid})
		if !reply.Ok {
			t.Fatalf("Ok=false for in-range uid %v: %v", uid, reply.Errors)
		}
	}
}

func TestValidate_Shell_NotAbsolute(t *testing.T) {
	for label, shell := range map[string]string{"relative": "bin/bash", "leading dash": "-rf"} {
		t.Run(label, func(t *testing.T) {
			reply := validateReply(t, map[string]any{"name": "redis", "shell": shell})
			if reply.Ok {
				t.Fatalf("Ok=true for non-absolute shell %q", shell)
			}
		})
	}
}

func TestValidate_Home_NotAbsolute(t *testing.T) {
	reply := validateReply(t, map[string]any{"name": "redis", "home": "var/redis"})
	if reply.Ok {
		t.Fatal("Ok=true for non-absolute home")
	}
}

func TestValidate_Group_Invalid(t *testing.T) {
	for label, group := range map[string]string{"leading dash": "-g", "slash": "a/b", "uppercase": "Wheel"} {
		t.Run(label, func(t *testing.T) {
			reply := validateReply(t, map[string]any{"name": "redis", "group": group})
			if reply.Ok {
				t.Fatalf("Ok=true for invalid group %q", group)
			}
		})
	}
}

func TestValidate_Groups_Invalid(t *testing.T) {
	reply := validateReply(t, map[string]any{"name": "redis", "groups": []any{"wheel", "-bad"}})
	if reply.Ok {
		t.Fatal("Ok=true for invalid supplementary group")
	}
}

func TestApply_Present_ArgInjectionSeparator(t *testing.T) {
	// `--` обязан стоять перед позиционным name в argv useradd (arg-injection
	// guard, defense-in-depth поверх формат-проверки name).
	r := internaltest.NewRunner()
	r.On("useradd -M -- redis", util.Result{ExitCode: 0})
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatalf("changed=false on create; calls=%v", r.Calls)
	}
	if len(r.Calls) != 1 || r.Calls[0] != "useradd -M -- redis" {
		t.Fatalf("`--` separator missing in useradd argv: %v", r.Calls)
	}
}

func TestApply_Present_InvalidName_Fails(t *testing.T) {
	// Apply без предшествующей Validate: битое имя не должно дойти до useradd.
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
		},
	}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "-rf"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false for injection-shaped name")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("useradd was called for invalid name: %v", r.Calls)
	}
}
