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

// Package libvirt provides a thin wrapper around the libvirt Go bindings
// for use by the zkvm CAPI provider controllers.
//
// The three files in this package have a single responsibility each:
//   client.go  — open / close the libvirt SSH connection
//   volume.go  — create / delete qcow2 disk images in a storage pool
//   domain.go  — define / start / stop / undefine libvirt VM domains
package libvirt

import (
	"fmt"
	"os"

	libvirtgo "libvirt.org/go/libvirt"
)

// Client wraps a live libvirt connection to a KVM host.
// Obtain one via NewClient() or NewClientWithAuth() and always call Close() when done.
type Client struct {
	conn    *libvirtgo.Connect
	kvmHost string
}

// ClientOptions contains connection parameters for libvirt over SSH.
type ClientOptions struct {
	KVMHost string
	SSHUser string
	KeyPath string
}

// NewClient opens a libvirt connection to the KVM host via SSH using defaults or env vars.
func NewClient(kvmHost string) (*Client, error) {
	user := os.Getenv("OCPZ_KVM_USER")
	if user == "" {
		user = "root"
	}
	return NewClientWithAuth(ClientOptions{
		KVMHost: kvmHost,
		SSHUser: user,
	})
}

// NewClientWithAuth opens a libvirt connection with explicit SSH user and private key path.
func NewClientWithAuth(opts ClientOptions) (*Client, error) {
	user := opts.SSHUser
	if user == "" {
		user = "root"
	}

	var uri string
	if opts.KeyPath != "" {
		uri = fmt.Sprintf("qemu+ssh://%s@%s/system?keyfile=%s&no_verify=1&known_hosts=/dev/null", user, opts.KVMHost, opts.KeyPath)
	} else {
		uri = fmt.Sprintf("qemu+ssh://%s@%s/system?no_verify=1&known_hosts=/dev/null", user, opts.KVMHost)
	}

	conn, err := libvirtgo.NewConnect(uri)
	if err != nil {
		return nil, fmt.Errorf(
			"libvirt: cannot connect to KVM host %s (uri: %s): %w\n"+
				"→ Action: verify that RHEL + libvirtd is running on %s "+
				"and that SSH key/password auth is configured for user %q",
			opts.KVMHost, uri, err, opts.KVMHost, user,
		)
	}

	return &Client{conn: conn, kvmHost: opts.KVMHost}, nil
}

// Close releases the libvirt connection.
// Safe to call on a nil receiver — does nothing.
func (c *Client) Close() {
	if c == nil || c.conn == nil {
		return
	}
	// libvirt.Connect.Close() returns the number of remaining open connections.
	// We ignore the return value — any error here is non-fatal.
	_, _ = c.conn.Close()
}

// Ping verifies the libvirt connection is alive by fetching the host name.
// Returns an error if the connection has been lost.
func (c *Client) Ping() error {
	_, err := c.conn.GetHostname()
	if err != nil {
		return fmt.Errorf("libvirt: ping failed for %s: %w", c.kvmHost, err)
	}
	return nil
}

// StoragePoolExists returns true if a storage pool with the given name exists
// and is active on the KVM host.
func (c *Client) StoragePoolExists(poolName string) (bool, error) {
	pool, err := c.conn.LookupStoragePoolByName(poolName)
	if err != nil {
		// libvirt returns an error with code VIR_ERR_NO_STORAGE_POOL when not found.
		// We treat any lookup error as "not found" for simplicity.
		return false, nil //nolint:nilerr
	}
	defer pool.Free()

	active, err := pool.IsActive()
	if err != nil {
		return false, fmt.Errorf("libvirt: checking pool %q active state: %w", poolName, err)
	}
	return active, nil
}

// BridgeExists returns true if a network interface with the given name exists
// on the KVM host.
func (c *Client) BridgeExists(bridgeName string) (bool, error) {
	iface, err := c.conn.LookupInterfaceByName(bridgeName)
	if err != nil {
		return false, nil //nolint:nilerr
	}
	defer iface.Free()
	return true, nil
}
