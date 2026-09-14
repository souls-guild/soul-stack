// The structural checks — everything refusable without touching a hypervisor.
//
// Each action's validate function is the SAME function Apply calls before doing
// anything (see object.go), so "Validate rejects what Apply would reject"
// (NIM-786) is structural here rather than a property two lists have to keep.
package main

import (
	"fmt"
	"regexp"
)

// The connection param keys, as constants so that a rename breaks the guard test
// rather than silently leaving a state undeclared.
//
// Two, because libvirt needs two: where the hypervisor is, and which slice of it
// this artifact may touch. There is no credential among them — a libvirtd over
// its unix socket authenticates by the permissions on that socket, and TLS to a
// remote one is configured in libvirt's own client files.
const (
	connEndpoint  = "endpoint"
	connNamespace = "namespace"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateConn checks the params every action carries.
func validateConn(params map[string]any, requireNamespace bool) []string {
	f := newFields(params, "")

	if ep := f.str(connEndpoint); ep == "" {
		f.errs = append(f.errs, "endpoint is required — the libvirt connection URI, e.g. qemu:///system")
	} else if !knownLibvirtURI(ep) {
		f.errs = append(f.errs, fmt.Sprintf("endpoint %q is not a libvirt URI: vmlocal expects something like qemu:///system or qemu+ssh://host/system. "+
			"A host:port endpoint means this step is addressing vmlocal with some other provider's connection details.", ep))
	}

	if requireNamespace && f.str(connNamespace) == "" {
		f.errs = append(f.errs, "namespace is required: it is the scope this artifact is allowed to see, and without it a probe would report VMs it did not create")
	}
	return f.errs
}

// validateCreated is the batch-provision check.
func validateCreated(params map[string]any) []string {
	errs := validateConn(params, false)
	f := newFields(params, "")

	rawProfile, ok := params["profile"].(map[string]any)
	if !ok {
		return append(errs, "profile is required and must be an object")
	}
	prof, perrs := parseProfile(rawProfile)
	errs = append(errs, perrs...)

	if prof.scope(f.str(connNamespace)) == "" {
		errs = append(errs, "profile must set namespace (or the step must pass one)")
	}

	// image_id XOR image_name. Both at once is refused rather than silently
	// preferring one — an author who wrote both has two intentions and only one of
	// them would happen.
	switch {
	case prof.imageID == "" && prof.imageName == "":
		errs = append(errs, "profile needs image_id (exact UUID) or image_name (a volume in the image pool, e.g. debian-12)")
	case prof.imageID != "" && prof.imageName != "":
		errs = append(errs, "profile sets both image_id and image_name — keep one")
	case prof.imageID != "" && !uuidRe.MatchString(prof.imageID):
		errs = append(errs, "profile.image_id must be a UUID")
	}
	if prof.networkID == "" {
		errs = append(errs, "profile.network_id is required (the UUID of a libvirt network)")
	} else if !uuidRe.MatchString(prof.networkID) {
		errs = append(errs, "profile.network_id must be a UUID")
	}

	if prof.cpuSize <= 0 {
		errs = append(errs, "profile.cpu_size is required and must be > 0")
	}
	if prof.ramSize <= 0 {
		errs = append(errs, "profile.ram_size is required and must be > 0 (BYTES, not MiB)")
	}
	if prof.bootDiskSize <= 0 {
		errs = append(errs, "profile.boot_disk_size is required and must be > 0 (BYTES, not GiB)")
	}

	count := f.integer("count")
	if f.has("count") && count <= 0 {
		errs = append(errs, "count must be > 0")
	}

	// Batch identity, without which a rerun cannot recognise its own machines and
	// would top up on top of a full batch.
	if f.str("name") == "" && prof.runLabel == "" {
		errs = append(errs, fmt.Sprintf("name is required unless profile.labels[%q] is set: without one of the two a rerun cannot recognise its own VMs and would spawn orphans", runLabelKey))
	}

	if ud := f.str("userdata"); len(ud) > userdataMaxBytes {
		errs = append(errs, fmt.Sprintf("userdata is %d bytes, over the %d-byte cap", len(ud), userdataMaxBytes))
	}

	return append(errs, f.errs...)
}

// validateVMIDs is the check shared by the actions that address existing VMs: the
// connection, a namespace to look them up in, and a non-empty id list.
func validateVMIDs(params map[string]any) []string {
	errs := validateConn(params, true)
	f := newFields(params, "")
	ids := f.strList("vm_ids")
	if len(ids) == 0 && len(f.errs) == 0 {
		errs = append(errs, "vm_ids is required and must not be empty")
	}
	return append(errs, f.errs...)
}

// validateProbed: vm_ids and run_label are both optional filters.
func validateProbed(params map[string]any) []string {
	errs := validateConn(params, true)
	f := newFields(params, "")
	f.strList("vm_ids")
	f.str("run_label")
	return append(errs, f.errs...)
}

// validateResized adds the absolute targets and the downtime consent.
//
// The consent gate is real rather than ceremonial: cpu and ram go through
// stop → update → start, so a cpu_cores or ram_mb change DOES interrupt whatever
// the machine was serving. libvirt can hot-plug some of it; this artifact does
// not, and the gate says so rather than the implementation surprising anyone.
func validateResized(params map[string]any) []string {
	errs := validateVMIDs(params)
	f := newFields(params, "")

	cpu, ram, disk := f.integer("cpu_cores"), f.integer("ram_mb"), f.integer("disk_gb")
	if cpu < 0 {
		errs = append(errs, "cpu_cores must be >= 0 (0 leaves cpu alone)")
	}
	if ram < 0 {
		errs = append(errs, "ram_mb must be >= 0 (0 leaves ram alone)")
	}
	if disk < 0 {
		errs = append(errs, "disk_gb must be >= 0 (0 leaves the disk alone)")
	}
	if cpu == 0 && ram == 0 && disk == 0 && len(f.errs) == 0 {
		errs = append(errs, "resized needs at least one of cpu_cores / ram_mb / disk_gb to be > 0")
	}
	if (cpu > 0 || ram > 0) && !f.boolean("allow_downtime") {
		errs = append(errs, "a cpu_cores or ram_mb change stops and starts the VM, so it needs allow_downtime: true. A disk_gb-only resize is online and does not.")
	}
	return append(errs, f.errs...)
}
