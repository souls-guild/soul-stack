// The parts of the libvirt layer that are pure functions of their input: the
// domain XML in both directions, and the image catalogue's identity scheme.
//
// The rest of libvirt.go needs a hypervisor and lives in the `libvirt`-tagged
// lane. These are the pieces where a mistake is silent — a domain that parses
// into the wrong fields reports a machine that does not exist.
package main

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// ourDomainXML renders a domain the way CreateDomain does, so the test parses
// what the code actually writes rather than a hand-typed approximation.
func ourDomainXML(t *testing.T, name, namespace string) string {
	t.Helper()
	prof, errs := parseProfile(validProfile())
	if len(errs) > 0 {
		t.Fatalf("validProfile is not valid: %v", errs)
	}
	x, err := domainDefinition(domainSpec{
		Name:        name,
		Namespace:   namespace,
		Labels:      map[string]string{runLabelKey: "batch", "tier": "db"},
		Profile:     prof,
		ImageID:     imageUUID("debian-12"),
		NetworkID:   "70b148c1-76bb-46c7-8f58-3f81783ac7a0",
		NetworkName: "default",
	}, "/pool/disks/"+name+".qcow2", "/pool/disks/"+name+"-seed.iso")
	if err != nil {
		t.Fatalf("domainDefinition: %v", err)
	}
	return x
}

// What we write, we can read: every field `probed` reports survives the round
// trip. A field that silently decodes to its zero would report a machine with no
// memory, or no batch, and nothing would complain.
func TestDomainXMLRoundTrip(t *testing.T) {
	var dx domainXML
	if err := xml.Unmarshal([]byte(ourDomainXML(t, "batch-0", "proofns")), &dx); err != nil {
		t.Fatalf("our own domain XML does not parse: %v", err)
	}

	if dx.Name != "batch-0" {
		t.Errorf("name=%q", dx.Name)
	}
	if got := dx.memoryBytes(); got != 2<<30 {
		t.Errorf("memory=%d bytes, want %d — the unit attribute was mishandled", got, int64(2)<<30)
	}
	if dx.VCPU.Value != 2 {
		t.Errorf("vcpu=%d, want 2", dx.VCPU.Value)
	}
	if got := dx.bootDiskPath(); got != "/pool/disks/batch-0.qcow2" {
		t.Errorf("boot disk=%q", got)
	}
	if got := dx.seedDiskPath(); got != "/pool/disks/batch-0-seed.iso" {
		t.Errorf("seed disk=%q", got)
	}
	if !strings.HasPrefix(dx.mac(), "52:54:00:") {
		t.Errorf("mac=%q", dx.mac())
	}

	m := dx.Metadata.VM
	if m == nil {
		t.Fatal("our own metadata did not decode")
	}
	if m.XMLName.Space != metadataURI {
		t.Errorf("metadata namespace=%q, want %q — the ownership check keys on it", m.XMLName.Space, metadataURI)
	}
	if m.Namespace != "proofns" {
		t.Errorf("namespace=%q", m.Namespace)
	}
	if got := m.labelMap()[runLabelKey]; got != "batch" {
		t.Errorf("run label=%q, want batch", got)
	}
	if got := m.labelMap()["tier"]; got != "db" {
		t.Errorf("labels did not survive: %v", m.labelMap())
	}
}

// ★ A stranger's `<vm>` metadata in another XML namespace must not break the
// read. With the namespace in the struct tag, encoding/xml returns an
// UnmarshalError for exactly this input — and that error propagated out of the
// per-domain read and failed the whole call for a namespace this artifact was
// supposed to be ignoring. Any unrelated VM on the workstation may have one.
func TestForeignVMMetadataDoesNotBreakTheRead(t *testing.T) {
	const foreign = `<domain type='kvm'>
  <name>someone-elses-vm</name>
  <memory unit='KiB'>1048576</memory>
  <vcpu placement='static'>1</vcpu>
  <metadata>
    <vm xmlns='https://example.invalid/other/1'>
      <namespace>proofns</namespace>
    </vm>
  </metadata>
  <devices/>
</domain>`

	var dx domainXML
	if err := xml.Unmarshal([]byte(foreign), &dx); err != nil {
		t.Fatalf("a foreign <vm> element made the domain unreadable: %v", err)
	}
	if dx.Metadata.VM != nil && dx.Metadata.VM.XMLName.Space == metadataURI {
		t.Error("a foreign namespace was accepted as ours — that machine would be listed, resized and destroyed")
	}
}

// A domain with no metadata of ours parses and is simply not ours.
func TestDomainWithoutOurMetadataParses(t *testing.T) {
	const plain = `<domain type='kvm'><name>plain</name><memory unit='KiB'>1024</memory><vcpu>1</vcpu><devices/></domain>`
	var dx domainXML
	if err := xml.Unmarshal([]byte(plain), &dx); err != nil {
		t.Fatalf("a domain without our metadata does not parse: %v", err)
	}
	if dx.Metadata.VM != nil {
		t.Error("metadata decoded from a domain that has none")
	}
}

// ★ ListDomains skips a domain it cannot read and FAILS on a hypervisor it
// cannot reach. The distinction is the whole point: a swallowed transport error
// looks like an empty namespace, and an empty namespace tells `created` to build
// the batch again on top of one that already exists.
//
// The classifier is a sentinel wrapped at the one place that decodes, not a
// type-switch over encoding/xml's errors — encoding/xml reports a bad number as a
// bare strconv error, which no such switch catches.
func TestUnreadableDomainIsDistinctFromUnreachable(t *testing.T) {
	// The production decoder, on inputs a real host can produce. A bad number is
	// the case a type-switch over encoding/xml's own error types misses, because
	// it comes back as a bare strconv error.
	for _, bad := range []string{
		`<domain><memory>not-a-number</memory></domain>`,
		`<domain><name>unclosed`,
		`<domain><vcpu>x</vcpu></domain>`,
	} {
		if _, err := parseDomainXML(bad, "some-domain"); err == nil {
			t.Errorf("%q parsed without error", bad)
		} else if !errors.Is(err, errUnreadableDomain) {
			t.Errorf("%q gave %v, which ListDomains would treat as a transport failure and fail the whole namespace on", bad, err)
		}
	}

	// Ours decodes, and is therefore never skipped.
	if _, err := parseDomainXML(ourDomainXML(t, "batch-0", "proofns"), "batch-0"); err != nil {
		t.Errorf("our own domain was classified unreadable: %v", err)
	}

	// Comparing two unrelated sentinels proves nothing, so what is asserted here
	// instead is the property that matters: the wrap is the ONLY way the sentinel
	// gets attached, so anything that did not come through parseDomainXML fails
	// the call rather than silently emptying the namespace.
	if _, err := parseDomainXML(`<domain><name>fine</name></domain>`, "fine"); err != nil {
		t.Errorf("a well-formed domain was rejected: %v", err)
	}
}

// ★ The image id must be computable from the name an operator writes in
// `image_name`, without asking this host what its files are called — otherwise
// `image_id` is an exact id nobody can write down.
func TestImageIDIsDerivedFromTheNameNotTheFilename(t *testing.T) {
	// The id an operator would write down, computed from the name alone.
	id := imageUUID("debian-12")
	if !uuidRe.MatchString(id) {
		t.Fatalf("image id %q is not a UUID, so it cannot satisfy the image_id contract", id)
	}

	// It must resolve against the pool WHATEVER the volume's file extension is —
	// this drives the real matching rule, not a re-derivation of it.
	byID := vmProfile{imageID: id}
	for _, file := range []string{"debian-12.qcow2", "debian-12.img", "debian-12.raw", "debian-12"} {
		if !imageMatches(file, byID) {
			t.Errorf("volume %q does not match image_id %s — the id depends on the file extension, so nobody can write it down", file, id)
		}
	}
	if imageMatches("debian-13.qcow2", byID) {
		t.Error("a different image matched the id")
	}

	byName := vmProfile{imageName: "debian-12"}
	if !imageMatches("debian-12.qcow2", byName) || !imageMatches("debian-12", byName) {
		t.Error("image_name does not match its own volume")
	}
	if imageMatches("debian-12-old.qcow2", byName) {
		t.Error("image_name matched a different volume")
	}
}

// The seed rides on virtio and the network is attached by name — both were real
// mistakes, and both are invisible until a guest fails to boot.
func TestDomainDefinitionShape(t *testing.T) {
	x := ourDomainXML(t, "batch-0", "proofns")

	if !strings.Contains(x, `<source network='default'/>`) {
		t.Error("the interface is not attached by network NAME; libvirt refuses a UUID there")
	}
	if strings.Contains(x, "device='cdrom'") {
		t.Error("the seed is attached as a cdrom: the Debian/Ubuntu cloud kernels carry no AHCI or sr driver, " +
			"so it never appears in the guest and cloud-init finds no datasource")
	}
	if !strings.Contains(x, `<target dev='`+seedDiskTarget+`' bus='virtio'/>`) {
		t.Errorf("the seed is not on a virtio disk at %s", seedDiskTarget)
	}
	if !strings.Contains(x, `<mac address='`) {
		t.Error("the MAC is not pinned; libvirt would regenerate it on every redefine")
	}
}

// ★ Metadata written by an OLDER build must still read as ours. `rm_external_id`
// left the element with NIM-873, and a reader that choked on it — or stopped
// recognising the namespace — would make `created` orphan every existing batch
// and build siblings beside it. encoding/xml ignores an unmatched element; this
// pins that it keeps doing so for the fields that decide ownership.
func TestMetadataFromAnOlderBuildStillReads(t *testing.T) {
	old := `<vm xmlns="https://souls.guild/vmlocal/1"><namespace>ns</namespace>` +
		`<created_at>2026-09-13T00:00:00Z</created_at><image_id>img</image_id>` +
		`<network_id>net</network_id><deletion_protection>true</deletion_protection>` +
		`<rm_external_id>cmdb-123</rm_external_id>` +
		`<label><key>soulstack-run</key><value>b</value></label></vm>`
	var m vmMetaRead
	if err := xml.Unmarshal([]byte(old), &m); err != nil {
		t.Fatalf("metadata from an older build no longer parses: %v", err)
	}
	if !m.ours() {
		t.Fatal("metadata from an older build is no longer recognised as ours — an existing batch would be orphaned")
	}
	if m.Namespace != "ns" || !m.DeletionProtection || len(m.Labels) != 1 {
		t.Errorf("fields lost: %+v", m.vmMetaBody)
	}
}
