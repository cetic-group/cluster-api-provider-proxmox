//go:build integration

/*
Copyright 2023-2026 IONOS Cloud.

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

package goproxmox

// Integration test for issue #216: deleting an HA-managed VM.
//
// It runs against a REAL Proxmox cluster and is gated behind the `integration`
// build tag so it never runs in the normal unit-test suite. It creates a tiny
// throwaway VM, registers it as an HA resource, then asserts that DeleteVM
// removes it (which only works with the purge fix).
//
// Run with:
//
//	PVE_URL="https://pve.example:8006" \
//	PVE_TOKEN_ID="root@pam!capi" \
//	PVE_SECRET="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" \
//	PVE_NODE="pve-node-1" \
//	PVE_INSECURE=1 \
//	go test -tags integration -run TestIntegration_DeleteHAManagedVM \
//	    -v -count=1 ./pkg/proxmox/goproxmox/

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func haIntegrationClient(t *testing.T) (*APIClient, string) {
	t.Helper()

	baseURL := os.Getenv("PVE_URL")
	tokenID := os.Getenv("PVE_TOKEN_ID")
	secret := os.Getenv("PVE_SECRET")
	node := os.Getenv("PVE_NODE")
	if baseURL == "" || tokenID == "" || secret == "" || node == "" {
		t.Skip("set PVE_URL, PVE_TOKEN_ID, PVE_SECRET and PVE_NODE to run the HA integration test")
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	if os.Getenv("PVE_INSECURE") != "" {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in for self-signed test clusters
		}
	}

	client, err := NewAPIClient(context.Background(), logr.Discard(), baseURL,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(tokenID, secret),
	)
	require.NoError(t, err, "could not connect to Proxmox at %s", baseURL)

	return client, node
}

func TestIntegration_DeleteHAManagedVM(t *testing.T) {
	client, nodeName := haIntegrationClient(t)
	ctx := context.Background()

	// Pick a free VMID for the throwaway VM.
	cluster, err := client.Cluster(ctx)
	require.NoError(t, err)
	nextID, err := cluster.NextID(ctx)
	require.NoError(t, err)
	vmID := int64(nextID)
	sid := fmt.Sprintf("vm:%d", vmID)
	t.Logf("using throwaway VMID %d on node %s", vmID, nodeName)

	node, err := client.Node(ctx, nodeName)
	require.NoError(t, err)

	// Best-effort cleanup in case the test fails before the assertion.
	t.Cleanup(func() {
		_ = client.Delete(ctx, "/cluster/ha/resources/"+sid, nil)
		_, _ = client.deleteVMWithPurge(ctx, &proxmox.VirtualMachine{Node: nodeName, VMID: proxmox.StringOrUint64(vmID)})
	})

	// 1. Create a minimal, diskless VM (enough to exist and be HA-registered).
	createTask, err := node.NewVirtualMachine(ctx, int(vmID),
		proxmox.VirtualMachineOption{Name: "name", Value: fmt.Sprintf("capmox-ha-test-%d", vmID)},
		proxmox.VirtualMachineOption{Name: "memory", Value: 512},
		proxmox.VirtualMachineOption{Name: "cores", Value: 1},
	)
	require.NoError(t, err)
	require.NoError(t, createTask.WaitFor(ctx, 60), "VM creation task did not finish")
	t.Logf("created VM %d", vmID)

	// 2. Register it as an HA resource (kept stopped so the CRM doesn't fight us).
	require.NoError(t, client.Post(ctx, "/cluster/ha/resources",
		map[string]string{"sid": sid, "state": "stopped"}, nil),
		"could not add VM to HA (does the token have HA privileges?)")
	t.Logf("registered %s as HA resource", sid)

	// 3. Confirm Proxmox now reports the VM as HA-managed.
	require.Eventually(t, func() bool {
		vm, verr := node.VirtualMachine(ctx, int(vmID))
		return verr == nil && vm.HA.Managed == 1
	}, 30*time.Second, 2*time.Second, "VM never became HA-managed")
	t.Logf("VM %d is HA-managed", vmID)

	// 4. The actual assertion: DeleteVM must succeed despite HA membership.
	task, err := client.DeleteVM(ctx, nodeName, vmID)
	require.NoError(t, err, "DeleteVM failed for HA-managed VM (issue #216)")
	if task != nil {
		require.NoError(t, task.WaitFor(ctx, 60), "delete task did not finish")
	}
	t.Logf("DeleteVM succeeded for HA-managed VM %d", vmID)

	// 5. Verify the VM and its HA resource are really gone.
	require.Eventually(t, func() bool {
		free, cerr := cluster.CheckID(ctx, int(vmID))
		return cerr == nil && free
	}, 30*time.Second, 2*time.Second, "VMID still taken after delete")

	var haRes []map[string]any
	if err := client.Get(ctx, "/cluster/ha/resources", &haRes); err == nil {
		for _, r := range haRes {
			require.NotEqual(t, sid, fmt.Sprintf("%v", r["sid"]), "HA resource still present after delete")
		}
	}
	t.Logf("VM %d and HA resource %s fully removed — issue #216 fixed", vmID, sid)
}
