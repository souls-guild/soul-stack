package util_test

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestDetectFirewall_PrefersUFW(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127}
	r.On("ufw --version", util.Result{ExitCode: 0})
	r.On("firewall-cmd --version", util.Result{ExitCode: 0})
	if got := util.DetectFirewall(context.Background(), r); got != util.FirewallUFW {
		t.Fatalf("DetectFirewall=%q want ufw", got)
	}
}

func TestDetectFirewall_Firewalld(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127}
	r.On("firewall-cmd --version", util.Result{ExitCode: 0})
	if got := util.DetectFirewall(context.Background(), r); got != util.FirewallFirewalld {
		t.Fatalf("DetectFirewall=%q want firewalld", got)
	}
}

func TestDetectFirewall_Unknown(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127}
	if got := util.DetectFirewall(context.Background(), r); got != util.FirewallUnknown {
		t.Fatalf("DetectFirewall=%q want unknown", got)
	}
}
