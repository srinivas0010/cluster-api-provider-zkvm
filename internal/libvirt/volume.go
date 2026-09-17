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
	"fmt"
)

// VolumeCreateParams holds the parameters needed to create a single qcow2
// disk image in a libvirt storage pool.
type VolumeCreateParams struct {
	// PoolName is the name of the storage pool where the volume will be created.
	PoolName string
	// Name is the filename of the volume, e.g. "mycluster-cp-0.qcow2".
	Name string
	// SizeGB is the logical capacity of the volume in gigabytes.
	// The actual disk usage starts near zero (thin provisioning with qcow2).
	SizeGB int
}

// CreateVolume creates a new qcow2 disk image in the specified storage pool.
//
// The volume is thin-provisioned: logical capacity = SizeGB, initial
// allocation = 0 GB. Disk space on the host grows as the VM writes data.
//
// Returns the absolute path to the created volume file on the KVM host.
func (c *Client) CreateVolume(p VolumeCreateParams) (string, error) {
	pool, err := c.conn.LookupStoragePoolByName(p.PoolName)
	if err != nil {
		return "", fmt.Errorf(
			"libvirt CreateVolume: storage pool %q not found on %s: %w\n"+
				"→ Action: run 'virsh pool-list --all' on %s and verify the pool exists and is active",
			p.PoolName, c.kvmHost, err, c.kvmHost,
		)
	}
	defer pool.Free()

	// Refresh the pool so libvirt picks up any externally added files.
	// Bug 6: propagate the error instead of silently swallowing it with _ = err.
	// A Refresh failure typically means the pool is inactive or the underlying
	// storage path is gone — continuing after this leads to confusing failures.
	if err := pool.Refresh(0); err != nil {
		return "", fmt.Errorf(
			"libvirt CreateVolume: pool %q Refresh failed on %s: %w\n"+
				"→ Action: run 'virsh pool-start %s' on %s to activate the pool",
			p.PoolName, c.kvmHost, err, p.PoolName, c.kvmHost,
		)
	}

	volXML := fmt.Sprintf(`
<volume type='file'>
  <name>%s</name>
  <capacity unit='GiB'>%d</capacity>
  <allocation unit='GiB'>0</allocation>
  <target>
    <format type='qcow2'/>
    <permissions>
      <mode>0600</mode>
    </permissions>
  </target>
</volume>`, p.Name, p.SizeGB)

	vol, err := pool.StorageVolCreateXML(volXML, 0)
	if err != nil {
		return "", fmt.Errorf(
			"libvirt CreateVolume: failed to create volume %q in pool %q on %s: %w\n"+
				"→ Action: verify the pool has enough free space (virsh pool-info %s)",
			p.Name, p.PoolName, c.kvmHost, err, p.PoolName,
		)
	}
	defer vol.Free()

	path, err := vol.GetPath()
	if err != nil {
		return "", fmt.Errorf("libvirt CreateVolume: GetPath for %q: %w", p.Name, err)
	}

	return path, nil
}

// DeleteVolume removes a qcow2 disk image from the storage pool by name.
//
// If the volume does not exist, the function returns nil (idempotent).
// This allows the reconciler to call DeleteVolume safely during cleanup
// even if the volume was never created or was already deleted.
func (c *Client) DeleteVolume(poolName, volName string) error {
	pool, err := c.conn.LookupStoragePoolByName(poolName)
	if err != nil {
		// Pool not found — nothing to delete.
		return nil //nolint:nilerr
	}
	defer pool.Free()

	vol, err := pool.LookupStorageVolByName(volName)
	if err != nil {
		// Volume not found — already deleted or never created.
		return nil //nolint:nilerr
	}
	defer vol.Free()

	if err := vol.Delete(0); err != nil {
		return fmt.Errorf(
			"libvirt DeleteVolume: failed to delete volume %q from pool %q on %s: %w",
			volName, poolName, c.kvmHost, err,
		)
	}

	return nil
}

// VolumeExists returns true if a volume with the given name exists in the pool.
func (c *Client) VolumeExists(poolName, volName string) (bool, error) {
	pool, err := c.conn.LookupStoragePoolByName(poolName)
	if err != nil {
		return false, nil //nolint:nilerr
	}
	defer pool.Free()

	vol, err := pool.LookupStorageVolByName(volName)
	if err != nil {
		return false, nil //nolint:nilerr
	}
	vol.Free()
	return true, nil
}

// GetVolumePath returns the absolute path of an existing volume on the KVM host.
//
// Bug 1 fix: the controller previously reconstructed the path by concatenating
// a hardcoded "/var/lib/libvirt/images/" prefix. That assumption breaks for
// custom pools. This function asks libvirt directly, which always returns the
// authoritative path regardless of pool configuration.
func (c *Client) GetVolumePath(poolName, volName string) (string, error) {
	pool, err := c.conn.LookupStoragePoolByName(poolName)
	if err != nil {
		return "", fmt.Errorf(
			"libvirt GetVolumePath: pool %q not found on %s: %w",
			poolName, c.kvmHost, err,
		)
	}
	defer pool.Free()

	vol, err := pool.LookupStorageVolByName(volName)
	if err != nil {
		return "", fmt.Errorf(
			"libvirt GetVolumePath: volume %q not found in pool %q on %s: %w",
			volName, poolName, c.kvmHost, err,
		)
	}
	defer vol.Free()

	path, err := vol.GetPath()
	if err != nil {
		return "", fmt.Errorf(
			"libvirt GetVolumePath: GetPath for %q: %w",
			volName, err,
		)
	}
	return path, nil
}
