// The libvirt side of the seam.
//
// go-libvirt speaks libvirt's RPC protocol itself rather than binding libvirt.so,
// so this whole file compiles under CGO_ENABLED=0 and the artifact a Keeper
// fetches is one static binary.
//
// Two storage pools carry the state, and they are libvirt's rather than this
// artifact's: the image catalogue is a pool, so "the list of images" is a real
// libvirt read and the files are owned by the daemon that runs the machines
// instead of by whichever user launched the plugin. Names come from the
// environment, not from params — where the daemon keeps its files is a property of
// the host, and a scenario that had to name it could not move between hosts.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"
)

const (
	metadataURI = "https://souls.guild/vmlocal/1"

	envImagePool = "VMLOCAL_IMAGE_POOL"
	envDiskPool  = "VMLOCAL_DISK_POOL"

	defaultImagePool = "vmlocal-images"
	defaultDiskPool  = "vmlocal-disks"

	// bootDiskTarget is the guest device the boot volume is attached at, and the
	// handle DomainBlockResize takes.
	bootDiskTarget = "vda"
	// seedDiskTarget is where the NoCloud seed rides.
	//
	// ★ It is a virtio DISK and not a SATA cdrom, which is the obvious way to
	// attach an ISO and does not work: the Debian/Ubuntu cloud kernels are built
	// for virtio and carry no AHCI or sr driver, so the cdrom never appears in
	// the guest, cloud-init finds no datasource, and the machine comes up with no
	// ssh key and the hostname `localhost`. cloud-init matches on the filesystem
	// LABEL (`cidata`), not on the device type, so a plain disk is found.
	seedDiskTarget = "vdb"
)

// supportedURISchemes are the libvirt transports this artifact accepts. The list
// exists so that a step pointed at some other provider's endpoint is told so,
// rather than failing later inside a dial with a message about a hostname.
var supportedURISchemes = []string{"qemu", "qemu+unix", "qemu+ssh", "qemu+tcp", "qemu+tls", "test"}

func knownLibvirtURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	for _, s := range supportedURISchemes {
		if u.Scheme == s {
			return true
		}
	}
	return false
}

type libvirtHV struct {
	l         *libvirt.Libvirt
	imagePool string
	diskPool  string
}

func dialLibvirt(endpoint string) (hypervisor, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse %q as a libvirt URI: %w", endpoint, err)
	}
	l, err := libvirt.ConnectToURI(u)
	if err != nil {
		return nil, err
	}
	return &libvirtHV{
		l:         l,
		imagePool: envOr(envImagePool, defaultImagePool),
		diskPool:  envOr(envDiskPool, defaultDiskPool),
	}, nil
}

// imageMatches decides whether a pool volume is the image a profile asked for.
//
// Extracted so that the rule is testable as itself: `image_id` is matched against
// the UUID of the volume's EXTENSION-LESS name, which is the whole reason an
// operator can compute the id from what they wrote in `image_name`.
func imageMatches(volName string, prof vmProfile) bool {
	short := imageNameOf(volName)
	if prof.imageID != "" {
		return imageUUID(short) == prof.imageID
	}
	return prof.imageName != "" && (volName == prof.imageName || short == prof.imageName)
}

// imageNameOf is the catalogue name of a volume: its filename without the disk
// extension. `debian-12.qcow2` and `debian-12.img` are both the image `debian-12`.
func imageNameOf(volName string) string {
	for _, ext := range []string{".qcow2", ".img", ".raw"} {
		if strings.HasSuffix(volName, ext) {
			return strings.TrimSuffix(volName, ext)
		}
	}
	return volName
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (h *libvirtHV) Close() error { return h.l.Disconnect() }

// ── UUID helpers ────────────────────────────────────────────────────────────
// libvirt's wire UUID is 16 raw bytes; everything an operator sees is the dashed
// text form, so the two are converted at this boundary and nowhere else.

func parseUUID(s string) (libvirt.UUID, error) {
	var u libvirt.UUID
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return u, fmt.Errorf("%q is not a UUID", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return u, fmt.Errorf("%q is not a UUID: %w", s, err)
	}
	copy(u[:], b)
	return u, nil
}

func formatUUID(u libvirt.UUID) string {
	s := hex.EncodeToString(u[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// imageUUID derives a stable UUID for a catalogue entry, so that `image_id` can
// name an image exactly rather than by a string a pool might spell two ways.
// Version 5 over the artifact's metadata URI.
//
// ★ It hashes the IMAGE NAME — the volume name with its file extension stripped —
// and not the volume's filename. Hashing the filename would make the id depend on
// whether the pool happens to store `debian-12.qcow2` or `debian-12.img`, which
// breaks the property that makes an exact id usable at all: an operator must be
// able to compute it from the name they already write in `image_name`, without
// first asking this host what its files are called.
func imageUUID(imageName string) string {
	ns := sha1.Sum([]byte(metadataURI))
	sum := sha1.Sum(append(ns[:], []byte(imageName)...))
	var u libvirt.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(u)
}

// ── domain XML ──────────────────────────────────────────────────────────────

// vmMetaBody is this artifact's own metadata: the namespace scope and the batch
// labels. It is the reason `probed` can tell a machine it made from the
// operator's unrelated VM on the same host.
type vmMetaBody struct {
	Namespace          string      `xml:"namespace"`
	CreatedAt          string      `xml:"created_at"`
	ImageID            string      `xml:"image_id"`
	NetworkID          string      `xml:"network_id"`
	DeletionProtection bool        `xml:"deletion_protection"`
	Labels             []metaLabel `xml:"label"`
}

// vmMeta is the element this artifact WRITES.
//
// ★ The namespace is in the STRUCT TAG because that is the only place
// encoding/xml honours it on marshal: a Space set on the XMLName value is
// silently dropped, and the element goes out with no xmlns at all — which would
// leave every machine unrecognisable to the read side, so `probed` would report
// nothing and `created` would rebuild the batch on every run.
type vmMeta struct {
	XMLName xml.Name `xml:"https://souls.guild/vmlocal/1 vm"`
	vmMetaBody
}

// vmMetaRead is the element this artifact READS, and it is a separate type on
// purpose.
//
// ★ XMLName carries NO tag, so it records whatever name the element actually had
// and imposes no namespace constraint. With the namespace in the tag, decoding a
// stranger's `<vm>` in a different namespace is an UnmarshalError — and that
// error came back out of the per-domain read and failed the whole call for a
// namespace this artifact was supposed to be ignoring. Ownership is decided by
// comparing the resolved Space, which also survives libvirt re-serialising the
// element with a prefix declared on an ancestor.
type vmMetaRead struct {
	XMLName xml.Name
	vmMetaBody
}

func (m vmMetaRead) ours() bool { return m.XMLName.Space == metadataURI }

type metaLabel struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

func (m vmMetaBody) labelMap() map[string]string {
	out := make(map[string]string, len(m.Labels))
	for _, l := range m.Labels {
		out[l.Key] = l.Value
	}
	return out
}

type domainXML struct {
	XMLName xml.Name `xml:"domain"`
	Name    string   `xml:"name"`
	UUID    string   `xml:"uuid"`
	Memory  struct {
		Unit  string `xml:"unit,attr"`
		Value uint64 `xml:",chardata"`
	} `xml:"memory"`
	VCPU struct {
		Value int64 `xml:",chardata"`
	} `xml:"vcpu"`
	Metadata struct {
		// Matches the local name `vm` in ANY namespace; [vmMetaRead.ours]
		// decides whether it is this artifact's.
		VM *vmMetaRead `xml:"vm"`
	} `xml:"metadata"`
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
		} `xml:"disk"`
		Interfaces []struct {
			MAC struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Network string `xml:"network,attr"`
			} `xml:"source"`
		} `xml:"interface"`
	} `xml:"devices"`
}

// memoryBytes normalises the unit libvirt answers in. libvirt rewrites whatever
// unit was defined into KiB on read, so trusting the number without the attribute
// would report a machine 1024 times smaller than it is.
func (d domainXML) memoryBytes() int64 {
	switch strings.ToLower(d.Memory.Unit) {
	case "", "kib", "k":
		return int64(d.Memory.Value) * 1024
	case "bytes", "b":
		return int64(d.Memory.Value)
	case "mib", "m":
		return int64(d.Memory.Value) * 1024 * 1024
	case "gib", "g":
		return int64(d.Memory.Value) * 1024 * 1024 * 1024
	default:
		return int64(d.Memory.Value) * 1024
	}
}

func (d domainXML) bootDiskPath() string {
	for _, disk := range d.Devices.Disks {
		if disk.Target.Dev == bootDiskTarget {
			return disk.Source.File
		}
	}
	return ""
}

func (d domainXML) seedDiskPath() string {
	for _, disk := range d.Devices.Disks {
		if disk.Target.Dev == seedDiskTarget {
			return disk.Source.File
		}
	}
	return ""
}

func (d domainXML) mac() string {
	if len(d.Devices.Interfaces) == 0 {
		return ""
	}
	return d.Devices.Interfaces[0].MAC.Address
}

// ── reads ───────────────────────────────────────────────────────────────────

func (h *libvirtHV) info(dom libvirt.Domain) (domainInfo, error) {
	raw, err := h.l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return domainInfo{}, err
	}
	dx, err := parseDomainXML(raw, dom.Name)
	if err != nil {
		return domainInfo{}, err
	}
	state, _, _, _, _, err := h.l.DomainGetInfo(dom)
	if err != nil {
		return domainInfo{}, err
	}

	d := domainInfo{
		UUID:        formatUUID(dom.UUID),
		Name:        dx.Name,
		Running:     state == uint8(libvirt.DomainRunning),
		VCPUs:       dx.VCPU.Value,
		MemoryBytes: dx.memoryBytes(),
		MAC:         dx.mac(),
		Labels:      map[string]string{},
	}
	// A `<vm>` element in somebody else's namespace is somebody else's machine.
	if m := dx.Metadata.VM; m != nil && m.ours() {
		d.Namespace = m.Namespace
		d.CreatedAt = m.CreatedAt
		d.ImageID = m.ImageID
		d.NetworkID = m.NetworkID
		d.DeletionProtection = m.DeletionProtection
		d.Labels = m.labelMap()
	}
	if path := dx.bootDiskPath(); path != "" {
		if _, capacity, _, err := h.l.DomainGetBlockInfo(dom, path, 0); err == nil {
			d.DiskBytes = int64(capacity)
		}
	}
	return d, nil
}

// ListDomains returns only the machines this artifact made in this namespace.
// A domain with no metadata of ours is not ours, and is skipped rather than
// reported with empty fields.
//
// ★ A domain this artifact cannot read is SKIPPED, not fatal — but only for the
// two reasons that mean "not ours or not there any more": it vanished between the
// list and the read, or its XML does not parse. Anything else fails the call. The
// distinction matters because a swallowed transport error would look like an
// EMPTY namespace, and an empty namespace tells `created` to build the batch from
// scratch on top of a batch that already exists.
func (h *libvirtHV) ListDomains(namespace string) ([]domainInfo, error) {
	doms, _, err := h.l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}
	out := make([]domainInfo, 0, len(doms))
	for _, dom := range doms {
		d, err := h.info(dom)
		if err != nil {
			if libvirt.IsNotFound(err) || errors.Is(err, errUnreadableDomain) {
				continue
			}
			return nil, err
		}
		if d.Namespace == "" || d.Namespace != namespace {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// errUnreadableDomain marks a domain whose XML this artifact could not decode.
// Ours always decode, so such a domain is not ours and is skipped — as opposed to
// a hypervisor that cannot be reached, which must fail the call.
var errUnreadableDomain = errors.New("domain xml is unreadable")

// parseDomainXML decodes a domain document, wrapping any failure in
// [errUnreadableDomain].
//
// ★ The sentinel is applied HERE, at the one place that decodes, rather than by
// classifying the error afterwards: encoding/xml reports a bad number as a bare
// strconv error, so a type-switch over its own error types misses it and the
// caller cannot tell "cannot read this domain" from "cannot reach libvirt". The
// difference decides whether ListDomains skips one machine or fails the call, and
// a swallowed transport error looks like an empty namespace — which tells
// `created` to build the batch again on top of one that already exists.
func parseDomainXML(raw, name string) (domainXML, error) {
	var dx domainXML
	if err := xml.Unmarshal([]byte(raw), &dx); err != nil {
		return domainXML{}, fmt.Errorf("%w: parse domain xml for %s: %v", errUnreadableDomain, name, err)
	}
	return dx, nil
}

func (h *libvirtHV) LookupDomain(uuid string) (domainInfo, error) {
	u, err := parseUUID(uuid)
	if err != nil {
		return domainInfo{}, err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return domainInfo{}, errNoDomain
		}
		return domainInfo{}, err
	}
	d, err := h.info(dom)
	if err != nil {
		// The domain can disappear between the lookup and the read — two RPCs,
		// and a concurrent actor. That is "gone", and a caller polling for its
		// absence must see it as such rather than spinning to its timeout.
		if libvirt.IsNotFound(err) {
			return domainInfo{}, errNoDomain
		}
		return domainInfo{}, err
	}
	return d, nil
}

func (h *libvirtHV) LookupNetwork(id string) (string, string, error) {
	if u, err := parseUUID(id); err == nil {
		n, err := h.l.NetworkLookupByUUID(u)
		if err != nil {
			return "", "", fmt.Errorf("no libvirt network with UUID %s: %w", id, err)
		}
		return n.Name, formatUUID(n.UUID), nil
	}
	n, err := h.l.NetworkLookupByName(id)
	if err != nil {
		return "", "", fmt.Errorf("no libvirt network named %q: %w", id, err)
	}
	return n.Name, formatUUID(n.UUID), nil
}

// Lease is where primary_ip and sid come from: dnsmasq records what the guest
// announced over DHCP, so the answer is the machine's own account of itself
// rather than what this artifact asked for.
func (h *libvirtHV) Lease(_ context.Context, networkID, mac string) (string, string, bool, error) {
	if mac == "" {
		return "", "", false, nil
	}
	u, err := parseUUID(networkID)
	if err != nil {
		return "", "", false, err
	}
	n, err := h.l.NetworkLookupByUUID(u)
	if err != nil {
		return "", "", false, err
	}
	leases, _, err := h.l.NetworkGetDhcpLeases(n, libvirt.OptString{mac}, 1, 0)
	if err != nil {
		return "", "", false, err
	}
	for _, ls := range leases {
		if ls.Ipaddr == "" {
			continue
		}
		hostname := ""
		if len(ls.Hostname) > 0 {
			hostname = ls.Hostname[0]
		}
		return ls.Ipaddr, hostname, true, nil
	}
	return "", "", false, nil
}

// ResolveImage turns image_id or image_name into a backing volume in the image
// pool. A miss names what IS there: an operator holding a cloud image UUID has no
// other way to learn this host's catalogue.
func (h *libvirtHV) ResolveImage(prof vmProfile) (string, string, error) {
	pool, err := h.l.StoragePoolLookupByName(h.imagePool)
	if err != nil {
		return "", "", fmt.Errorf("image pool %q not found (set %s): %w", h.imagePool, envImagePool, err)
	}
	if err := h.l.StoragePoolRefresh(pool, 0); err != nil {
		return "", "", fmt.Errorf("refresh image pool %q: %w", h.imagePool, err)
	}
	vols, _, err := h.l.StoragePoolListAllVolumes(pool, 1, 0)
	if err != nil {
		return "", "", err
	}

	catalogue := make([]string, 0, len(vols))
	for _, v := range vols {
		short := imageNameOf(v.Name)
		catalogue = append(catalogue, fmt.Sprintf("%s=%s", short, imageUUID(short)))

		if !imageMatches(v.Name, prof) {
			continue
		}
		path, err := h.l.StorageVolGetPath(v)
		if err != nil {
			return "", "", err
		}
		// ★ An overlay smaller than the image it sits on gives the guest a
		// TRUNCATED filesystem — qemu allows it and says nothing, and the damage
		// shows up as a corrupt root long after the create reported success. A
		// capacity this artifact cannot read fails the create rather than waiving
		// the check: an unreadable image is not evidence that the size is fine.
		_, capacity, _, ierr := h.l.StorageVolGetInfo(v)
		if ierr != nil {
			return "", "", fmt.Errorf("cannot read the size of image %q, so boot_disk_size cannot be checked against it: %w", short, ierr)
		}
		if prof.bootDiskSize > 0 && uint64(prof.bootDiskSize) < capacity {
			return "", "", fmt.Errorf(
				"profile.boot_disk_size is %d bytes but image %q is %d bytes: the overlay would truncate the image and the guest filesystem with it",
				prof.bootDiskSize, short, capacity)
		}
		return imageUUID(short), path, nil
	}

	sort.Strings(catalogue)
	want := prof.imageName
	if want == "" {
		want = prof.imageID
	}
	return "", "", fmt.Errorf("image %q is not in pool %q; it holds: %s",
		want, h.imagePool, strings.Join(catalogue, ", "))
}

// ── writes ──────────────────────────────────────────────────────────────────

// CreateDomain builds the overlay, uploads the seed, defines the machine and
// starts it.
//
// ★ Every failure path after the first volume exists UNWINDS what it made. The
// volume names are derived from the machine name, so a leaked volume is not just
// waste: the next run regenerates the same name, StorageVolCreateXML answers
// "storage volume already exists", and the batch is wedged until somebody runs
// virsh vol-delete by hand. Leaving a half-made machine behind is the one failure
// this function must not have.
func (h *libvirtHV) CreateDomain(_ context.Context, spec domainSpec) (dom domainInfo, err error) {
	pool, err := h.l.StoragePoolLookupByName(h.diskPool)
	if err != nil {
		return domainInfo{}, fmt.Errorf("disk pool %q not found (set %s): %w", h.diskPool, envDiskPool, err)
	}

	// ★ `committed` is the point past which the machine EXISTS and the unwind must
	// not run. Without it the unwind also fires on a failure of the final read —
	// which happens after the domain is defined, started and running — and deletes
	// the backing files out from under a live machine. The caller then reports the
	// create failed, the id never reaches its output, and the next run adopts a
	// machine whose metadata and lease are intact and which can never boot again.
	var made []libvirt.StorageVol
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		for _, v := range made {
			if derr := h.l.StorageVolDelete(v, 0); derr != nil {
				err = fmt.Errorf("%w (and volume %q could not be cleaned up: %v — "+
					"delete it before retrying, or the next run collides on the name)", err, v.Name, derr)
			}
		}
	}()

	bootName := spec.Profile.bootDiskName
	if bootName == "" {
		bootName = spec.Name + ".qcow2"
	}
	bootVol, err := h.l.StorageVolCreateXML(pool, overlayVolumeXML(bootName, spec.Profile.bootDiskSize, spec.ImagePath), 0)
	if err != nil {
		return domainInfo{}, fmt.Errorf("create boot volume %q: %w", bootName, err)
	}
	made = append(made, bootVol)
	bootPath, err := h.l.StorageVolGetPath(bootVol)
	if err != nil {
		return domainInfo{}, err
	}

	seedName := spec.Name + "-seed.iso"
	seedVol, err := h.l.StorageVolCreateXML(pool, rawVolumeXML(seedName, int64(len(spec.Seed))), 0)
	if err != nil {
		return domainInfo{}, fmt.Errorf("create seed volume %q: %w", seedName, err)
	}
	made = append(made, seedVol)
	if err = h.l.StorageVolUpload(seedVol, strings.NewReader(string(spec.Seed)), 0, uint64(len(spec.Seed)), 0); err != nil {
		return domainInfo{}, fmt.Errorf("upload seed %q: %w", seedName, err)
	}
	seedPath, err := h.l.StorageVolGetPath(seedVol)
	if err != nil {
		return domainInfo{}, err
	}

	xmlDesc, err := domainDefinition(spec, bootPath, seedPath)
	if err != nil {
		return domainInfo{}, err
	}
	// A define that fails AFTER the daemon committed it — an RPC timeout on the
	// reply — would send the unwind to delete volumes a defined domain points at.
	// The window is narrower than the one `committed` closes (it needs a transport
	// failure straddling a server-side commit, not any error), and a refusal is
	// not distinguishable from it without a re-lookup, so the unwind is left to
	// run: an orphaned define with its disks removed is recovered by deleting the
	// domain, where the reverse leaves a name nobody can reuse.
	d, err := h.l.DomainDefineXML(xmlDesc)
	if err != nil {
		return domainInfo{}, fmt.Errorf("define %q: %w", spec.Name, err)
	}
	if err = h.l.DomainCreate(d); err != nil {
		// A machine that was defined but will not boot is not left behind as a
		// half-made thing for the next run to adopt. If the undefine itself fails
		// the volumes must STAY, or what is left is a defined domain pointing at
		// files that no longer exist.
		if uerr := h.l.DomainUndefineFlags(d, 0); uerr != nil {
			committed = true
			return domainInfo{}, fmt.Errorf("start %q failed (%v) and the domain could not be undefined: %w — "+
				"remove it by hand before retrying", spec.Name, err, uerr)
		}
		return domainInfo{}, fmt.Errorf("start %q: %w", spec.Name, err)
	}

	// The machine is running. Whatever happens below, its disks stay.
	committed = true
	return h.info(d)
}

// DestroyDomain removes the machine and the volumes it owns. A machine already
// gone is errNoDomain, which the caller turns into an idempotent success.
func (h *libvirtHV) DestroyDomain(uuid string) error {
	u, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return errNoDomain
		}
		return err
	}

	// Read the volume paths BEFORE undefining: after the domain is gone there is
	// nothing left that names them, and they would leak.
	//
	// ★ A failure to read them is REPORTED, not skipped past. Treating an empty
	// path list as "nothing to delete" is the same silent volume loss the error
	// propagation below exists to prevent: the domain goes, the disks stay, the
	// step says "confirmed gone", and the next create under that name is wedged on
	// "storage volume already exists" with nothing pointing back here.
	var bootPath, seedPath string
	var pathErr error
	if raw, rerr := h.l.DomainGetXMLDesc(dom, 0); rerr != nil {
		pathErr = fmt.Errorf("could not read %s to find its volumes: %w", dom.Name, rerr)
	} else if dx, perr := parseDomainXML(raw, dom.Name); perr != nil {
		pathErr = fmt.Errorf("could not parse %s to find its volumes: %w", dom.Name, perr)
	} else {
		bootPath, seedPath = dx.bootDiskPath(), dx.seedDiskPath()
	}

	if err := h.l.DomainDestroy(dom); err != nil && !libvirt.IsNotFound(err) {
		// A domain that is already stopped answers an error to destroy; that is
		// not a reason to leave it defined.
		if !strings.Contains(strings.ToLower(err.Error()), "not running") {
			return err
		}
	}
	if err := h.l.DomainUndefineFlags(dom, libvirt.DomainUndefineNvram); err != nil && !libvirt.IsNotFound(err) {
		return err
	}
	// ★ A volume that could not be deleted is REPORTED, not swallowed. The caller
	// turns this into "teardown could not be confirmed", which is the truth: the
	// domain is gone but the disk is not, and a later create under the same name
	// collides on the volume with no hint of why.
	if pathErr != nil {
		return fmt.Errorf("%w: %v", errVolumeSurvived, pathErr)
	}
	for _, p := range []string{bootPath, seedPath} {
		if p == "" {
			continue
		}
		vol, lerr := h.l.StorageVolLookupByPath(p)
		if lerr != nil {
			if libvirt.IsNotFound(lerr) {
				continue
			}
			return fmt.Errorf("%w: volume %s could not be looked up: %v", errVolumeSurvived, p, lerr)
		}
		if derr := h.l.StorageVolDelete(vol, 0); derr != nil && !libvirt.IsNotFound(derr) {
			return fmt.Errorf("%w: volume %s survives: %v", errVolumeSurvived, p, derr)
		}
	}
	return nil
}

// gracefulStopTimeout is how long a guest gets to honour the ACPI shutdown before
// it is stopped the hard way.
//
// ★ 30s and not longer, measured: the Debian and Ubuntu GENERICCLOUD images do not
// act on the ACPI power button at all, so this window is spent in full on every
// cpu/ram resize of a stock cloud image. It is not zero because a graceful stop
// lets the guest flush its filesystem, and a resize is not worth corrupting a
// database over.
var gracefulStopTimeout = 30 * time.Second

// StopDomain asks the guest to shut down and waits, because a cpu/ram change
// applies to a machine that is off. It is the caller's allow_downtime that
// authorised this.
//
// It reports whether the stop had to be FORCED. A forced stop is an unclean
// shutdown and the caller says so in an event: silently pulling the power on a
// machine an operator asked to resize is the kind of thing that should never be
// learned from a corrupted filesystem afterwards.
func (h *libvirtHV) StopDomain(ctx context.Context, uuid string) (bool, error) {
	u, err := parseUUID(uuid)
	if err != nil {
		return false, err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		return false, err
	}
	// A machine that is already off is nothing to stop. Asking anyway answers
	// VIR_ERR_OPERATION_INVALID, which is not a not-found, so it would abort a
	// resize of a VM that simply happened to be powered down.
	state, _, _, _, _, err := h.l.DomainGetInfo(dom)
	if err != nil {
		return false, err
	}
	if state == uint8(libvirt.DomainShutoff) {
		return false, nil
	}
	if err := h.l.DomainShutdown(dom); err != nil && !libvirt.IsNotFound(err) {
		return false, err
	}
	deadline := time.Now().Add(gracefulStopTimeout)
	for {
		state, _, _, _, _, err := h.l.DomainGetInfo(dom)
		if err != nil {
			return false, err
		}
		if state == uint8(libvirt.DomainShutoff) {
			return false, nil
		}
		if time.Now().After(deadline) {
			// A guest that ignores ACPI still has to stop, or the resize would
			// silently not apply.
			return true, h.l.DomainDestroy(dom)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (h *libvirtHV) StartDomain(uuid string) error {
	u, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		return err
	}
	return h.l.DomainCreate(dom)
}

// SetVCPUs and SetMemoryBytes write the PERSISTENT config, which is what the
// machine comes back with on the next start. AFFECT_CONFIG rather than LIVE is
// the whole reason the caller stopped it first.
func (h *libvirtHV) SetVCPUs(uuid string, n int64) error {
	u, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		return err
	}
	flags := uint32(libvirt.DomainVCPUConfig | libvirt.DomainVCPUMaximum)
	if err := h.l.DomainSetVcpusFlags(dom, uint32(n), flags); err != nil {
		return err
	}
	return h.l.DomainSetVcpusFlags(dom, uint32(n), uint32(libvirt.DomainVCPUConfig))
}

func (h *libvirtHV) SetMemoryBytes(uuid string, b int64) error {
	u, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		return err
	}
	kib := uint64(b / 1024)
	if err := h.l.DomainSetMemoryFlags(dom, kib, uint32(libvirt.DomainMemConfig|libvirt.DomainMemMaximum)); err != nil {
		return err
	}
	return h.l.DomainSetMemoryFlags(dom, kib, uint32(libvirt.DomainMemConfig))
}

// ResizeDiskBytes grows the boot volume online. The block device grows; the
// filesystem inside follows only if the guest runs growpart — this reaches the
// device, not the partition table on it.
//
// ★ The size goes in BYTES because the BYTES flag is set. virDomainBlockResize
// takes kibibytes when flags is 0 and bytes when VIR_DOMAIN_BLOCK_RESIZE_BYTES is
// present, and go-libvirt passes the number through verbatim. Converting to KiB
// *and* setting the flag asks for a device 1024x smaller than intended — which on
// a grow is a destructive SHRINK, and a permanently non-idempotent one, because
// the next run reads the truncated size and shrinks again.
func (h *libvirtHV) ResizeDiskBytes(uuid string, b int64) error {
	u, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	dom, err := h.l.DomainLookupByUUID(u)
	if err != nil {
		return err
	}
	return h.l.DomainBlockResize(dom, bootDiskTarget, uint64(b), libvirt.DomainBlockResizeBytes)
}

func (h *libvirtHV) FreeMemoryBytes() (int64, error) {
	free, err := h.l.NodeGetFreeMemory()
	if err != nil {
		return 0, err
	}
	return int64(free), nil
}

func (h *libvirtHV) PoolFreeBytes() (int64, error) {
	pool, err := h.l.StoragePoolLookupByName(h.diskPool)
	if err != nil {
		return 0, err
	}
	if err := h.l.StoragePoolRefresh(pool, 0); err != nil {
		return 0, err
	}
	_, _, _, available, err := h.l.StoragePoolGetInfo(pool)
	if err != nil {
		return 0, err
	}
	return int64(available), nil
}

// ── XML builders ────────────────────────────────────────────────────────────

func overlayVolumeXML(name string, capacity int64, backing string) string {
	return fmt.Sprintf(`<volume type='file'>
  <name>%s</name>
  <capacity unit='bytes'>%d</capacity>
  <target><format type='qcow2'/></target>
  <backingStore>
    <path>%s</path>
    <format type='qcow2'/>
  </backingStore>
</volume>`, xmlEscape(name), capacity, xmlEscape(backing))
}

func rawVolumeXML(name string, size int64) string {
	return fmt.Sprintf(`<volume type='file'>
  <name>%s</name>
  <capacity unit='bytes'>%d</capacity>
  <target><format type='raw'/></target>
</volume>`, xmlEscape(name), size)
}

// domainDefinition renders the machine.
//
// ★ The identity is FRESH per creation, and the MAC is derived from it, which is
// two decisions that look like one:
//
//   - The MAC is PINNED in the XML because libvirt generates a new one every time
//     a domain is REDEFINED when the element is absent — silently breaking the
//     lease lookup that `sid` and `primary_ip` depend on, on a machine that is
//     otherwise running fine.
//   - The UUID is RANDOM rather than derived from namespace/name because dnsmasq
//     keeps a lease for its full TTL after the machine is gone. With a derived
//     MAC, destroying a batch and creating it again under the same name inherits
//     the old lease, and `created` reports the new machine ready — with the old
//     address — about a second after defining it, before it has booted at all.
//     Measured: two consecutive live runs both "came up" at 192.168.122.198, the
//     second in 1s. A fresh identity has no lease to inherit.
//
// A recreated machine is a different machine and gets a different vm_id.
func domainDefinition(spec domainSpec, bootPath, seedPath string) (string, error) {
	domUUID, err := randomUUID()
	if err != nil {
		return "", err
	}
	mac, err := macFromUUID(domUUID)
	if err != nil {
		return "", err
	}

	meta := vmMeta{vmMetaBody: vmMetaBody{
		Namespace:          spec.Namespace,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
		ImageID:            spec.ImageID,
		NetworkID:          spec.NetworkID,
		DeletionProtection: spec.Profile.deletionProtection,
	}}
	keys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		meta.Labels = append(meta.Labels, metaLabel{Key: k, Value: spec.Labels[k]})
	}
	metaXML, err := xml.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("render domain metadata: %w", err)
	}

	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <uuid>%s</uuid>
  <memory unit='bytes'>%d</memory>
  <currentMemory unit='bytes'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <metadata>%s</metadata>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features><acpi/><apic/></features>
  <cpu mode='host-passthrough' check='none' migratable='off'/>
  <clock offset='utc'/>
  <on_reboot>restart</on_reboot>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='%s' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='%s' bus='virtio'/>
      <readonly/>
    </disk>
    <interface type='network'>
      <mac address='%s'/>
      <source network='%s'/>
      <model type='virtio'/>
    </interface>
    <serial type='pty'><target port='0'/></serial>
    <console type='pty'><target type='serial' port='0'/></console>
    <memballoon model='virtio'/>
  </devices>
</domain>`,
		xmlEscape(spec.Name), domUUID,
		spec.Profile.ramSize, spec.Profile.ramSize, spec.Profile.cpuSize,
		metaXML,
		xmlEscape(bootPath), bootDiskTarget,
		xmlEscape(seedPath), seedDiskTarget,
		mac, xmlEscape(spec.NetworkName)), nil
}

// randomUUID mints a machine identity. Version 4, because a machine's id should
// carry no meaning that could collide with another machine's.
func randomUUID() (string, error) {
	var u libvirt.UUID
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("mint a domain uuid: %w", err)
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return formatUUID(u), nil
}

// macFromUUID derives a stable QEMU-range MAC (52:54:00:…) from the domain UUID.
// Stable across redefines of one machine, distinct between machines — which is
// exactly the pair of properties the lease lookup needs.
func macFromUUID(domUUID string) (string, error) {
	u, err := parseUUID(domUUID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", u[13], u[14], u[15]), nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
