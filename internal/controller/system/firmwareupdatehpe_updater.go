// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
)

// HPEFirmwareEntry represents a single firmware component as reported by iLO's FirmwareInventory.
type HPEFirmwareEntry struct {
	// Targets are HPE's stable hardware identities for this component, used to match against SPP manifest entries.
	// Corresponds to Oem.Hpe.Targets[] in the iLO Redfish FirmwareInventory response.
	Targets []string
	// DeviceClass is the HPE device class GUID from Oem.Hpe.DeviceClass.
	DeviceClass string
	// Version is the currently installed firmware version string.
	Version string
	// Name is the human-readable component name (e.g. "System ROM", "iLO 5").
	Name string
}

// SPPManifestEntry represents a single firmware package entry from the SPP manifest/metadata.json.
type SPPManifestEntry struct {
	// TargetGUIDs are the hardware identities this package targets; at least one must match an
	// HPEFirmwareEntry.Targets entry to be considered applicable to the server.
	TargetGUIDs []string
	// Version is the firmware version provided by this package.
	Version string
	// Name is the human-readable component name from the manifest.
	Name string
	// PackagePath is the filename of the package (e.g. "cp071577.exe") relative to the BaseURI.
	PackagePath string
	// UpdatableBy lists the update agents that can apply this package (e.g. "Bmc", "Uefi", "RuntimeAgent").
	UpdatableBy []string
	// DirectFlashOK indicates that iLO can flash this component directly without an OS agent.
	// Derived from Devices.Device[].FirmwareImages[].DirectFlashOK in the SPP metadata.
	DirectFlashOK bool
}

// HPETaskStatus represents the current status of a single component task in iLO's UpdateTaskQueue.
type HPETaskStatus struct {
	// URI is the Redfish URI of the task in the UpdateTaskQueue.
	URI string
	// Name is the human-readable name of the component being updated.
	Name string
	// State is the task state as reported by iLO (e.g. "Pending", "InProgress", "Completed", "Exception").
	State string
	// PercentComplete is the task completion percentage (0–100).
	PercentComplete int32
	// Message contains any status detail or error message from iLO for this task.
	Message string
}

// iloClientConfig holds the connection parameters for an HPE iLO Redfish client.
type iloClientConfig struct {
	// Host is the iLO IP address or hostname (without scheme or port).
	Host string
	// Username is the iLO account username.
	Username string
	// Password is the iLO account password.
	Password string
}

// hpeRepositoryUpdater abstracts all iLO Redfish operations required by the FirmwareUpdateHPE controller.
type hpeRepositoryUpdater interface {
	// GetFirmwareInventory returns all currently installed firmware components from iLO's FirmwareInventory.
	GetFirmwareInventory(ctx context.Context) ([]HPEFirmwareEntry, error)

	// GetSPPManifest fetches and parses manifest/metadata.json from the SPP repository at baseURI.
	// username and password are the HTTP credentials for the SPP server (resolved from SecretRef by the caller).
	GetSPPManifest(ctx context.Context, baseURI, username, password string) ([]SPPManifestEntry, error)

	// AddFromUri instructs iLO to fetch the package at packageURI into its ComponentRepository.
	AddFromUri(ctx context.Context, packageURI string) error

	// CreateInstallSet creates a new InstallSet in iLO containing one ApplyUpdate entry per component
	// filename plus a final ResetServer step. Returns the Redfish URI of the created InstallSet.
	CreateInstallSet(ctx context.Context, name string, componentFilenames []string) (string, error)

	// InvokeInstallSet invokes the InstallSet at installSetURI, scheduling all component updates and the
	// server reboot. Returns the Redfish URI of the invocation task in the UpdateTaskQueue.
	InvokeInstallSet(ctx context.Context, installSetURI string) (string, error)

	// GetInstallSetTasks returns the current status of all component tasks in iLO's UpdateTaskQueue.
	GetInstallSetTasks(ctx context.Context, installSetURI string) ([]HPETaskStatus, error)

	// DeleteInstallSet removes the InstallSet at installSetURI from iLO. Called on cleanup/deletion.
	DeleteInstallSet(ctx context.Context, installSetURI string) error
}

// newHPERepositoryUpdater returns a real hpeRepositoryUpdater backed by the HPE iLO Redfish API.
func newHPERepositoryUpdater(cfg iloClientConfig) hpeRepositoryUpdater {
	return &iloClient{
		host:     cfg.Host,
		username: cfg.Username,
		password: cfg.Password,
		// iLO uses self-signed TLS certificates; skip verification as is standard for iLO deployments.
		// Proxy is set to a no-op to bypass any HTTP_PROXY/HTTPS_PROXY environment variables —
		// iLO is on the management network and must be reached directly.
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
				Proxy:             func(*http.Request) (*url.URL, error) { return nil, nil },
				DisableKeepAlives: true,
			},
		},
	}
}
