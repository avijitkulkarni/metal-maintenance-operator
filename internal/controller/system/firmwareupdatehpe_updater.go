// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"context"
	"errors"
)

// HPEFirmwareEntry represents a single firmware component as reported by iLO's FirmwareInventory.
type HPEFirmwareEntry struct {
	// TargetGUID is HPE's stable hardware identity, used to match inventory entries against SPP manifest entries.
	TargetGUID string
	// Version is the currently installed firmware version string.
	Version string
	// Name is the human-readable component name (e.g. "System ROM", "HPE Ethernet 1Gb 4-port 331i Adapter").
	Name string
}

// SPPManifestEntry represents a single firmware package entry from the SPP manifest/metadata.json.
type SPPManifestEntry struct {
	// TargetGUID is the hardware identity this package targets; matches HPEFirmwareEntry.TargetGUID.
	TargetGUID string
	// Version is the firmware version provided by this package.
	Version string
	// Name is the human-readable component name from the manifest.
	Name string
	// PackagePath is the relative path to the .fwpkg file within the SPP repository (appended to baseURI).
	PackagePath string
	// UpdatableBy lists the update agents that can apply this package (e.g. "Bmc", "Uefi", "RuntimeAgent").
	// Only entries containing "Bmc" or "Uefi" are eligible for out-of-band flashing via iLO.
	UpdatableBy []string
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

// hpeRepositoryUpdater abstracts all iLO Redfish operations required by the FirmwareUpdateHPE controller.
//
// TODO: Move to a real implementation backed by the HPE iLO BMC client in metal-operator once
// HPE-specific Redfish operations are added to the metal-operator BMC interface.
type hpeRepositoryUpdater interface {
	// GetFirmwareInventory returns all currently installed firmware components from iLO's FirmwareInventory.
	GetFirmwareInventory(ctx context.Context) ([]HPEFirmwareEntry, error)

	// GetSPPManifest fetches and parses manifest/metadata.json from the SPP repository at baseURI.
	// username and password are passed directly; the controller resolves them from the SecretRef beforehand.
	GetSPPManifest(ctx context.Context, baseURI, username, password string) ([]SPPManifestEntry, error)

	// AddFromUri instructs iLO to pull the .fwpkg at packageURI into its ComponentRepository (staging shelf).
	// iLO fetches the file itself; no local download is needed.
	AddFromUri(ctx context.Context, packageURI string) error

	// CreateInstallSet creates a new InstallSet in iLO containing one ApplyUpdate entry per component
	// filename plus a final ResetServer step. componentFilenames are the .fwpkg filenames as stored
	// in iLO's ComponentRepository after AddFromUri. Returns the Redfish URI of the created InstallSet.
	CreateInstallSet(ctx context.Context, name string, componentFilenames []string) (string, error)

	// InvokeInstallSet invokes the InstallSet at installSetURI, scheduling all component updates and the
	// server reboot. Returns the Redfish URI of the invocation task in the UpdateTaskQueue.
	InvokeInstallSet(ctx context.Context, installSetURI string) (string, error)

	// GetInstallSetTasks returns the current status of all component tasks in iLO's UpdateTaskQueue that
	// are associated with the given installSetURI.
	GetInstallSetTasks(ctx context.Context, installSetURI string) ([]HPETaskStatus, error)

	// DeleteInstallSet removes the InstallSet at installSetURI from iLO. Called on cleanup/deletion.
	DeleteInstallSet(ctx context.Context, installSetURI string) error
}

// stubHPERepositoryUpdater is a placeholder implementation of hpeRepositoryUpdater that returns an error
// on every call. It exists so the controller compiles and the CRD can be registered before the real
// HPE iLO BMC client is available in metal-operator.
//
// Replace this with a real implementation once metal-operator exposes the HPE-specific Redfish operations.
type stubHPERepositoryUpdater struct{}

var errHPEUpdaterNotImplemented = errors.New("HPE repository updater not implemented: requires HPE iLO BMC client in metal-operator")

func (s *stubHPERepositoryUpdater) GetFirmwareInventory(_ context.Context) ([]HPEFirmwareEntry, error) {
	return nil, errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) GetSPPManifest(_ context.Context, _, _, _ string) ([]SPPManifestEntry, error) {
	return nil, errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) AddFromUri(_ context.Context, _ string) error {
	return errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) CreateInstallSet(_ context.Context, _ string, _ []string) (string, error) {
	return "", errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) InvokeInstallSet(_ context.Context, _ string) (string, error) {
	return "", errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) GetInstallSetTasks(_ context.Context, _ string) ([]HPETaskStatus, error) {
	return nil, errHPEUpdaterNotImplemented
}

func (s *stubHPERepositoryUpdater) DeleteInstallSet(_ context.Context, _ string) error {
	return errHPEUpdaterNotImplemented
}

// newHPERepositoryUpdater returns an hpeRepositoryUpdater for use by the FirmwareUpdateHPE controller.
// TODO: Accept a BMC client parameter and return a real implementation once metal-operator supports HPE iLO.
func newHPERepositoryUpdater() hpeRepositoryUpdater {
	return &stubHPERepositoryUpdater{}
}
