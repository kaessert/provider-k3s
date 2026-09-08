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
	"fmt"
	"strings"
)

// InstallParams holds parameters for building a k3s server install command.
type InstallParams struct {
	K3sVersion        string
	K3sChannel        string
	ClusterInit       bool
	TLSSAN            string
	DisableTraefik    bool
	DisableServiceLB  bool
	ExtraArgs         string
	DatastoreEndpoint string
}

// JoinParams holds parameters for building a k3s join command.
type JoinParams struct {
	ServerHost string
	NodeToken  string
	Role       string // "agent" or "server"
	K3sVersion string
	K3sChannel string
	ExtraArgs  string
	TLSSAN     string
}

// InstallCommand builds the k3s server install command string.
func InstallCommand(params InstallParams) string {
	envParts := make([]string, 0, 2)

	// Build INSTALL_K3S_EXEC
	execArgs := []string{"server"}

	if params.TLSSAN != "" {
		execArgs = append(execArgs, fmt.Sprintf("--tls-san %s", params.TLSSAN))
	}

	if params.ClusterInit {
		execArgs = append(execArgs, "--cluster-init")
	}

	if params.DatastoreEndpoint != "" {
		execArgs = append(execArgs, fmt.Sprintf("--datastore-endpoint %s", params.DatastoreEndpoint))
	}

	if params.DisableTraefik {
		execArgs = append(execArgs, "--disable traefik")
	}

	if params.DisableServiceLB {
		execArgs = append(execArgs, "--disable servicelb")
	}

	if params.ExtraArgs != "" {
		execArgs = append(execArgs, params.ExtraArgs)
	}

	envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", strings.Join(execArgs, " ")))

	// Version specification
	envParts = append(envParts, versionEnv(params.K3sVersion, params.K3sChannel)...)

	return fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))
}

// JoinCommand builds the k3s agent/server join command string.
//
// The install script this pipes into (get.k3s.io) creates a systemd unit
// with Type=notify and TimeoutStartSec=0, and -- unless told to skip it --
// starts that unit itself with a plain `systemctl restart`, which under
// Type=notify BLOCKS until the k3s process itself calls sd_notify(READY=1):
// that is, until the agent/server has actually finished joining and is
// fully serving, not merely until the process has been launched. On a
// resource-constrained host that wait measured close to the entire
// reconcile external-call budget, so a single blocked SSH session consumed
// nearly the whole timeout on its own, leaving Observe's own poll loop no
// chance to see incremental progress.
//
// INSTALL_K3S_SKIP_START tells the install script to stop short of that
// blocking restart -- the binary, systemd unit and environment file are
// still written -- and the command chains its own non-blocking restart
// (systemctl ... --no-block) afterward, which returns as soon as the
// restart job is queued rather than waiting for it to finish. The unit's
// Type=notify is untouched, so `systemctl is-active` still only reports
// "active" once the agent/server is genuinely ready -- Observe's poll loop
// sees "activating" in the meantime instead of a blocked SSH call.
func JoinCommand(params JoinParams) string {
	var envParts []string

	// K3S_URL and K3S_TOKEN are required for joining
	envParts = append(envParts,
		fmt.Sprintf("K3S_URL='https://%s:6443'", params.ServerHost),
		fmt.Sprintf("K3S_TOKEN='%s'", params.NodeToken),
	)

	// If joining as an additional server, set INSTALL_K3S_EXEC
	if params.Role == "server" {
		execArgs := []string{"server"}

		if params.TLSSAN != "" {
			execArgs = append(execArgs, fmt.Sprintf("--tls-san %s", params.TLSSAN))
		}

		if params.ExtraArgs != "" {
			execArgs = append(execArgs, params.ExtraArgs)
		}

		envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", strings.Join(execArgs, " ")))
	} else if params.ExtraArgs != "" {
		// Agent with extra args
		envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", params.ExtraArgs))
	}

	// Version specification
	envParts = append(envParts, versionEnv(params.K3sVersion, params.K3sChannel)...)

	// See the doc comment above: skip the install script's own blocking
	// restart and issue a non-blocking one ourselves.
	envParts = append(envParts, "INSTALL_K3S_SKIP_START='true'")

	install := fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))

	return install + " && " + nonBlockingRestartCommand(ServiceNameForRole(params.Role))
}

// ServiceNameForRole returns the systemd unit name get.k3s.io derives for a
// join of the given role, mirroring its own setup_env logic: joining as an
// additional "server" resolves CMD_K3S to "server", giving SYSTEM_NAME
// "k3s" (the same unit name as the original cluster server); anything else
// resolves to the "agent" default, giving SYSTEM_NAME "k3s-agent". Exported
// so Observe's own systemctl probe uses the exact same mapping Create and
// Update build their restart command from.
func ServiceNameForRole(role string) string {
	if role == "server" {
		return "k3s"
	}
	return "k3s-agent"
}

// nonBlockingRestartCommand builds a `systemctl restart --no-block` for the
// given service, falling back to sudo exactly like get.k3s.io's own $SUDO
// detection (unless already running as root) since the install script has
// already left the unit enabled and its environment file written -- this
// only ever needs to trigger the start, never wait for it.
func nonBlockingRestartCommand(service string) string {
	return fmt.Sprintf(
		`sh -c 'SUDO=; [ "$(id -u)" = 0 ] || SUDO=sudo; $SUDO systemctl restart %s --no-block'`,
		service,
	)
}

// ServiceState classifies a `systemctl is-active` reply for a k3s service
// unit started by the non-blocking restart JoinCommand builds (see its doc
// comment). Because that restart returns before the unit finishes starting,
// Observe's poll loop can now see states a purely synchronous, blocking
// restart never left it time to observe.
type ServiceState int

const (
	// ServiceNotFound means the unit was never started at all -- "inactive",
	// "failed", "unknown" or any other reply this provider does not
	// recognise as part of a start/restart in flight. Treated as absence:
	// the external resource does not exist yet (or reinstalling it is
	// exactly the right response, per this provider's stable-external-name
	// model).
	ServiceNotFound ServiceState = iota
	// ServiceConverging means a restart is in flight -- "activating",
	// "reloading" or "deactivating" (the stop phase of a restart) -- and
	// the unit has neither reached nor left its active state yet. The
	// resource already exists (it must not be re-created), but is not yet
	// ready.
	ServiceConverging
	// ServiceActive means the unit's own Type=notify process has reported
	// itself ready.
	ServiceActive
)

// ClassifyServiceState maps a `systemctl is-active` reply to a ServiceState.
func ClassifyServiceState(stdout string) ServiceState {
	switch stdout {
	case "active":
		return ServiceActive
	case "activating", "reloading", "deactivating":
		return ServiceConverging
	default:
		return ServiceNotFound
	}
}

// UninstallServerCommand returns the k3s server uninstall command.
func UninstallServerCommand() string {
	return "/usr/local/bin/k3s-uninstall.sh"
}

// UninstallAgentCommand returns the k3s agent uninstall command.
func UninstallAgentCommand() string {
	return "/usr/local/bin/k3s-agent-uninstall.sh"
}

// RewriteKubeconfig replaces 127.0.0.1 and localhost with the actual host address in a kubeconfig.
func RewriteKubeconfig(kubeconfig, host string) string {
	result := strings.ReplaceAll(kubeconfig, "127.0.0.1", host)
	result = strings.ReplaceAll(result, "localhost", host)
	return result
}

func versionEnv(version, channel string) []string {
	var parts []string
	if version != "" {
		parts = append(parts, fmt.Sprintf("INSTALL_K3S_VERSION='%s'", version))
	} else if channel != "" {
		parts = append(parts, fmt.Sprintf("INSTALL_K3S_CHANNEL='%s'", channel))
	}
	return parts
}
