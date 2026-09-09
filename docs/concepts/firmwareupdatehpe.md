# FirmwareUpdateHPE

`FirmwareUpdateHPE` updates the firmware of multiple components (NIC, BIOS, PSU, disk, storage controllers) on
exactly one HPE `Server` in a single reboot cycle, by fetching packages from a remote SPP (Service Pack for
ProLiant) repository and applying them through the server's iLO Redfish interface.

## Key Concepts

### SPP Repository
The [SPP](https://support.hpe.com/hpesc/public/home) is HPE's firmware bundle for ProLiant servers.
The controller expects it to be hosted at an HTTP/HTTPS URL with the standard SPP layout:
- `<baseURI>/manifest/metadata.json` — component manifest listing ~800 firmware packages with target GUIDs,
  versions, and `UpdatableBy` metadata.
- `<baseURI>/<path>/<component>.fwpkg` — individual firmware packages.

### iLO Redfish Flow
Unlike single-component upgrades, HPE uses a **staged batch** approach entirely through iLO:

1. **AddFromUri** — each `.fwpkg` is pushed to iLO's `ComponentRepository` (staging shelf) via a Redfish action
   pointing to the external URL; iLO pulls the file itself.
2. **Install Set** — a Redfish `InstallSet` is created containing an ordered sequence of `ApplyUpdate` steps for
   all staged components plus a final `ResetServer` step.
3. **Invoke** — the Install Set is invoked; iLO schedules all steps and reboots the server once at the end.
4. **UpdateTaskQueue** — iLO populates per-component tasks that the controller polls until all succeed or one fails.

### UpdatableBy Filter
Only components marked `UpdatableBy: Bmc` or `UpdatableBy: Uefi` in the manifest are staged — approximately
140 out-of-band-flashable components. Components marked `UpdatableBy: RuntimeAgent` (requiring an OS agent) are
excluded.

### Target GUID
Each component in the SPP manifest carries a stable hardware identity (Target GUID) that encodes PCI vendor/device
IDs. The controller matches manifest entries against iLO's `FirmwareInventory` using this GUID to compute the diff.

### Convergence Passes
After a successful Install Set completes, the controller re-fetches the FirmwareInventory and recomputes the diff
to detect any components that still need updating (e.g. failed silently). This loop is bounded by
`spec.maxRepositoryPasses` (default 5).

## Key Points

- `FirmwareUpdateHPE` is cluster-scoped and immutably bound to one server via `spec.serverRef`.
- `spec.repository.baseURI` is the HTTP/HTTPS root of the hosted SPP bundle.
- `spec.repository.secretRef` optionally references a Secret with keys `username` and `password` for HTTP auth.
- `spec.applyDowngradeVersions` controls whether packages older than current installed versions are included.
  Defaults to `false`.
- `spec.serverMaintenancePolicy` controls how maintenance is requested (`Enforced` or `OwnerApproval`), same
  semantics as [`ServerMaintenance`](servermaintenance.md).
- `spec.serverMaintenanceRef` is managed by the controller; do not set manually.
- `spec.retryPolicy.maxAttempts` bounds automatic retries after transient failure.
- `spec.maxRepositoryPasses` bounds convergence iterations after an Install Set completes. Defaults to 5.
- `status.state` reflects the overall lifecycle: `Pending`, `InProgress`, `Completed`, or `Failed`.
- `status.diffSummary` reports the number of components identified as needing an update in the last pass.
- `status.installSetURI` and `status.invokeTaskURI` are the Redfish URIs of the active Install Set and its
  invocation task in iLO.
- `status.componentTasks` and `status.componentTasksSummary` reflect per-component task state from iLO's
  UpdateTaskQueue.
- `status.passCount` increments on each convergence pass; resets to zero on a clean diff.
- `status.failedAttempts` counts automatic retry attempts made after failure.

## Workflow

1. The controller requests (or reuses) `ServerMaintenance` for the server per `spec.serverMaintenancePolicy`.
2. Once the server is in maintenance, the controller fetches `<baseURI>/manifest/metadata.json` and compares it
   against iLO's `FirmwareInventory` using Target GUIDs to compute which components need updating.
3. For each component in the diff, the controller calls iLO's `AddFromUri` Redfish action to stage the `.fwpkg`
   from the remote repository into iLO's `ComponentRepository`.
4. Once all packages are staged, the controller creates an `InstallSet` in iLO containing one `ApplyUpdate` entry
   per staged component and a final `ResetServer` step, then invokes it.
5. The controller polls iLO's `UpdateTaskQueue` until all component tasks complete (or a failure is detected).
6. After tasks complete, the controller re-fetches `FirmwareInventory` and recomputes the diff (convergence pass).
   If no components remain, `status.state` becomes `Completed`. If components remain and `status.passCount`
   is below `spec.maxRepositoryPasses`, another pass begins. Otherwise, the resource transitions to `Failed`.
7. On deletion, the controller deletes any active Install Set in iLO, cleans up the `ServerMaintenance` it
   requested, and removes its finalizer.

## Example

```yaml
apiVersion: system.metal.ironcore.dev/v1alpha1
kind: FirmwareUpdateHPE
metadata:
  name: hpe-firmware-update-gen10
spec:
  serverRef:
    name: my-hpe-proliant-server
  repository:
    baseURI: https://spp.example.internal/spp-2024-11
    secretRef:
      name: spp-http-credentials
      namespace: metal-operator-system
  serverMaintenancePolicy: OwnerApproval
  applyDowngradeVersions: false
  maxRepositoryPasses: 3
  retryPolicy:
    maxAttempts: 2
```

## Implementation Progress

| Step | File | Status |
|------|------|--------|
| 1 — CRD types | `api/system/v1alpha1/firmwareupdatehpe_types.go` | Done |
| 2 — DeepCopy methods | `api/system/v1alpha1/zz_generated.deepcopy.go` | Done |
| 3 — Stub updater interface | `internal/controller/system/firmwareupdatehpe_updater.go` | Done |
| 4 — Controller | `internal/controller/system/firmwareupdatehpe_controller.go` | Done |
| 5 — Scheme + registration | `cmd/main.go` | Done |
