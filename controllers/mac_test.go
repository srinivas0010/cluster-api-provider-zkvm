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

package controllers

import (
	"fmt"
	"strings"
	"testing"
)

// TestDeriveMACAddress verifies that deriveMACAddress:
//  1. Always produces the 52:54:00 QEMU/KVM vendor prefix.
//  2. Is deterministic — same input → same output.
//  3. Is anagram-safe — different names with the same character multiset
//     must NOT produce the same MAC (the old XOR bug).
//  4. Produces a valid 6-octet colon-delimited hex string.
func TestDeriveMACAddress(t *testing.T) {
	t.Run("vendor_prefix", func(t *testing.T) {
		mac := deriveMACAddress("mycluster-cp-0")
		if !strings.HasPrefix(mac, "52:54:00:") {
			t.Errorf("expected prefix 52:54:00:, got %q", mac)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		name := "mycluster-worker-0"
		a := deriveMACAddress(name)
		b := deriveMACAddress(name)
		if a != b {
			t.Errorf("deriveMACAddress not deterministic: %q != %q", a, b)
		}
	})

	t.Run("anagram_safe", func(t *testing.T) {
		// "abc" and "cba" are anagrams — old XOR hash would return the same MAC.
		macABC := deriveMACAddress("abc")
		macCBA := deriveMACAddress("cba")
		if macABC == macCBA {
			t.Errorf("anagram collision: deriveMACAddress(%q) == deriveMACAddress(%q) == %q", "abc", "cba", macABC)
		}
	})

	t.Run("valid_format", func(t *testing.T) {
		for _, name := range []string{"cp-0", "worker-1", "mycluster-control-plane-abc12"} {
			mac := deriveMACAddress(name)
			parts := strings.Split(mac, ":")
			if len(parts) != 6 {
				t.Errorf("deriveMACAddress(%q) = %q: expected 6 octets, got %d", name, mac, len(parts))
				continue
			}
			for _, p := range parts {
				if len(p) != 2 {
					t.Errorf("deriveMACAddress(%q) = %q: octet %q is not 2 hex digits", name, mac, p)
				}
			}
		}
	})

	t.Run("no_collisions_in_cluster", func(t *testing.T) {
		// Simulate a compact cluster: 3 control-plane + 0 worker names.
		// All MACs must be unique.
		names := []string{
			"mycluster-control-plane-0",
			"mycluster-control-plane-1",
			"mycluster-control-plane-2",
		}
		seen := make(map[string]string)
		for _, n := range names {
			mac := deriveMACAddress(n)
			if prev, exists := seen[mac]; exists {
				t.Errorf("MAC collision: deriveMACAddress(%q) == deriveMACAddress(%q) == %q", n, prev, mac)
			}
			seen[mac] = n
		}
	})

	t.Run("no_collisions_ha_cluster", func(t *testing.T) {
		// Simulate a full HA cluster: 3 control-plane + 3 workers.
		var names []string
		for i := 0; i < 3; i++ {
			names = append(names, fmt.Sprintf("ha-cluster-control-plane-%d", i))
		}
		for i := 0; i < 3; i++ {
			names = append(names, fmt.Sprintf("ha-cluster-md-0-%d", i))
		}
		seen := make(map[string]string)
		for _, n := range names {
			mac := deriveMACAddress(n)
			if prev, exists := seen[mac]; exists {
				t.Errorf("MAC collision: %q and %q both map to %q", n, prev, mac)
			}
			seen[mac] = n
		}
	})
}
