// Guards on the NoCloud seed, which is the half of an installation the cloud
// normally does and the reason a local stand tests the real path.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// ★ THE FRAGILE PROPERTY, pinned.
//
// cloud-init's NoCloud datasource opens `user-data` and `meta-data` by those exact
// names. Conformant ISO9660 would store them as USER_DATA.;1 and META_DATA.;1 —
// this writer emits lowercase names with a hyphen instead, which the spec does not
// allow and the kernel's iso9660 driver accepts.
//
// Verified end to end on 2026-09-12: a Debian 12 genericcloud guest booted from a
// seed built by this function, ran cloud-init-local, applied the key and announced
// its hostname over DHCP. Before that fix the guest came up as `localhost` with
// ssh.service failed — and the symptom appears three steps later as an SSH
// timeout, with nothing pointing back here.
//
// Reading the bytes back with the same library would prove nothing: its reader is
// lenient about its own writer's naming. This asserts on the raw image.
func TestSeedCarriesLowercaseNames(t *testing.T) {
	iso, err := buildSeed("batch-0", "proofns", "#cloud-config\n")
	if err != nil {
		t.Fatalf("buildSeed: %v", err)
	}
	for _, name := range []string{"user-data", "meta-data"} {
		if !bytes.Contains(iso, []byte(name)) {
			t.Errorf("the raw image does not contain %q — cloud-init will find no datasource "+
				"and every machine boots without its key, as `localhost`", name)
		}
	}
	for _, wrong := range []string{"USER_DATA", "META_DATA"} {
		if bytes.Contains(iso, []byte(wrong)) {
			t.Errorf("the raw image contains %q: the names were uppercased into conformant "+
				"ISO9660 and cloud-init will not match them", wrong)
		}
	}
}

// The volume label is what the datasource matches on — not the device, which is
// why the seed can ride on a plain virtio disk.
func TestSeedIsLabelledCidata(t *testing.T) {
	iso, err := buildSeed("batch-0", "proofns", "")
	if err != nil {
		t.Fatalf("buildSeed: %v", err)
	}
	if !bytes.Contains(iso, []byte(strings.ToUpper(seedVolumeLabel))) &&
		!bytes.Contains(iso, []byte(seedVolumeLabel)) {
		t.Errorf("the image is not labelled %q, so NoCloud will not find it", seedVolumeLabel)
	}
}

// ★ meta-data carries local-hostname, and that is load-bearing rather than
// cosmetic: it is what the guest announces over DHCP, and the announcement is
// where `sid` comes from.
func TestSeedSetsTheHostnameSidIsDerivedFrom(t *testing.T) {
	iso, err := buildSeed("batch-0", "proofns", "")
	if err != nil {
		t.Fatalf("buildSeed: %v", err)
	}
	if !bytes.Contains(iso, []byte("local-hostname: batch-0")) {
		t.Error("meta-data does not set local-hostname — the guest announces nothing over DHCP and sid falls back to a guess")
	}
	if !bytes.Contains(iso, []byte("instance-id: batch-0.proofns")) {
		t.Error("meta-data does not carry a namespaced instance-id")
	}
}

// An empty userdata still has to be a valid cloud-config: cloud-init treats
// unparseable user-data as a failure and leaves the machine without the ssh host
// keys it generates.
func TestEmptyUserdataBecomesAValidCloudConfig(t *testing.T) {
	iso, err := buildSeed("batch-0", "proofns", "   ")
	if err != nil {
		t.Fatalf("buildSeed: %v", err)
	}
	if !bytes.Contains(iso, []byte("#cloud-config")) {
		t.Error("blank userdata did not become a valid cloud-config document")
	}
}

func TestOperatorUserdataIsCarriedVerbatim(t *testing.T) {
	ud := "#cloud-config\npackages:\n  - redis-server\n"
	iso, err := buildSeed("batch-0", "proofns", ud)
	if err != nil {
		t.Fatalf("buildSeed: %v", err)
	}
	if !bytes.Contains(iso, []byte("redis-server")) {
		t.Error("the operator's user-data did not reach the image")
	}
}

// ★ The MAC is a function of the machine's identity: STABLE for one machine
// across redefines, DISTINCT between machines. Both halves are load-bearing.
//
// Stable, because libvirt generates a fresh MAC on every redefine when the
// element is absent, silently breaking the lease lookup sid and primary_ip depend
// on — on a machine that is otherwise running fine.
//
// Distinct, because dnsmasq keeps a lease for its full TTL after a machine is
// gone. Two machines sharing a MAC inherit each other's lease, and `created`
// reports the new one ready, at the old address, before it has booted.
func TestMACIsAFunctionOfMachineIdentity(t *testing.T) {
	id, err := randomUUID()
	if err != nil {
		t.Fatalf("randomUUID: %v", err)
	}
	first, err := macFromUUID(id)
	if err != nil {
		t.Fatalf("macFromUUID: %v", err)
	}
	second, err := macFromUUID(id)
	if err != nil {
		t.Fatalf("macFromUUID: %v", err)
	}
	if first != second {
		t.Fatalf("one identity gave two MACs: %s then %s — a redefine would lose the lease", first, second)
	}
	if !strings.HasPrefix(first, "52:54:00:") {
		t.Errorf("mac=%s, want the QEMU-assigned range", first)
	}

	seen := map[string]bool{first: true}
	for i := 0; i < 200; i++ {
		other, err := randomUUID()
		if err != nil {
			t.Fatalf("randomUUID: %v", err)
		}
		mac, err := macFromUUID(other)
		if err != nil {
			t.Fatalf("macFromUUID: %v", err)
		}
		if seen[mac] {
			t.Fatalf("two machines derived the same MAC (%s) within %d draws — they would inherit each other's lease", mac, i+1)
		}
		seen[mac] = true
	}
}

// ★ A recreated machine is a DIFFERENT machine and gets a different vm_id, which
// is the cloud's semantics — and is what stops a stale dnsmasq lease from
// reporting it ready before it has booted.
func TestEachCreationMintsAFreshIdentity(t *testing.T) {
	prof, errs := parseProfile(validProfile())
	if len(errs) > 0 {
		t.Fatalf("validProfile is not valid: %v", errs)
	}
	spec := domainSpec{
		Name: "batch-0", Namespace: "proofns",
		Profile: prof, NetworkName: "default",
	}

	first, err := domainDefinition(spec, "/pool/a.qcow2", "/pool/a-seed.iso")
	if err != nil {
		t.Fatalf("domainDefinition: %v", err)
	}
	second, err := domainDefinition(spec, "/pool/a.qcow2", "/pool/a-seed.iso")
	if err != nil {
		t.Fatalf("domainDefinition: %v", err)
	}
	if uuidOf(first) == uuidOf(second) {
		t.Errorf("two creations under the same name reused identity %s: the second machine would inherit "+
			"the first's DHCP lease and be reported ready, at the old address, about a second after being defined",
			uuidOf(first))
	}
}

func uuidOf(domainXML string) string {
	const open, close = "<uuid>", "</uuid>"
	i := strings.Index(domainXML, open)
	j := strings.Index(domainXML, close)
	if i < 0 || j < 0 {
		return ""
	}
	return domainXML[i+len(open) : j]
}
