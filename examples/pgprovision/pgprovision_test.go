package pgprovision_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/examples/pgprovision"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

type recorder struct {
	scripts []string
	infos   []vmlib.SSHInfo
	err     error
}

func (r *recorder) RunScript(_ context.Context, info vmlib.SSHInfo, script string) error {
	r.scripts = append(r.scripts, script)
	r.infos = append(r.infos, info)
	return r.err
}

func setup(t *testing.T) (*vmlib.Service, *vmlibtest.Backend) {
	b := vmlibtest.New(2, "base")
	s, err := vmlib.NewService(b, vmlib.WithSSHProber(func(context.Context, vmlib.SSHInfo) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

func TestBuildProvisionsThenCommitsNewImageAndCleansUp(t *testing.T) {
	s, b := setup(t)
	r := &recorder{}
	img, err := pgprovision.Build(context.Background(), s, s.WaitSSHReady, r, pgprovision.Options{BaseImage: "base", Profile: "small", OutputImage: "postgres"})
	if err != nil || img.ID != "postgres" {
		t.Fatalf("%v %+v", err, img)
	}
	if len(r.scripts) != 1 || !strings.Contains(r.scripts[0], "apt-get") || !strings.Contains(r.scripts[0], "postgresql") {
		t.Fatalf("%q", r.scripts)
	}
	if r.infos[0].ProxyJump() == "" {
		t.Fatal("provisioning must go through the bastion metadata")
	}
	if b.Published != 1 || b.Count() != 0 {
		t.Fatalf("published=%d leases=%d (builder VM must be deleted)", b.Published, b.Count())
	}
}

func TestScriptConfiguresRouteAndDNSFromGuestMetadata(t *testing.T) {
	s, _ := setup(t)
	r := &recorder{}
	if _, err := pgprovision.Build(context.Background(), s, s.WaitSSHReady, r, pgprovision.Options{BaseImage: "base", Profile: "small", OutputImage: "pg"}); err != nil {
		t.Fatal(err)
	}
	script := r.scripts[0]
	g := r.infos[0].Guest
	if g == nil {
		t.Fatal("SSHInfo carries no guest network metadata")
	}
	for _, want := range []string{"ip route replace default via " + g.Gateway + " dev eth0", "nameserver " + g.DNS[0]} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "ip route replace") > strings.Index(script, "apt-get") {
		t.Fatal("network must be configured before apt-get")
	}
}

func TestBuildRefusesWithoutUsableGuestNetworkMetadata(t *testing.T) {
	s, b := setup(t)
	b.GuestDNS = nil // server policy provides no DNS
	r := &recorder{}
	_, err := pgprovision.Build(context.Background(), s, s.WaitSSHReady, r, pgprovision.Options{BaseImage: "base", Profile: "small", OutputImage: "pg"})
	if err == nil || len(r.scripts) != 0 || b.Published != 0 {
		t.Fatalf("%v scripts=%d published=%d", err, len(r.scripts), b.Published)
	}
	if b.Count() != 0 {
		t.Fatal("builder VM leaked")
	}
}

func TestScriptRejectsUnsafeMetadata(t *testing.T) {
	for _, g := range []*vmlib.GuestNetwork{
		nil,
		{Gateway: "10.0.0.1; reboot", DNS: []string{"1.1.1.1"}},
		{Gateway: "10.0.0.1", DNS: []string{"1.1.1.1\nnameserver 6.6.6.6"}},
		{Gateway: "not-an-ip", DNS: []string{"1.1.1.1"}},
		{Gateway: "10.0.0.1", DNS: nil},
	} {
		if _, err := pgprovision.ScriptFor(vmlib.SSHInfo{Guest: g}); err == nil {
			t.Errorf("accepted %+v", g)
		}
	}
}

func TestFailedProvisioningNeverPublishes(t *testing.T) {
	s, b := setup(t)
	r := &recorder{err: errors.New("apt failed")}
	_, err := pgprovision.Build(context.Background(), s, s.WaitSSHReady, r, pgprovision.Options{BaseImage: "base", Profile: "small", OutputImage: "postgres"})
	if err == nil || b.Published != 0 {
		t.Fatalf("%v published=%d", err, b.Published)
	}
	if b.Count() != 0 {
		t.Fatal("builder VM leaked")
	}
}

func TestOptionsAreValidated(t *testing.T) {
	s, _ := setup(t)
	_, err := pgprovision.Build(context.Background(), s, s.WaitSSHReady, &recorder{}, pgprovision.Options{BaseImage: "../x", Profile: "small", OutputImage: "o"})
	if !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("%v", err)
	}
}
