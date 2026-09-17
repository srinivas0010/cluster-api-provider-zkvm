/*
Copyright 2024 IBM Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libvirt

import (
	"encoding/xml"
	"fmt"

	libvirtgo "libvirt.org/go/libvirt"
)

// DomainCreateParams holds everything needed to define and start one KVM VM.
type DomainCreateParams struct {
	// Name is the libvirt domain name, e.g. "mycluster-cp-0".
	// Must be unique on the KVM host.
	Name string
	// VCPUs is the number of virtual CPUs for the VM.
	VCPUs int
	// MemoryGB is the amount of RAM in gigabytes.
	MemoryGB int
	// DiskPath is the absolute path to the qcow2 disk image on the KVM host.
	DiskPath string
	// NetworkBridge is the Linux bridge interface the VM's NIC attaches to.
	NetworkBridge string
	// MACAddress is the MAC address to assign to the VM's NIC.
	// Format: "52:54:00:xx:xx:xx" (QEMU/KVM convention).
	MACAddress string
	// KernelPath is the absolute path to the s390x kernel image on the KVM host.
	// Used for direct kernel boot (no BIOS/UEFI needed).
	KernelPath string
	// InitrdPath is the absolute path to the initrd image on the KVM host.
	InitrdPath string
	// KernelCmdline is the kernel command line injected at boot.
	// For the CAPI flow this includes: ignition.config.url=<url> ip=<ip> nameserver=<ns>
	KernelCmdline string
}

// domainXMLInterface is used for parsing MAC from domain XML.
type domainXMLInterface struct {
	MAC struct {
		Address string `xml:"address,attr"`
	} `xml:"mac"`
}

// domainXMLDoc is a minimal struct for parsing relevant fields from domain XML.
type domainXMLDoc struct {
	XMLName    xml.Name             `xml:"domain"`
	Interfaces []domainXMLInterface `xml:"devices>interface"`
}

// DefineDomain defines a libvirt VM domain from the given parameters.
// The domain is registered with libvirt but NOT started (powered on).
// Call StartDomain() to boot it.
//
// Architecture: s390x (IBM Z). Machine type: s390-ccw-virtio.
// Boot method: direct kernel boot (kernel + initrd + cmdline — no BIOS).
func (c *Client) DefineDomain(p DomainCreateParams) error {
	domainXML := fmt.Sprintf(`
<domain type='kvm'>
  <name>%s</name>
  <memory unit='GiB'>%d</memory>
  <currentMemory unit='GiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>

  <os>
    <type arch='s390x' machine='s390-ccw-virtio'>hvm</type>
    <kernel>%s</kernel>
    <initrd>%s</initrd>
    <cmdline>%s</cmdline>
  </os>

  <features>
    <acpi/>
  </features>

  <cpu mode='host-passthrough'/>

  <devices>
    <emulator>/usr/bin/qemu-system-s390x</emulator>

    <!-- Root disk: thin-provisioned qcow2 via virtio -->
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='none' io='native'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
      <boot order='1'/>
    </disk>

    <!-- Network: virtio NIC attached to the host bridge -->
    <interface type='bridge'>
      <mac address='%s'/>
      <source bridge='%s'/>
      <model type='virtio'/>
    </interface>

    <!-- Serial console for debugging -->
    <console type='pty'>
      <target type='sclp'/>
    </console>

    <!-- Balloon driver for memory stats (required for libvirt memoryStats()) -->
    <memballoon model='virtio'/>

    <!-- VirtIO RNG for entropy -->
    <rng model='virtio'>
      <backend model='random'>/dev/urandom</backend>
    </rng>
  </devices>
</domain>`,
		p.Name,
		p.MemoryGB,
		p.MemoryGB,
		p.VCPUs,
		p.KernelPath,
		p.InitrdPath,
		p.KernelCmdline,
		p.DiskPath,
		p.MACAddress,
		p.NetworkBridge,
	)

	dom, err := c.conn.DomainDefineXML(domainXML)
	if err != nil {
		return fmt.Errorf(
			"libvirt DefineDomain: failed to define domain %q on %s: %w",
			p.Name, c.kvmHost, err,
		)
	}
	dom.Free()
	return nil
}

// StartDomain powers on a defined (stopped) libvirt domain.
//
// The domain must already be defined via DefineDomain() before calling this.
// Equivalent to pressing the power button on the VM.
func (c *Client) StartDomain(name string) error {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf(
			"libvirt StartDomain: domain %q not found on %s: %w\n"+
				"→ Action: call DefineDomain() before StartDomain()",
			name, c.kvmHost, err,
		)
	}
	defer dom.Free()

	if err := dom.Create(); err != nil {
		return fmt.Errorf(
			"libvirt StartDomain: failed to start domain %q on %s: %w",
			name, c.kvmHost, err,
		)
	}
	return nil
}

// DestroyDomain forcefully powers off a running domain and removes its definition.
//
// Equivalent to: virsh destroy <name> && virsh undefine <name>
// This is intentionally forceful (not graceful shutdown) because during cluster
// teardown we need a guaranteed, immediate removal regardless of guest OS state.
//
// Idempotent: if the domain does not exist, returns nil.
func (c *Client) DestroyDomain(name string) error {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		// Domain not found — already destroyed or never created.
		return nil //nolint:nilerr
	}
	defer dom.Free()

	// Force power off — ignore error if already stopped.
	state, _, err := dom.GetState()
	if err == nil && state == libvirtgo.DOMAIN_RUNNING {
		if err := dom.Destroy(); err != nil {
			return fmt.Errorf("libvirt DestroyDomain: destroy %q on %s: %w", name, c.kvmHost, err)
		}
	}

	// Remove the domain definition.
	if err := dom.Undefine(); err != nil {
		return fmt.Errorf("libvirt DestroyDomain: undefine %q on %s: %w", name, c.kvmHost, err)
	}

	return nil
}

// DomainExists returns true if a domain with the given name exists on the KVM host.
func (c *Client) DomainExists(name string) (bool, error) {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return false, nil //nolint:nilerr
	}
	dom.Free()
	return true, nil
}

// IsDomainRunning returns true if the named domain is currently in the
// RUNNING state on the KVM host.
//
// Bug 3 fix: the controller previously called StartDomain() unconditionally
// and swallowed the "already running" error. That caused a noisy error log
// on every reconcile after first boot. This function lets the caller check
// state first, making the start path truly idempotent.
func (c *Client) IsDomainRunning(name string) (bool, error) {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		// Domain not found at all — not running.
		return false, nil //nolint:nilerr
	}
	defer dom.Free()

	state, _, err := dom.GetState()
	if err != nil {
		return false, fmt.Errorf("libvirt IsDomainRunning: GetState for %q on %s: %w", name, c.kvmHost, err)
	}
	return state == libvirtgo.DOMAIN_RUNNING, nil
}

// GetDomainIP returns the first IPv4 address reported by the libvirt guest
// agent for the named domain.
//
// Bug 5 fix: Status.Addresses was always populated with an empty string.
// This function queries libvirt's guest agent interface list (requires
// qemu-guest-agent running inside the VM). If the guest agent is not yet
// available (VM still booting) the call returns an error and the controller
// requeues — correct behaviour during the boot window.
//
// The caller should tolerate transient errors and requeue with backoff.
func (c *Client) GetDomainIP(name string) (string, error) {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return "", fmt.Errorf("libvirt GetDomainIP: domain %q not found on %s: %w", name, c.kvmHost, err)
	}
	defer dom.Free()

	// INTERFACE_ADDRESSES_SRC_AGENT (1) uses the guest agent.
	// INTERFACE_ADDRESSES_SRC_LEASE (0) uses DHCP leases — less accurate.
	// We prefer the guest agent for authoritative data.
	ifaces, err := dom.ListAllInterfaceAddresses(libvirtgo.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT)
	if err != nil {
		return "", fmt.Errorf(
			"libvirt GetDomainIP: ListAllInterfaceAddresses for %q on %s: %w\n"+
				"→ VM may still be booting or qemu-guest-agent not yet running",
			name, c.kvmHost, err,
		)
	}

	for _, iface := range ifaces {
		// Skip loopback.
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.Addrs {
			// Type 0 = IPv4.
			if addr.Type == 0 && addr.Addr != "" {
				return addr.Addr, nil
			}
		}
	}

	return "", fmt.Errorf("libvirt GetDomainIP: no IPv4 address found for domain %q on %s", name, c.kvmHost)
}

// GetDomainMAC returns the MAC address of the first NIC of the named domain.
// The domain must already exist (be defined). Used by GetNodeDetails to
// retrieve the actual MAC libvirt assigned so it can be put in agent-config.yaml.
func (c *Client) GetDomainMAC(name string) (string, error) {
	dom, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return "", fmt.Errorf("libvirt GetDomainMAC: domain %q not found: %w", name, err)
	}
	defer dom.Free()

	xmlStr, err := dom.GetXMLDesc(0)
	if err != nil {
		return "", fmt.Errorf("libvirt GetDomainMAC: GetXMLDesc for %q: %w", name, err)
	}

	var doc domainXMLDoc
	if err := xml.Unmarshal([]byte(xmlStr), &doc); err != nil {
		return "", fmt.Errorf("libvirt GetDomainMAC: parse XML for %q: %w", name, err)
	}

	if len(doc.Interfaces) == 0 {
		return "", fmt.Errorf("libvirt GetDomainMAC: no network interfaces found in domain %q", name)
	}

	return doc.Interfaces[0].MAC.Address, nil
}
