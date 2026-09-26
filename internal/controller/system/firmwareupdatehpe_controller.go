// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ironcore-dev/controller-utils/clientutils"
	"github.com/ironcore-dev/controller-utils/conditionutils"
	"github.com/ironcore-dev/metal-maintenance-operator/api"
	maintenancev1alpha1 "github.com/ironcore-dev/metal-maintenance-operator/api/maintenance/v1alpha1"
	systemv1alpha1 "github.com/ironcore-dev/metal-maintenance-operator/api/system/v1alpha1"
	constants "github.com/ironcore-dev/metal-maintenance-operator/internal/constants"
	utils "github.com/ironcore-dev/metal-maintenance-operator/internal/utils"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	FirmwareUpdateHPEFinalizer = "system.metal.ironcore.dev/firmwareupdatehpe"

	// HPE-specific condition types
	ConditionHPEInstallSetInvoked = "InstallSetInvoked"

	// HPE-specific condition reasons
	ReasonHPEInstallSetInvoked  = "InstallSetInvoked"
	ReasonHPEMaxPassesReached   = "MaxRepositoryPassesReached"
	ReasonHPETasksFailed        = "ComponentTasksFailed"
	ReasonHPEDiffEmpty          = "FirmwareDiffEmpty"
	ReasonHPEInstallSetCreating = "CreatingInstallSet"

	defaultMaxRepositoryPasses int32 = 5
)

// FirmwareUpdateHPEReconciler reconciles a FirmwareUpdateHPE object.
type FirmwareUpdateHPEReconciler struct {
	client.Client
	ManagerNamespace            string
	Scheme                      *runtime.Scheme
	ResyncInterval              time.Duration
	Conditions                  *conditionutils.Accessor
	DefaultFailedAutoRetryCount int32
}

// +kubebuilder:rbac:groups=system.metal.ironcore.dev,resources=firmwareupdatehpes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=system.metal.ironcore.dev,resources=firmwareupdatehpes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=system.metal.ironcore.dev,resources=firmwareupdatehpes/finalizers,verbs=update
// +kubebuilder:rbac:groups=metal.ironcore.dev,resources=servers,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=metal.ironcore.dev,resources=bmcs,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.ironcore.dev,resources=bmcsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.ironcore.dev,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=maintenance.metal.ironcore.dev,resources=servermaintenances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maintenance.metal.ironcore.dev,resources=servermaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *FirmwareUpdateHPEReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	fw := &systemv1alpha1.FirmwareUpdateHPE{}
	if err := r.Get(ctx, req.NamespacedName, fw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.V(1).Info("Reconciling FirmwareUpdateHPE")
	return r.reconcileExists(ctx, fw)
}

func (r *FirmwareUpdateHPEReconciler) reconcileExists(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) (ctrl.Result, error) {
	ok, err := r.shouldDelete(ctx, fw)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ok {
		return r.delete(ctx, fw)
	}
	return r.reconcile(ctx, fw)
}

func (r *FirmwareUpdateHPEReconciler) shouldDelete(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) (bool, error) {
	isProgressing := func() (bool, error) {
		if fw.Status.State != systemv1alpha1.FirmwareUpdateHPEStateInProgress {
			return false, nil
		}
		if fw.Spec.ServerRef != nil {
			if _, err := utils.GetServerByName(ctx, r.Client, fw.Spec.ServerRef.Name); apierrors.IsNotFound(err) {
				return false, nil
			}
		}
		if fw.Spec.ServerMaintenanceRef == nil {
			return false, nil
		}
		return utils.IsAnyServerMaintenanceActive(ctx, r.Client, []metalv1alpha1.ObjectReference{*fw.Spec.ServerMaintenanceRef})
	}
	return utils.ShouldProceedWithDeletion(ctx, fw, FirmwareUpdateHPEFinalizer, isProgressing)
}

func (r *FirmwareUpdateHPEReconciler) delete(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Deleting FirmwareUpdateHPE")
	defer log.V(1).Info("Deleted FirmwareUpdateHPE")

	if !controllerutil.ContainsFinalizer(fw, FirmwareUpdateHPEFinalizer) {
		return ctrl.Result{}, nil
	}

	// Best-effort: delete the iLO InstallSet if one was created.
	if fw.Status.InstallSetURI != "" && fw.Spec.ServerRef != nil {
		if server, err := utils.GetServerByName(ctx, r.Client, fw.Spec.ServerRef.Name); err == nil {
			if updater, err := r.buildILOClient(ctx, server); err == nil {
				if err := updater.DeleteInstallSet(ctx, fw.Status.InstallSetURI); err != nil {
					log.V(1).Info("Failed to delete InstallSet from iLO (non-fatal, proceeding with cleanup)", "InstallSetURI", fw.Status.InstallSetURI, "error", err)
				}
			} else {
				log.V(1).Info("Failed to build iLO client for InstallSet cleanup (non-fatal)", "error", err)
			}
		} else {
			log.V(1).Info("Server not found for InstallSet cleanup (non-fatal)", "Server", fw.Spec.ServerRef.Name, "error", err)
		}
	}

	if err := r.cleanupServerMaintenanceReferences(ctx, fw); err != nil {
		return ctrl.Result{}, err
	}

	if modified, err := clientutils.PatchEnsureNoFinalizer(ctx, r.Client, fw, FirmwareUpdateHPEFinalizer); err != nil || modified {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *FirmwareUpdateHPEReconciler) cleanupServerMaintenanceReferences(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) error {
	log := ctrl.LoggerFrom(ctx)
	if fw.Spec.ServerMaintenanceRef == nil {
		return nil
	}

	serverMaintenance, err := r.getServerMaintenanceForRef(ctx, fw.Spec.ServerMaintenanceRef)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get referred ServerMaintenance: %w", err)
	}

	if serverMaintenance.DeletionTimestamp.IsZero() {
		if metav1.IsControlledBy(serverMaintenance, fw) {
			log.V(1).Info("Deleting ServerMaintenance", "ServerMaintenance", client.ObjectKeyFromObject(serverMaintenance))
			if err := r.Delete(ctx, serverMaintenance); err != nil {
				return err
			}
		} else {
			log.V(1).Info("ServerMaintenance is controlled by somebody else", "ServerMaintenance", client.ObjectKeyFromObject(serverMaintenance))
		}
	}

	if apierrors.IsNotFound(err) || err == nil {
		log.V(1).Info("Cleaning up ServerMaintenance ref in FirmwareUpdateHPE as the object is gone")
		if err := r.patchServerMaintenanceRef(ctx, fw, nil); err != nil {
			return fmt.Errorf("failed to clean up serverMaintenance ref: %w", err)
		}
	}
	return nil
}

func (r *FirmwareUpdateHPEReconciler) reconcile(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	if utils.ShouldIgnoreReconciliation(fw) {
		log.V(1).Info("Skipped FirmwareUpdateHPE reconciliation")
		return ctrl.Result{}, nil
	}

	if modified, err := clientutils.PatchEnsureFinalizer(ctx, r.Client, fw, FirmwareUpdateHPEFinalizer); err != nil || modified {
		return ctrl.Result{}, err
	}

	requeue, err := r.transitionState(ctx, fw)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
	}

	log.V(1).Info("Reconciled FirmwareUpdateHPE")
	return ctrl.Result{}, nil
}

func (r *FirmwareUpdateHPEReconciler) transitionState(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	if fw.Spec.ServerRef == nil {
		return false, fmt.Errorf("FirmwareUpdateHPE does not have a ServerRef")
	}

	server, err := utils.GetServerByName(ctx, r.Client, fw.Spec.ServerRef.Name)
	if err != nil {
		return false, fmt.Errorf("failed to fetch server: %w", err)
	}

	updater, err := r.buildILOClient(ctx, server)
	if err != nil {
		return false, fmt.Errorf("failed to build iLO client for server %s: %w", server.Name, err)
	}

	switch fw.Status.State {
	case "", systemv1alpha1.FirmwareUpdateHPEStatePending:
		// Remove the retry annotation if present so the next reconcile starts fresh.
		if utils.ShouldRetryReconciliation(fw) {
			fwBase := fw.DeepCopy()
			annotations := fw.GetAnnotations()
			delete(annotations, constants.OperationAnnotation)
			fw.SetAnnotations(annotations)
			if err := r.Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
				return true, fmt.Errorf("failed to patch FirmwareUpdateHPE for retrying: %w", err)
			}
			log.V(1).Info("Removed retry annotation from FirmwareUpdateHPE")
			return false, nil
		}
		return r.processPendingState(ctx, fw, server, updater)
	case systemv1alpha1.FirmwareUpdateHPEStateInProgress:
		return r.processInProgressState(ctx, fw, updater)
	case systemv1alpha1.FirmwareUpdateHPEStateCompleted:
		return false, r.cleanupServerMaintenanceReferences(ctx, fw)
	case systemv1alpha1.FirmwareUpdateHPEStateFailed:
		return r.processFailedState(ctx, fw, server)
	}

	log.V(1).Info("Unknown State found", "State", fw.Status.State)
	return false, nil
}

// processPendingState drives the Pending phase: request maintenance → compute diff → stage packages → create/invoke InstallSet.
func (r *FirmwareUpdateHPEReconciler) processPendingState(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, server *metalv1alpha1.Server, updater hpeRepositoryUpdater) (bool, error) {
	// Step 1: ensure server is in maintenance.
	if ok, err := r.handleServerMaintenance(ctx, fw, server); err != nil || !ok {
		return false, err
	}

	// Step 2: resolve repository credentials.
	var username, password string
	if fw.Spec.Repository.SecretRef != nil {
		var err error
		username, password, err = utils.GetImageCredentialsForSecretRef(ctx, r.Client, fw.Spec.Repository.SecretRef)
		if err != nil {
			return false, fmt.Errorf("failed to get repository credentials: %w", err)
		}
	}

	// Step 3: compute firmware diff (SPP manifest vs FirmwareInventory).
	components, err := r.handleDiff(ctx, fw, updater, username, password)
	if err != nil {
		return false, r.transitionFailed(ctx, fw, fmt.Sprintf("failed to compute firmware diff: %v", err))
	}
	if len(components) == 0 {
		if err := r.cleanupServerMaintenanceReferences(ctx, fw); err != nil {
			return false, err
		}
		return false, r.updateStatus(ctx, fw, systemv1alpha1.FirmwareUpdateHPEStateCompleted, nil)
	}

	// Step 4: stage each .fwpkg into iLO's ComponentRepository via AddFromUri.
	filenames, err := r.handleStaging(ctx, fw, components, updater)
	if err != nil {
		return false, r.transitionFailed(ctx, fw, fmt.Sprintf("failed to stage firmware packages: %v", err))
	}

	// Step 5: create an InstallSet and invoke it.
	if err := r.handleInstallSet(ctx, fw, filenames, updater); err != nil {
		return false, r.transitionFailed(ctx, fw, fmt.Sprintf("failed to create/invoke Install Set: %v", err))
	}

	// Transition to InProgress — the server is now rebooting under iLO's control.
	issuedCondition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, ConditionHPEInstallSetInvoked)
	if err != nil {
		return false, err
	}
	if err := r.Conditions.Update(issuedCondition,
		conditionutils.UpdateStatus(corev1.ConditionTrue),
		conditionutils.UpdateReason(ReasonHPEInstallSetInvoked),
		conditionutils.UpdateMessage(fmt.Sprintf("InstallSet invoked: %s", fw.Status.InstallSetURI)),
	); err != nil {
		return false, fmt.Errorf("failed to update InstallSetInvoked condition: %w", err)
	}
	return false, r.updateStatus(ctx, fw, systemv1alpha1.FirmwareUpdateHPEStateInProgress, issuedCondition)
}

// handleServerMaintenance requests a ServerMaintenance and waits for InMaintenance state.
// Returns (true, nil) when the server is confirmed in maintenance and the controller should proceed.
func (r *FirmwareUpdateHPEReconciler) handleServerMaintenance(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, server *metalv1alpha1.Server) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	if fw.Spec.ServerMaintenanceRef == nil {
		if requeue, err := r.requestServerMaintenance(ctx, fw, server); err != nil || requeue {
			return false, err
		}
	}

	condition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionServerMaintenanceWaiting)
	if err != nil {
		return false, err
	}

	maintenance, err := utils.GetServerMaintenanceForObjectReference(ctx, r.Client, fw.Spec.ServerMaintenanceRef)
	if err != nil {
		return false, fmt.Errorf("failed to get referenced ServerMaintenance: %w", err)
	}

	if maintenance.Status.State != maintenancev1alpha1.ServerMaintenanceStateInMaintenance {
		log.V(1).Info("Server not yet in maintenance", "Server", server.Name, "ServerMaintenanceState", maintenance.Status.State)
		if condition.Status != metav1.ConditionTrue {
			if err := r.Conditions.Update(condition,
				conditionutils.UpdateStatus(corev1.ConditionTrue),
				conditionutils.UpdateReason(constants.ReasonMaintenanceWaiting),
				conditionutils.UpdateMessage(fmt.Sprintf("Waiting for approval of %v", fw.Spec.ServerMaintenanceRef.Name)),
			); err != nil {
				return false, fmt.Errorf("failed to update ServerMaintenance waiting condition: %w", err)
			}
			if err := r.updateStatus(ctx, fw, fw.Status.State, condition); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if condition.Reason != constants.ReasonMaintenanceApproved {
		if err := r.Conditions.Update(condition,
			conditionutils.UpdateStatus(corev1.ConditionFalse),
			conditionutils.UpdateReason(constants.ReasonMaintenanceApproved),
			conditionutils.UpdateMessage("Server is now in Maintenance mode"),
		); err != nil {
			return false, fmt.Errorf("failed to update ServerMaintenance approved condition: %w", err)
		}
		if err := r.updateStatus(ctx, fw, fw.Status.State, condition); err != nil {
			return false, err
		}
		return false, nil
	}

	return true, nil
}

func (r *FirmwareUpdateHPEReconciler) requestServerMaintenance(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, server *metalv1alpha1.Server) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	if fw.Spec.ServerMaintenanceRef != nil {
		if _, err := utils.GetServerMaintenanceForObjectReference(ctx, r.Client, fw.Spec.ServerMaintenanceRef); apierrors.IsNotFound(err) {
			log.V(1).Info("Referenced ServerMaintenance no longer exists, clearing ref to allow re-creation")
			if err = r.patchServerMaintenanceRef(ctx, fw, nil); err != nil {
				return false, fmt.Errorf("failed to clear stale ServerMaintenance ref: %w", err)
			}
			return true, nil
		} else if err != nil {
			return false, fmt.Errorf("failed to verify ServerMaintenance existence: %w", err)
		}
		condition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionServerMaintenanceCreated)
		if err != nil {
			return false, err
		}
		if condition.Status == metav1.ConditionTrue {
			return false, nil
		}
		if err := r.Conditions.Update(condition,
			conditionutils.UpdateStatus(corev1.ConditionTrue),
			conditionutils.UpdateReason(constants.ReasonMaintenanceCreated),
			conditionutils.UpdateMessage(fmt.Sprintf("Created/Present %v", fw.Spec.ServerMaintenanceRef.Name)),
		); err != nil {
			return false, fmt.Errorf("failed to update ServerMaintenance created condition: %w", err)
		}
		if err := r.updateStatus(ctx, fw, fw.Status.State, condition); err != nil {
			return false, err
		}
		return true, nil
	}

	serverMaintenance := &maintenancev1alpha1.ServerMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.ManagerNamespace,
			Name:      fw.Name,
		},
	}
	opResult, err := controllerutil.CreateOrPatch(ctx, r.Client, serverMaintenance, func() error {
		if fw.Spec.ServerMaintenancePolicy != nil {
			serverMaintenance.Spec.Policy = *fw.Spec.ServerMaintenancePolicy
		} else {
			serverMaintenance.Spec.Policy = maintenancev1alpha1.ServerMaintenancePolicyEnforced
		}
		serverMaintenance.Spec.ServerRef = &corev1.LocalObjectReference{Name: server.Name}
		if serverMaintenance.Status.State != maintenancev1alpha1.ServerMaintenanceStateInMaintenance && serverMaintenance.Status.State != "" {
			serverMaintenance.Status.State = ""
		}
		return controllerutil.SetControllerReference(fw, serverMaintenance, r.Client.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("failed to create or patch ServerMaintenance: %w", err)
	}
	log.V(1).Info("Created ServerMaintenance", "ServerMaintenance", client.ObjectKeyFromObject(serverMaintenance), "Operation", opResult)

	if err = r.patchServerMaintenanceRef(ctx, fw, serverMaintenance); err != nil {
		return false, fmt.Errorf("failed to patch ServerMaintenance ref in FirmwareUpdateHPE spec: %w", err)
	}
	return true, nil
}

func (r *FirmwareUpdateHPEReconciler) patchServerMaintenanceRef(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, serverMaintenance *maintenancev1alpha1.ServerMaintenance) error {
	fwBase := fw.DeepCopy()
	if serverMaintenance == nil {
		fw.Spec.ServerMaintenanceRef = nil
	} else {
		fw.Spec.ServerMaintenanceRef = &metalv1alpha1.ObjectReference{
			Namespace: serverMaintenance.Namespace,
			Name:      serverMaintenance.Name,
		}
	}
	return r.Patch(ctx, fw, client.MergeFrom(fwBase))
}

func (r *FirmwareUpdateHPEReconciler) getServerMaintenanceForRef(ctx context.Context, ref *metalv1alpha1.ObjectReference) (*maintenancev1alpha1.ServerMaintenance, error) {
	if ref == nil {
		return nil, fmt.Errorf("server maintenance reference is nil")
	}
	sm := &maintenancev1alpha1.ServerMaintenance{}
	if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: r.ManagerNamespace}, sm); err != nil {
		return sm, err
	}
	return sm, nil
}

// handleDiff fetches the SPP manifest and iLO FirmwareInventory, then returns the list of components
// that need updating. It writes the result to status.diffSummary.
func (r *FirmwareUpdateHPEReconciler) handleDiff(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, updater hpeRepositoryUpdater, username, password string) ([]SPPManifestEntry, error) {
	manifest, err := updater.GetSPPManifest(ctx, fw.Spec.Repository.BaseURI, username, password)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch SPP manifest from %s: %w", fw.Spec.Repository.BaseURI, err)
	}

	inventory, err := updater.GetFirmwareInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch iLO FirmwareInventory: %w", err)
	}

	installedVersions := make(map[string]string)      // target GUID → version
	installedVersionsByClass := make(map[string]string) // DeviceClass → version
	for _, e := range inventory {
		for _, guid := range e.Targets {
			installedVersions[guid] = e.Version
		}
		if e.DeviceClass != "" {
			installedVersionsByClass[e.DeviceClass] = e.Version
		}
	}

	applyDowngrade := fw.Spec.ApplyDowngradeVersions != nil && *fw.Spec.ApplyDowngradeVersions

	var toUpdate []SPPManifestEntry
	for _, pkg := range manifest {
		if !isOOBFlashable(pkg) {
			continue
		}
		var currentVersion string
		var found bool
		// DeviceClass uniquely identifies a component type; prefer it over TargetGUIDs which
		// are frequently shared as a broadcast GUID across many unrelated components.
		if pkg.DeviceClass != "" {
			if v, ok := installedVersionsByClass[pkg.DeviceClass]; ok {
				currentVersion = v
				found = true
			}
		}
		if !found {
			for _, guid := range pkg.TargetGUIDs {
				if v, ok := installedVersions[guid]; ok {
					currentVersion = v
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}
		normalCurrent := normalizeVersion(currentVersion)
		normalTarget := normalizeVersion(pkg.Version)
		if normalCurrent == normalTarget {
			continue
		}
		if !applyDowngrade && isDowngrade(normalCurrent, normalTarget) {
			continue
		}
		toUpdate = append(toUpdate, pkg)
	}

	fwBase := fw.DeepCopy()
	fw.Status.DiffSummary = &systemv1alpha1.HPEDiffSummary{
		Total:   int32(len(toUpdate)),
		Message: fmt.Sprintf("Found %d component(s) requiring firmware update", len(toUpdate)),
	}
	fw.Status.ObservedGeneration = fw.Generation
	if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
		return nil, fmt.Errorf("failed to patch DiffSummary: %w", err)
	}

	return toUpdate, nil
}

// isOOBFlashable reports whether a component can be flashed out-of-band via iLO.
func isOOBFlashable(pkg SPPManifestEntry) bool {
	if pkg.DirectFlashOK {
		return true
	}
	for _, u := range pkg.UpdatableBy {
		if u == "Bmc" || u == "Uefi" {
			return true
		}
	}
	return false
}

// isDowngrade reports whether candidate is an older version than current.
// TODO: Implement proper HPE version string comparison; HPE versions are not semver.
// Currently uses lexicographic comparison which is only correct for simple dotted-numeric strings.
func isDowngrade(current, candidate string) bool {
	return candidate < current
}

// normalizeVersion converts an HPE version string to a canonical form for comparison.
// Handles formats such as:
//   - FirmwareInventory: "U34 v3.66 (04/01/2026)" → "3.66_04_01_2026"
//   - SPP manifest:      "3.66_04-01-2026"        → "3.66_04_01_2026"
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	// Strip a leading model prefix like "U34 " (uppercase letter + pure digits + space).
	// HPE FirmwareInventory includes the platform code before the version, e.g. "U34 v3.66 (04/01/2026)".
	if len(v) >= 2 && v[0] >= 'A' && v[0] <= 'Z' {
		end := 1
		for end < len(v) && v[end] >= '0' && v[end] <= '9' {
			end++
		}
		if end > 1 && end < len(v) && v[end] == ' ' {
			v = v[end+1:]
		}
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "V"), "v")
	var b strings.Builder
	for _, r := range v {
		switch {
		case (r >= '0' && r <= '9') || r == '.':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '/' || r == ' ' || r == '(' || r == ')':
			b.WriteRune('_')
		}
	}
	result := b.String()
	for strings.Contains(result, "__") {
		result = strings.ReplaceAll(result, "__", "_")
	}
	return strings.Trim(result, "_")
}

// handleStaging calls AddFromUri for each component in the diff.
//
// For firmware that iLO stages in ComponentRepository (iLO, NIC, storage…), AddFromUri
// returns the actual Filename; these are collected for use in CreateInstallSet ApplyUpdate entries.
//
// For firmware applied directly by iLO without ComponentRepository staging (System ROM / BIOS),
// AddFromUri returns "". These are omitted from the returned list; they are already pending
// application on the next server reboot and do not need an ApplyUpdate step.
func (r *FirmwareUpdateHPEReconciler) handleStaging(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, components []SPPManifestEntry, updater hpeRepositoryUpdater) ([]string, error) {
	filenames := make([]string, 0, len(components))
	for _, pkg := range components {
		packageURI := fw.Spec.Repository.BaseURI + "/" + pkg.PackagePath
		filename, err := updater.AddFromUri(ctx, packageURI)
		if err != nil {
			return nil, fmt.Errorf("failed to stage %s via AddFromUri: %w", pkg.Name, err)
		}
		if filename != "" {
			// Firmware staged in ComponentRepository — include in InstallSet ApplyUpdate.
			filenames = append(filenames, filename)
		}
		// filename == "": firmware staged directly (BIOS/System ROM); server reboot applies it.
	}
	return filenames, nil
}

// handleInstallSet creates an InstallSet in iLO for all staged components, invokes it, and writes
// the resulting URIs to status.installSetURI and status.invokeTaskURI.
func (r *FirmwareUpdateHPEReconciler) handleInstallSet(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, componentFilenames []string, updater hpeRepositoryUpdater) error {
	installSetName := fmt.Sprintf("hpe-fw-update-%s", fw.Name)
	installSetURI, err := updater.CreateInstallSet(ctx, installSetName, componentFilenames)
	if err != nil {
		return fmt.Errorf("failed to create InstallSet %s: %w", installSetName, err)
	}

	invokeTaskURI, err := updater.InvokeInstallSet(ctx, installSetURI)
	if err != nil {
		return fmt.Errorf("failed to invoke InstallSet %s: %w", installSetURI, err)
	}

	fwBase := fw.DeepCopy()
	fw.Status.InstallSetURI = installSetURI
	fw.Status.InvokeTaskURI = invokeTaskURI
	fw.Status.ObservedGeneration = fw.Generation
	return r.Status().Patch(ctx, fw, client.MergeFrom(fwBase))
}

// processInProgressState polls iLO's UpdateTaskQueue and drives convergence once all tasks finish.
func (r *FirmwareUpdateHPEReconciler) processInProgressState(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, updater hpeRepositoryUpdater) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	tasks, err := updater.GetInstallSetTasks(ctx, fw.Status.InstallSetURI)
	if err != nil {
		return false, fmt.Errorf("failed to poll UpdateTaskQueue: %w", err)
	}

	componentTasks, summary := buildTaskStatus(tasks)
	fwBase := fw.DeepCopy()
	fw.Status.ComponentTasks = componentTasks
	fw.Status.ComponentTasksSummary = &summary
	fw.Status.ObservedGeneration = fw.Generation
	if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
		return false, fmt.Errorf("failed to patch component task status: %w", err)
	}

	if summary.Failed > 0 {
		msg := fmt.Sprintf("%d of %d component task(s) failed", summary.Failed, summary.Total)
		log.V(1).Info(msg, "FirmwareUpdateHPE", fw.Name)
		return false, r.transitionFailed(ctx, fw, msg)
	}

	if summary.InProgress > 0 || (summary.Total > 0 && summary.Completed < summary.Total) {
		log.V(1).Info("Install Set tasks still running", "total", summary.Total, "completed", summary.Completed, "inProgress", summary.InProgress)
		return true, nil
	}

	return r.handleConvergence(ctx, fw, updater)
}

// buildTaskStatus converts raw iLO task data into CRD status types and an aggregate summary.
func buildTaskStatus(tasks []HPETaskStatus) ([]systemv1alpha1.HPEUpdateTask, systemv1alpha1.HPEUpdateTasksSummary) {
	result := make([]systemv1alpha1.HPEUpdateTask, 0, len(tasks))
	var summary systemv1alpha1.HPEUpdateTasksSummary
	summary.Total = int32(len(tasks))
	for _, t := range tasks {
		result = append(result, systemv1alpha1.HPEUpdateTask{
			URI:             t.URI,
			Name:            t.Name,
			State:           api.TaskState(t.State),
			PercentComplete: t.PercentComplete,
			Message:         t.Message,
		})
		switch t.State {
		case "Completed":
			summary.Completed++
		case "Exception", "Killed", "Cancelled":
			summary.Failed++
		default:
			summary.InProgress++
		}
	}
	return result, summary
}

// handleConvergence re-computes the firmware diff after an Install Set completes.
// A clean diff transitions to Completed; a non-empty diff triggers another pass if passCount allows.
func (r *FirmwareUpdateHPEReconciler) handleConvergence(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, updater hpeRepositoryUpdater) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	var username, password string
	if fw.Spec.Repository.SecretRef != nil {
		var err error
		username, password, err = utils.GetImageCredentialsForSecretRef(ctx, r.Client, fw.Spec.Repository.SecretRef)
		if err != nil {
			return false, fmt.Errorf("failed to get repository credentials for convergence: %w", err)
		}
	}

	components, err := r.handleDiff(ctx, fw, updater, username, password)
	if err != nil {
		return false, r.transitionFailed(ctx, fw, fmt.Sprintf("convergence diff failed: %v", err))
	}

	// Increment passCount before deciding the outcome.
	fwBase := fw.DeepCopy()
	fw.Status.PassCount++
	fw.Status.ObservedGeneration = fw.Generation
	if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
		return false, fmt.Errorf("failed to increment PassCount: %w", err)
	}

	if len(components) == 0 {
		log.V(1).Info("Firmware diff is empty after convergence pass, all components up to date", "passCount", fw.Status.PassCount)
		completedCondition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionVersionUpgradeCompleted)
		if err != nil {
			return false, err
		}
		if err := r.Conditions.Update(completedCondition,
			conditionutils.UpdateStatus(corev1.ConditionTrue),
			conditionutils.UpdateReason(ReasonHPEDiffEmpty),
			conditionutils.UpdateMessage(fmt.Sprintf("All firmware components are at the desired versions after %d pass(es)", fw.Status.PassCount)),
		); err != nil {
			return false, err
		}
		if err := r.cleanupServerMaintenanceReferences(ctx, fw); err != nil {
			return false, err
		}
		return false, r.updateStatus(ctx, fw, systemv1alpha1.FirmwareUpdateHPEStateCompleted, completedCondition)
	}

	maxPasses := defaultMaxRepositoryPasses
	if fw.Spec.MaxRepositoryPasses != nil {
		maxPasses = *fw.Spec.MaxRepositoryPasses
	}

	if fw.Status.PassCount >= maxPasses {
		msg := fmt.Sprintf("Reached maximum repository passes (%d); %d component(s) still require updating", maxPasses, len(components))
		log.V(1).Info(msg, "FirmwareUpdateHPE", fw.Name)
		return false, r.transitionFailed(ctx, fw, msg)
	}

	// Another pass needed: clear current Install Set state and transition back to Pending.
	log.V(1).Info("Firmware diff non-empty after convergence, starting another pass", "passCount", fw.Status.PassCount, "remaining", len(components))
	fwBase = fw.DeepCopy()
	fw.Status.InstallSetURI = ""
	fw.Status.InvokeTaskURI = ""
	fw.Status.ComponentTasks = nil
	fw.Status.ComponentTasksSummary = nil
	fw.Status.ObservedGeneration = fw.Generation
	if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
		return false, fmt.Errorf("failed to clear Install Set state for next pass: %w", err)
	}
	return false, r.updateStatus(ctx, fw, systemv1alpha1.FirmwareUpdateHPEStatePending, nil)
}

// transitionFailed writes a Failed condition and transitions status.state to Failed.
func (r *FirmwareUpdateHPEReconciler) transitionFailed(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, message string) error {
	ctrl.LoggerFrom(ctx).Info("FirmwareUpdateHPE transitioning to Failed", "reason", message)
	condition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionVersionUpgradeIssued)
	if err != nil {
		return err
	}
	if err := r.Conditions.Update(condition,
		conditionutils.UpdateStatus(corev1.ConditionFalse),
		conditionutils.UpdateReason(constants.ReasonUpgradeIssueFailed),
		conditionutils.UpdateMessage(message),
	); err != nil {
		return fmt.Errorf("failed to update failure condition: %w", err)
	}
	return r.updateStatus(ctx, fw, systemv1alpha1.FirmwareUpdateHPEStateFailed, condition)
}

func (r *FirmwareUpdateHPEReconciler) processFailedState(ctx context.Context, fw *systemv1alpha1.FirmwareUpdateHPE, server *metalv1alpha1.Server) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	if utils.ShouldRetryReconciliation(fw) {
		log.V(1).Info("Retrying FirmwareUpdateHPE as per annotation")
		fwBase := fw.DeepCopy()
		fw.Status.FailedAttempts = 0
		fw.Status.State = systemv1alpha1.FirmwareUpdateHPEStatePending
		fw.Status.ObservedGeneration = fw.Generation
		annotations := fw.GetAnnotations()
		retryCondition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionRetryOfFailedResourceIssued)
		if err != nil {
			return true, fmt.Errorf("failed to get retry condition: %w", err)
		}
		if retryCondition.Status != metav1.ConditionTrue {
			if err := r.Conditions.Update(retryCondition,
				conditionutils.UpdateStatus(metav1.ConditionTrue),
				conditionutils.UpdateReason(constants.ReasonRetryOfFailedResourceIssued),
				conditionutils.UpdateMessage(annotations[constants.OperationAnnotation]),
			); err != nil {
				return true, fmt.Errorf("failed to update retry condition: %w", err)
			}
		}
		fw.Status.Conditions = []metav1.Condition{*retryCondition}
		if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
			return true, fmt.Errorf("failed to patch FirmwareUpdateHPE status for retrying: %w", err)
		}
		return true, nil
	}

	var maxAttempts int32
	if fw.Spec.RetryPolicy != nil && fw.Spec.RetryPolicy.MaxAttempts != nil {
		maxAttempts = *fw.Spec.RetryPolicy.MaxAttempts
	} else if r.DefaultFailedAutoRetryCount > 0 {
		maxAttempts = r.DefaultFailedAutoRetryCount
	}

	if maxAttempts > 0 {
		if fw.Status.ObservedGeneration != fw.Generation {
			fw.Status.FailedAttempts = 0
		}
		if fw.Status.FailedAttempts < maxAttempts {
			log.V(1).Info("Auto-retrying FirmwareUpdateHPE", "FailedAttempts", fw.Status.FailedAttempts)
			fwBase := fw.DeepCopy()
			fw.Status.State = systemv1alpha1.FirmwareUpdateHPEStatePending
			fw.Status.ObservedGeneration = fw.Generation
			retryCondition, err := utils.GetCondition(r.Conditions, fw.Status.Conditions, constants.ConditionRetryOfFailedResourceIssued)
			if err != nil {
				return true, fmt.Errorf("failed to get retry condition: %w", err)
			}
			if retryCondition.Status == metav1.ConditionTrue {
				fw.Status.Conditions = []metav1.Condition{*retryCondition}
			} else {
				fw.Status.Conditions = nil
			}
			fw.Status.FailedAttempts++
			if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
				return true, fmt.Errorf("failed to patch FirmwareUpdateHPE status for auto-retry: %w", err)
			}
			return true, nil
		}
	}

	if fw.Status.FailedAttempts != 0 &&
		(maxAttempts == 0 || fw.Status.ObservedGeneration != fw.Generation) {
		fwBase := fw.DeepCopy()
		fw.Status.FailedAttempts = 0
		fw.Status.ObservedGeneration = fw.Generation
		if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
			return true, fmt.Errorf("failed to patch FirmwareUpdateHPE status for exhausted retries: %w", err)
		}
	}

	log.V(1).Info("FirmwareUpdateHPE remains in Failed state", "FirmwareUpdateHPE", fw.Name, "Server", server.Name)
	return false, nil
}

func (r *FirmwareUpdateHPEReconciler) updateStatus(
	ctx context.Context,
	fw *systemv1alpha1.FirmwareUpdateHPE,
	state systemv1alpha1.FirmwareUpdateHPEState,
	condition *metav1.Condition,
) error {
	if fw.Status.State == state && condition == nil {
		return nil
	}

	fwBase := fw.DeepCopy()
	fw.Status.State = state
	fw.Status.ObservedGeneration = fw.Generation

	if condition != nil {
		if err := r.Conditions.UpdateSlice(
			&fw.Status.Conditions,
			condition.Type,
			conditionutils.UpdateStatus(condition.Status),
			conditionutils.UpdateReason(condition.Reason),
			conditionutils.UpdateMessage(condition.Message),
		); err != nil {
			return fmt.Errorf("failed to update FirmwareUpdateHPE condition: %w", err)
		}
	} else {
		fw.Status.Conditions = []metav1.Condition{}
	}

	if err := r.Status().Patch(ctx, fw, client.MergeFrom(fwBase)); err != nil {
		return fmt.Errorf("failed to patch FirmwareUpdateHPE status: %w", err)
	}
	return nil
}

func (r *FirmwareUpdateHPEReconciler) enqueueHPEByServerRefs(ctx context.Context, obj client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)
	server := obj.(*metalv1alpha1.Server)

	if server.Status.State == metalv1alpha1.ServerStateDiscovery ||
		server.Status.State == metalv1alpha1.ServerStateError ||
		server.Status.State == metalv1alpha1.ServerStateInitial {
		return nil
	}

	fwList := &systemv1alpha1.FirmwareUpdateHPEList{}
	if err := r.List(ctx, fwList); err != nil {
		log.Error(err, "Failed to list FirmwareUpdateHPEList")
		return nil
	}

	for _, fw := range fwList.Items {
		if fw.Spec.ServerRef == nil || fw.Spec.ServerRef.Name != server.Name {
			continue
		}
		if fw.Spec.ServerMaintenanceRef == nil ||
			fw.Status.State == systemv1alpha1.FirmwareUpdateHPEStateCompleted ||
			fw.Status.State == systemv1alpha1.FirmwareUpdateHPEStateFailed {
			return nil
		}
		return []ctrl.Request{{
			NamespacedName: types.NamespacedName{Namespace: fw.Namespace, Name: fw.Name},
		}}
	}
	return nil
}

func (r *FirmwareUpdateHPEReconciler) enqueueHPEByBMC(ctx context.Context, obj client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)
	bmcObj := obj.(*metalv1alpha1.BMC)

	serverList := &metalv1alpha1.ServerList{}
	if err := clientutils.ListAndFilter(ctx, r.Client, serverList, func(object client.Object) (bool, error) {
		s := object.(*metalv1alpha1.Server)
		return s.Spec.BMCRef != nil && s.Spec.BMCRef.Name == bmcObj.Name, nil
	}); err != nil {
		log.Error(err, "Failed to list Servers for BMC", "BMC", bmcObj.Name)
		return nil
	}

	serverMap := make(map[string]struct{}, len(serverList.Items))
	for _, s := range serverList.Items {
		serverMap[s.Name] = struct{}{}
	}

	fwList := &systemv1alpha1.FirmwareUpdateHPEList{}
	if err := clientutils.ListAndFilter(ctx, r.Client, fwList, func(object client.Object) (bool, error) {
		fw := object.(*systemv1alpha1.FirmwareUpdateHPE)
		if fw.Spec.ServerRef == nil {
			return false, nil
		}
		_, exists := serverMap[fw.Spec.ServerRef.Name]
		return exists, nil
	}); err != nil {
		log.Error(err, "Failed to list FirmwareUpdateHPE objects for BMC", "BMC", bmcObj.Name)
		return nil
	}

	reqs := make([]ctrl.Request, 0)
	for _, fw := range fwList.Items {
		if fw.Status.State == systemv1alpha1.FirmwareUpdateHPEStateInProgress {
			reqs = append(reqs, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: fw.Namespace, Name: fw.Name},
			})
		}
	}
	return reqs
}

// buildILOClient resolves the iLO connection parameters for the given server and
// returns a real hpeRepositoryUpdater backed by the iLO Redfish API.
// It supports both the BMCRef (external BMC CR) and inline BMCAccess patterns.
func (r *FirmwareUpdateHPEReconciler) buildILOClient(ctx context.Context, server *metalv1alpha1.Server) (hpeRepositoryUpdater, error) {
	var host, username, password string

	if server.Spec.BMCRef != nil {
		bmc := &metalv1alpha1.BMC{}
		if err := r.Get(ctx, client.ObjectKey{Name: server.Spec.BMCRef.Name}, bmc); err != nil {
			return nil, fmt.Errorf("getting BMC %s: %w", server.Spec.BMCRef.Name, err)
		}

		switch {
		case bmc.Spec.Endpoint != nil:
			host = bmc.Spec.Endpoint.IP.String()
		case bmc.Spec.EndpointRef != nil:
			endpoint := &metalv1alpha1.Endpoint{}
			if err := r.Get(ctx, client.ObjectKey{Name: bmc.Spec.EndpointRef.Name}, endpoint); err != nil {
				return nil, fmt.Errorf("getting Endpoint %s: %w", bmc.Spec.EndpointRef.Name, err)
			}
			host = endpoint.Spec.IP.String()
		default:
			return nil, fmt.Errorf("BMC %s has neither inline endpoint nor endpointRef", bmc.Name)
		}

		bmcSecret := &metalv1alpha1.BMCSecret{}
		if err := r.Get(ctx, client.ObjectKey{Name: bmc.Spec.BMCSecretRef.Name}, bmcSecret); err != nil {
			return nil, fmt.Errorf("getting BMCSecret %s: %w", bmc.Spec.BMCSecretRef.Name, err)
		}
		username = string(bmcSecret.Data[metalv1alpha1.BMCSecretUsernameKeyName])
		password = string(bmcSecret.Data[metalv1alpha1.BMCSecretPasswordKeyName])

	} else if server.Spec.BMC != nil {
		host = server.Spec.BMC.Address
		bmcSecret := &metalv1alpha1.BMCSecret{}
		if err := r.Get(ctx, client.ObjectKey{Name: server.Spec.BMC.BMCSecretRef.Name}, bmcSecret); err != nil {
			return nil, fmt.Errorf("getting BMCSecret %s: %w", server.Spec.BMC.BMCSecretRef.Name, err)
		}
		username = string(bmcSecret.Data[metalv1alpha1.BMCSecretUsernameKeyName])
		password = string(bmcSecret.Data[metalv1alpha1.BMCSecretPasswordKeyName])

	} else {
		return nil, fmt.Errorf("server %s has neither BMCRef nor inline BMC configuration", server.Name)
	}

	return newHPERepositoryUpdater(iloClientConfig{
		Host:     host,
		Username: username,
		Password: password,
	}), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FirmwareUpdateHPEReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&systemv1alpha1.FirmwareUpdateHPE{}).
		Owns(&maintenancev1alpha1.ServerMaintenance{}).
		Watches(&metalv1alpha1.Server{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHPEByServerRefs)).
		Watches(&metalv1alpha1.BMC{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHPEByBMC)).
		Complete(r)
}
