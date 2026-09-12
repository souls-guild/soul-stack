// The NoCloud seed.
//
// cloud-init is not decoration here: user-data is the half of an installation the
// cloud does for you, and a local stand that skipped it would not exercise the
// path that is actually broken. The seed also carries `local-hostname`, which is
// what makes the machine announce a name over DHCP — and that announcement is
// where `sid` comes from.
package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/kdomanski/iso9660"
)

// seedVolumeLabel is what cloud-init's NoCloud datasource searches for. It
// matches on the filesystem label, not on the device, which is why the seed can
// be attached as an ordinary virtio disk.
const seedVolumeLabel = "cidata"

// defaultUserData is used when the step passes none. It is a valid cloud-config
// rather than empty bytes: cloud-init treats an unparseable user-data as a
// failure and leaves the machine without the ssh host keys it generates.
const defaultUserData = "#cloud-config\n"

// buildSeed renders the NoCloud seed ISO for one machine.
//
// ★ The two files must be named `user-data` and `meta-data` exactly. Plain
// ISO9660 would uppercase them into USER_DATA.;1 and cloud-init would find
// nothing; this writer emits them verbatim instead, which is non-conformant and
// is precisely why TestSeedCarriesLowercaseNames pins it. If that ever changes,
// every machine boots without its key and the failure appears as an SSH timeout
// three steps later.
func buildSeed(name, namespace, userdata string) ([]byte, error) {
	if strings.TrimSpace(userdata) == "" {
		userdata = defaultUserData
	}
	fqdn := name
	if namespace != "" {
		fqdn = name + "." + namespace
	}
	metadata := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", fqdn, name)

	w, err := iso9660.NewWriter()
	if err != nil {
		return nil, err
	}
	defer w.Cleanup()

	if err := w.AddFile(strings.NewReader(userdata), "user-data"); err != nil {
		return nil, fmt.Errorf("stage user-data: %w", err)
	}
	if err := w.AddFile(strings.NewReader(metadata), "meta-data"); err != nil {
		return nil, fmt.Errorf("stage meta-data: %w", err)
	}

	var buf bytes.Buffer
	if err := w.WriteTo(&buf, seedVolumeLabel); err != nil {
		return nil, fmt.Errorf("write seed iso: %w", err)
	}
	return buf.Bytes(), nil
}
