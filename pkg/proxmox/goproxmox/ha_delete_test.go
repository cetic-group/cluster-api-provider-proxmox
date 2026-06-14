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

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"

	"github.com/jarcoal/httpmock"
)

// haRejectedResponder mimics Proxmox refusing to delete an HA-managed VM: the
// human-readable reason is carried in the HTTP status line (reason phrase),
// which is what go-proxmox surfaces as the error for a 500 response.
func haRejectedResponder(vmID int64) httpmock.Responder {
	return func(_ *http.Request) (*http.Response, error) {
		resp := httpmock.NewStringResponse(http.StatusInternalServerError, "")
		resp.Status = fmt.Sprintf("500 unable to remove VM %d - used in HA resources and purge parameter not set.", vmID)
		return resp, nil
	}
}

func plain500Responder() httpmock.Responder {
	return func(_ *http.Request) (*http.Response, error) {
		resp := httpmock.NewStringResponse(http.StatusInternalServerError, "")
		resp.Status = "500 Internal Server Error"
		return resp, nil
	}
}

// TestProxmoxAPIClient_DeleteVM_HA covers deletion of HA-managed VMs (issue #216):
// the first plain delete fails, and DeleteVM must retry with purge=1 whenever
// the VM is HA-managed (detected via the HA flag or the PVE error message),
// but must NOT retry on unrelated failures.
func TestProxmoxAPIClient_DeleteVM_HA(t *testing.T) {
	const upid = "UPID:test:000D6BDA:041E0A54:654A5A1D:qmdestroy:103:root@pam:"
	const vmID int64 = 103

	tests := []struct {
		name          string
		haManaged     bool // status/current reports ha.managed == 1
		firstDeleteHA bool // first delete fails with the "used in HA resources" message
		firstDeleteOK bool // first delete succeeds (no retry expected)
		wantPurge     bool // a purge=1 delete is expected
		wantErr       bool
	}{
		{name: "ha flag triggers purge", haManaged: true, firstDeleteHA: false, wantPurge: true},
		{name: "ha error message triggers purge", haManaged: false, firstDeleteHA: true, wantPurge: true},
		{name: "unrelated failure is not purged", haManaged: false, firstDeleteHA: false, wantPurge: false, wantErr: true},
		{name: "plain delete succeeds without purge", haManaged: true, firstDeleteOK: true, wantPurge: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t)

			httpmock.RegisterResponder(http.MethodGet, `=~/cluster/nextid`,
				newJSONResponder(400, fmt.Sprintf("VM %d already exists", vmID)))
			httpmock.RegisterResponder(http.MethodGet, `=~/nodes/test/status`,
				newJSONResponder(200, proxmox.Node{Name: "test"}))
			httpmock.RegisterResponder(http.MethodGet, `=~/cluster/status`,
				newJSONResponder(200, proxmox.NodeStatuses{{Name: "test"}}))

			ha := proxmox.HA{}
			if test.haManaged {
				ha.Managed = 1
			}
			httpmock.RegisterResponder(http.MethodGet, `=~/nodes/test/qemu/103/status/current`,
				newJSONResponder(200, proxmox.VirtualMachine{Node: "test", VMID: 103, HA: ha}))
			httpmock.RegisterResponder(http.MethodGet, `=~/nodes/test/qemu/103/config`,
				newJSONResponder(200, proxmox.VirtualMachineConfig{CPU: "kvm64"}))

			// Plain delete (no query string): success or a failure of the configured kind.
			plainURL := testBaseURL + "api2/json/nodes/test/qemu/103"
			switch {
			case test.firstDeleteOK:
				httpmock.RegisterResponder(http.MethodDelete, plainURL, newJSONResponder(200, upid))
			case test.firstDeleteHA:
				httpmock.RegisterResponder(http.MethodDelete, plainURL, haRejectedResponder(vmID))
			default:
				httpmock.RegisterResponder(http.MethodDelete, plainURL, plain500Responder())
			}

			// Purge delete (query string purge=1): only succeeds when a retry is expected.
			purgeURL := plainURL + "?purge=1"
			httpmock.RegisterResponder(http.MethodDelete, purgeURL, newJSONResponder(200, upid))

			task, err := client.DeleteVM(context.Background(), "test", vmID)

			callCount := httpmock.GetCallCountInfo()
			purgeCalls := callCount[http.MethodDelete+" "+purgeURL]
			if test.wantPurge {
				require.Equal(t, 1, purgeCalls, "expected exactly one purge delete")
			} else {
				require.Zero(t, purgeCalls, "did not expect a purge delete")
			}

			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "qmdestroy", task.Type)
				require.Equal(t, "root@pam", task.User)
			}
		})
	}
}

// TestProxmoxAPIClient_EnsureHAResource covers registering a VM as a Proxmox HA
// resource (issue #216): create when absent, update when the state differs, and
// no-op when it is already in the desired state.
func TestProxmoxAPIClient_EnsureHAResource(t *testing.T) {
	const sid = "vm:103"
	const vmID int64 = 103

	tests := []struct {
		name       string
		existing   []map[string]any
		state      string
		wantCreate bool
		wantUpdate bool
	}{
		{name: "create when absent", existing: nil, state: "started", wantCreate: true},
		{name: "default state when empty", existing: nil, state: "", wantCreate: true},
		{name: "update when state differs", existing: []map[string]any{{"sid": sid, "state": "stopped"}}, state: "started", wantUpdate: true},
		{name: "no-op when already desired", existing: []map[string]any{{"sid": sid, "state": "started"}}, state: "started"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t)

			httpmock.RegisterResponder(http.MethodGet, `=~/cluster/ha/resources`,
				newJSONResponder(200, test.existing))
			createURL := testBaseURL + "api2/json/cluster/ha/resources"
			updateURL := createURL + "/" + sid
			httpmock.RegisterResponder(http.MethodPost, createURL, newJSONResponder(200, nil))
			httpmock.RegisterResponder(http.MethodPut, updateURL, newJSONResponder(200, nil))

			err := client.EnsureHAResource(context.Background(), vmID, test.state)
			require.NoError(t, err)

			calls := httpmock.GetCallCountInfo()
			require.Equal(t, boolToInt(test.wantCreate), calls[http.MethodPost+" "+createURL], "create calls")
			require.Equal(t, boolToInt(test.wantUpdate), calls[http.MethodPut+" "+updateURL], "update calls")
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
