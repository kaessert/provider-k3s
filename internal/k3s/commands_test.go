/*
Copyright 2025 The Crossplane Authors.

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

package k3s

import (
	"strings"
	"testing"
)

// TestJoinCommandSkipsBlockingStart proves the fix for the near-timeout
// Create()/Update() wedge: the piped install script must be told to skip
// its own blocking `systemctl restart` (which under the unit's default
// Type=notify waits for the agent/server to actually finish joining) so
// the SSH call returns promptly.
func TestJoinCommandSkipsBlockingStart(t *testing.T) {
	got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: "agent"})

	if !strings.Contains(got, "INSTALL_K3S_SKIP_START='true'") {
		t.Errorf("want INSTALL_K3S_SKIP_START='true' in the piped install command, got %q", got)
	}
}

// TestJoinCommandQueuesNonBlockingRestart proves the command chains its own
// non-blocking restart after the (now non-blocking) install, using
// --no-block so the SSH call does not wait for the unit to report ready.
func TestJoinCommandQueuesNonBlockingRestart(t *testing.T) {
	cases := []struct {
		name        string
		role        string
		wantService string
	}{
		{name: "agent", role: "agent", wantService: "k3s-agent"},
		{name: "additional server", role: "server", wantService: "k3s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: tc.role})

			wantRestart := "systemctl restart " + tc.wantService + " --no-block"
			if !strings.Contains(got, wantRestart) {
				t.Errorf("want %q chained onto the join command, got %q", wantRestart, got)
			}
			if !strings.HasSuffix(strings.TrimSpace(got), "'") {
				t.Errorf("want the restart wrapped in its own sh -c so sudo fallback is scoped to it, got %q", got)
			}
		})
	}
}

// TestJoinCommandRestartFallsBackToSudo proves the appended restart command
// mirrors get.k3s.io's own root-detection idiom rather than assuming
// either passwordless root or a sudo-capable user unconditionally.
func TestJoinCommandRestartFallsBackToSudo(t *testing.T) {
	got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: "agent"})

	if !strings.Contains(got, `[ "$(id -u)" = 0 ]`) {
		t.Errorf("want a root-uid check guarding the sudo fallback, got %q", got)
	}
	if !strings.Contains(got, "SUDO=sudo") {
		t.Errorf("want a sudo fallback for a non-root SSH user, got %q", got)
	}
}

// TestServiceNameForRole proves the exported helper mirrors get.k3s.io's
// own SYSTEM_NAME derivation, the same mapping Observe uses to build its
// systemctl probe.
func TestServiceNameForRole(t *testing.T) {
	if got := ServiceNameForRole("agent"); got != "k3s-agent" {
		t.Errorf("want k3s-agent for role agent, got %q", got)
	}
	if got := ServiceNameForRole("server"); got != "k3s" {
		t.Errorf("want k3s for role server, got %q", got)
	}
	if got := ServiceNameForRole(""); got != "k3s-agent" {
		t.Errorf("want the agent default for an empty role, got %q", got)
	}
}

// TestClassifyServiceState proves every systemctl is-active reply this
// provider cares about is classified correctly, in particular that the
// transitional states a non-blocking restart can now produce
// (activating/reloading/deactivating) are NOT treated as absence.
func TestClassifyServiceState(t *testing.T) {
	cases := []struct {
		stdout string
		want   ServiceState
	}{
		{stdout: "active", want: ServiceActive},
		{stdout: "activating", want: ServiceConverging},
		{stdout: "reloading", want: ServiceConverging},
		{stdout: "deactivating", want: ServiceConverging},
		{stdout: "inactive", want: ServiceNotFound},
		{stdout: "failed", want: ServiceNotFound},
		{stdout: "unknown", want: ServiceNotFound},
		{stdout: "", want: ServiceNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.stdout, func(t *testing.T) {
			if got := ClassifyServiceState(tc.stdout); got != tc.want {
				t.Errorf("ClassifyServiceState(%q) = %v, want %v", tc.stdout, got, tc.want)
			}
		})
	}
}
