// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ironcore-dev/metal-maintenance-operator/api"
	maintenancev1alpha1 "github.com/ironcore-dev/metal-maintenance-operator/api/maintenance/v1alpha1"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
)

// FirmwareUpdateHPEState describes the current state of a FirmwareUpdateHPE.
type FirmwareUpdateHPEState string

const (
	// FirmwareUpdateHPEStatePending specifies that the firmware update is waiting to begin.
	FirmwareUpdateHPEStatePending FirmwareUpdateHPEState = "Pending"
	// FirmwareUpdateHPEStateInProgress specifies that the firmware update is in progress.
	FirmwareUpdateHPEStateInProgress FirmwareUpdateHPEState = "InProgress"
	// FirmwareUpdateHPEStateCompleted specifies that the firmware update has completed successfully.
	FirmwareUpdateHPEStateCompleted FirmwareUpdateHPEState = "Completed"
	// FirmwareUpdateHPEStateFailed specifies that the firmware update has failed.
	FirmwareUpdateHPEStateFailed FirmwareUpdateHPEState = "Failed"
)

// HPERepositorySpec defines the remote SPP repository from which firmware packages are fetched.
type HPERepositorySpec struct {
	// BaseURI is the HTTP/HTTPS base URL of the SPP repository.
	// The controller appends relative paths (e.g. manifest/metadata.json, <component>.fwpkg) to this URI.
	// +required
	BaseURI string `json:"baseURI"`

	// SecretRef optionally references a Secret containing HTTP credentials (keys: username, password).
	// +optional
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`
}

// FirmwareUpdateHPETemplate defines repository and update behavior shared across HPE firmware update resources.
type FirmwareUpdateHPETemplate struct {
	// Repository specifies the remote SPP repository from which firmware packages are fetched.
	// +required
	Repository HPERepositorySpec `json:"repository"`

	// ApplyDowngradeVersions controls whether firmware packages older than currently installed
	// versions are included in the Install Set. Defaults to false.
	// +optional
	ApplyDowngradeVersions *bool `json:"applyDowngradeVersions,omitempty"`

	// ServerMaintenancePolicy is the maintenance policy to be enforced on the server.
	// +optional
	ServerMaintenancePolicy *maintenancev1alpha1.ServerMaintenancePolicy `json:"serverMaintenancePolicy,omitempty"`

	// RetryPolicy defines the retry behavior for automatic retries on transient failures.
	// +optional
	RetryPolicy *api.RetryPolicy `json:"retryPolicy,omitempty"`

	// MaxRepositoryPasses is the maximum number of repository convergence passes the controller will
	// attempt before declaring a stall. Defaults to 5.
	// +optional
	MaxRepositoryPasses *int32 `json:"maxRepositoryPasses,omitempty"`
}

// FirmwareUpdateHPESpec defines the desired state of FirmwareUpdateHPE.
type FirmwareUpdateHPESpec struct {
	// FirmwareUpdateHPETemplate defines the repository and update parameters.
	FirmwareUpdateHPETemplate `json:",inline"`

	// ServerMaintenanceRef is a reference to the ServerMaintenance object the controller created for this update.
	// +optional
	ServerMaintenanceRef *metalv1alpha1.ObjectReference `json:"serverMaintenanceRef,omitempty"`

	// ServerRef is a reference to the HPE server on which firmware will be updated.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serverRef is immutable"
	// +required
	ServerRef *corev1.LocalObjectReference `json:"serverRef"`
}

// HPEUpdateTask represents the status of a single firmware component update task in iLO's UpdateTaskQueue.
type HPEUpdateTask struct {
	// URI is the Redfish URI of this task in the iLO UpdateTaskQueue.
	// +optional
	URI string `json:"uri,omitempty"`

	// Name is the human-readable name of the firmware component being updated.
	// +optional
	Name string `json:"name,omitempty"`

	// State is the current state of the task as reported by iLO.
	// +optional
	State api.TaskState `json:"state,omitempty"`

	// PercentComplete is the completion percentage of the task (0-100).
	// +optional
	PercentComplete int32 `json:"percentComplete,omitempty"`

	// Message contains any status message or error detail from iLO for this task.
	// +optional
	Message string `json:"message,omitempty"`
}

// HPEUpdateTasksSummary provides aggregate counts of component task states.
type HPEUpdateTasksSummary struct {
	// Total is the total number of component tasks in the Install Set.
	Total int32 `json:"total"`

	// Completed is the number of tasks that have completed successfully.
	Completed int32 `json:"completed"`

	// InProgress is the number of tasks currently running.
	InProgress int32 `json:"inProgress"`

	// Failed is the number of tasks that have failed.
	Failed int32 `json:"failed"`
}

// HPEDiffSummary summarises the firmware diff computed from the SPP manifest vs. FirmwareInventory.
type HPEDiffSummary struct {
	// Total is the number of components identified as requiring an update.
	Total int32 `json:"total"`

	// Message provides a human-readable summary of the diff result.
	// +optional
	Message string `json:"message,omitempty"`
}

// FirmwareUpdateHPEStatus defines the observed state of FirmwareUpdateHPE.
type FirmwareUpdateHPEStatus struct {
	// State represents the current phase of the firmware update workflow.
	// +optional
	State FirmwareUpdateHPEState `json:"state,omitempty"`

	// DiffSummary describes the firmware diff computed against the SPP manifest during the last pass.
	// +optional
	DiffSummary *HPEDiffSummary `json:"diffSummary,omitempty"`

	// InstallSetURI is the Redfish URI of the Install Set created in iLO for batch firmware application.
	// +optional
	InstallSetURI string `json:"installSetURI,omitempty"`

	// InvokeTaskURI is the Redfish URI of the task created when the Install Set was invoked.
	// +optional
	InvokeTaskURI string `json:"invokeTaskURI,omitempty"`

	// ComponentTasks contains per-component task status reported by iLO's UpdateTaskQueue.
	// +optional
	ComponentTasks []HPEUpdateTask `json:"componentTasks,omitempty"`

	// ComponentTasksSummary provides aggregate counts across all component tasks.
	// +optional
	ComponentTasksSummary *HPEUpdateTasksSummary `json:"componentTasksSummary,omitempty"`

	// PassCount is the number of repository convergence passes completed in the current run.
	// +optional
	PassCount int32 `json:"passCount,omitempty"`

	// FailedAttempts is the number of automatic retry attempts made after failure.
	// +optional
	FailedAttempts int32 `json:"failedAttempts,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represents the latest available observations of the firmware update state.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fwuh
// +kubebuilder:printcolumn:name="ServerRef",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="ServerMaintenanceRef",type=string,JSONPath=`.spec.serverMaintenanceRef.name`
// +kubebuilder:printcolumn:name="PassCount",type=integer,JSONPath=`.status.passCount`
// +kubebuilder:printcolumn:name="DiffTotal",type=integer,JSONPath=`.status.diffSummary.total`
// +kubebuilder:printcolumn:name="TasksTotal",type=integer,JSONPath=`.status.componentTasksSummary.total`
// +kubebuilder:printcolumn:name="TasksDone",type=integer,JSONPath=`.status.componentTasksSummary.completed`
// +kubebuilder:printcolumn:name="TasksFailed",type=integer,JSONPath=`.status.componentTasksSummary.failed`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FirmwareUpdateHPE is the Schema for the firmwareupdatehpes API.
type FirmwareUpdateHPE struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FirmwareUpdateHPESpec   `json:"spec,omitempty"`
	Status FirmwareUpdateHPEStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FirmwareUpdateHPEList contains a list of FirmwareUpdateHPE.
type FirmwareUpdateHPEList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FirmwareUpdateHPE `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FirmwareUpdateHPE{}, &FirmwareUpdateHPEList{})
}
