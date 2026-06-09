/*
Copyright 2026 IONOS Cloud.

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

package network

import (
	"fmt"

	"k8s.io/utils/ptr"
)

// Network implements an interface that is used to centrally validate and
// normalize network configuration before the renderers use it.
type Network struct {
	Devices []NetworkConfigData
}

// Validate checks structural invariants that hold for any renderer. A renderer
// that only implements a subset of the stack is expected to override this,
// call it, and then reject the features it does not support.
func (n *Network) Validate() error {
	if len(n.Devices) == 0 {
		return ErrMissingNetworkConfigData
	}

	// Routes collide when they share a destination and metric within the same
	// routing table.
	routeCollision := make(map[string]struct{})

	// Tracks whether any device contributes a default gateway, either via a
	// default route or implicitly via DHCP.
	// TODO: IPv6 slaac.
	hasGateway := false

	for i := range n.Devices {
		d := &n.Devices[i]

		// A VRF device's routes live in its own table. A member interface's
		// routes live in the table of the VRF it is attached to.
		var deviceTable *int32
		switch {
		case d.Type == "vrf":
			deviceTable = d.Table
		default:
			if parent := n.Parent(i); parent != nil {
				deviceTable = parent.Table
			}
		}

		if err := validateRoutes(d.Routes, deviceTable, &hasGateway, routeCollision); err != nil {
			return err
		}
		if err := validateFIBRules(d.FIBRules, d.Type == "vrf"); err != nil {
			return err
		}

		// DHCP may produce a default gateway.
		if d.DHCP4 || d.DHCP6 {
			hasGateway = true
		}
	}

	// If you end up here, please make an issue explaining how you need a
	// cluster without a default gateway. This is a valid usecase and this check
	// is merely an anti-footgun for regular users. As a work around, set an
	// invalid gateway which netlink can not create.
	if !hasGateway {
		return ErrMissingGateway
	}

	return nil
}

// Parent returns the device controlling the device at index i (e.g. the VRF a
// NIC is attached to), or nil. It is derived by finding the device whose
// Children list names the device at i.
func (n *Network) Parent(i int) *NetworkConfigData {
	if i < 0 || i >= len(n.Devices) {
		return nil
	}
	name := n.Devices[i].Name
	for p := range n.Devices {
		for _, child := range n.Devices[p].Children {
			if child == name {
				return &n.Devices[p]
			}
		}
	}
	return nil
}

func validateRoutes(routes []RoutingData, deviceTable *int32, hasGateway *bool, routeCollisionMap map[string]struct{}) error {
	// No support for blackhole, etc.pp. Add iff you require this.
	for _, route := range routes {
		if !route.To.IsValid() {
			// Route without a target makes no sense.
			return ErrMalformedRoute
		}
		if route.To.Bits() == 0 && route.To.Addr().IsUnspecified() {
			*hasGateway = true
		}

		// A route is uniquely identified by its effective table, target subnet
		// and metric. An explicit per route table wins over the device's table.
		// The Default table in linux is table 254.
		effectiveTable := deviceTable
		if route.Table != nil {
			effectiveTable = route.Table
		}
		routeID := fmt.Sprintf("%d %s %d",
			ptr.Deref(effectiveTable, 254),
			route.To.String(),
			ptr.Deref(route.Metric, 0),
		)
		if _, exists := routeCollisionMap[routeID]; exists {
			return ErrConflictingMetrics
		}

		// A route's table may differ from the device's table: a member
		// interface is free to insert routes into any table, so only a genuine
		// (table, target, metric) collision is an error.
		routeCollisionMap[routeID] = struct{}{}
	}
	return nil
}

func validateFIBRules(rules []FIBRuleData, isVrf bool) error {
	for _, rule := range rules {
		// We only support To/From and we require a table if we're not a vrf.
		if !rule.To.IsValid() && !rule.From.IsValid() {
			return ErrMalformedFIBRule
		}
		if ptr.Deref(rule.Table, 0) == 0 && !isVrf {
			return ErrMalformedFIBRule
		}
	}
	return nil
}
