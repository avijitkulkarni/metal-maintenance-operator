// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// iloClient implements hpeRepositoryUpdater against the HPE iLO 5 Redfish API.
type iloClient struct {
	host       string
	username   string
	password   string
	httpClient *http.Client
}

// ─── Redfish JSON types for iLO FirmwareInventory ────────────────────────────

type iloCollection struct {
	Members []struct {
		ODataID string `json:"@odata.id"`
	} `json:"Members"`
}

type iloFirmwareEntry struct {
	Name    string `json:"Name"`
	Version string `json:"Version"`
	Oem     struct {
		Hpe struct {
			DeviceClass string   `json:"DeviceClass"`
			Targets     []string `json:"Targets"`
		} `json:"Hpe"`
	} `json:"Oem"`
}

// ─── Redfish JSON types for iLO UpdateTaskQueue ───────────────────────────────

type iloTask struct {
	ODataID         string `json:"@odata.id"`
	Name            string `json:"Name"`
	State           string `json:"State"`
	PercentComplete int32  `json:"PercentComplete"`
	Messages        []struct {
		Message string `json:"Message"`
	} `json:"Messages"`
}

// ─── Redfish JSON types for InstallSet creation ───────────────────────────────

type iloInstallSetSequenceEntry struct {
	Name        string   `json:"Name"`
	Filename    string   `json:"Filename,omitempty"`
	UpdatableBy []string `json:"UpdatableBy,omitempty"`
	Command     string   `json:"Command"`
}

type iloInstallSetBody struct {
	Name     string                       `json:"Name"`
	Sequence []iloInstallSetSequenceEntry `json:"Sequence"`
}

// ─── SPP metadata.json JSON types ────────────────────────────────────────────

type sppMetadata struct {
	Components []sppProduct `json:"Components"`
}

type sppProduct struct {
	ProductID string       `json:"ProductId"`
	Versions  []sppVersion `json:"Versions"`
}

type sppVersion struct {
	DeviceClass   string      `json:"DeviceClass"`
	Devices       sppDevices  `json:"Devices"`
	Package       sppPackage  `json:"Package"`
	PackageFormat string      `json:"PackageFormat"`
	UpdatableBy   []string    `json:"UpdatableBy"`
	VersionID     string      `json:"VersionId"`
}

type sppDevices struct {
	Device []sppDevice `json:"Device"`
}

type sppDevice struct {
	FirmwareImages []sppFirmwareImage `json:"FirmwareImages"`
}

type sppFirmwareImage struct {
	DirectFlashOK bool `json:"DirectFlashOK"`
}

type sppPackage struct {
	Files []sppFile          `json:"Files"`
	Name  []sppLocalizedText `json:"Name"`
}

type sppLocalizedText struct {
	Lang  string `json:"Lang"`
	Value string `json:"Value"`
}

type sppFile struct {
	Name        string   `json:"Name"`
	Version     string   `json:"Version"`
	TargetGUIDs []string `json:"TargetGUIDs"`
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (c *iloClient) baseURL() string {
	return "https://" + c.host
}

func (c *iloClient) url(redfishPath string) string {
	return c.baseURL() + redfishPath
}

func (c *iloClient) do(ctx context.Context, method, urlStr string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OData-Version", "4.0")
	return c.httpClient.Do(req)
}

func (c *iloClient) get(ctx context.Context, redfishPath string, out interface{}) error {
	resp, err := c.do(ctx, http.MethodGet, c.url(redfishPath), nil)
	if err != nil {
		return fmt.Errorf("GET %s: %w", redfishPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s returned %d: %s", redfishPath, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *iloClient) post(ctx context.Context, redfishPath string, bodyObj interface{}) (*http.Response, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(bodyObj); err != nil {
		return nil, fmt.Errorf("encoding POST body for %s: %w", redfishPath, err)
	}
	resp, err := c.do(ctx, http.MethodPost, c.url(redfishPath), &buf)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", redfishPath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST %s returned %d: %s", redfishPath, resp.StatusCode, body)
	}
	return resp, nil
}

func (c *iloClient) delete(ctx context.Context, redfishPath string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.url(redfishPath), nil)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", redfishPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE %s returned %d: %s", redfishPath, resp.StatusCode, body)
	}
	return nil
}

// ─── hpeRepositoryUpdater implementation ─────────────────────────────────────

// GetFirmwareInventory fetches all firmware entries from iLO's FirmwareInventory collection.
// Each entry is fetched individually because the collection only returns @odata.id references.
func (c *iloClient) GetFirmwareInventory(ctx context.Context) ([]HPEFirmwareEntry, error) {
	const inventoryPath = "/redfish/v1/UpdateService/FirmwareInventory"

	var coll iloCollection
	if err := c.get(ctx, inventoryPath, &coll); err != nil {
		return nil, fmt.Errorf("fetching FirmwareInventory collection: %w", err)
	}

	entries := make([]HPEFirmwareEntry, 0, len(coll.Members))
	for _, m := range coll.Members {
		var entry iloFirmwareEntry
		if err := c.get(ctx, m.ODataID, &entry); err != nil {
			return nil, fmt.Errorf("fetching FirmwareInventory entry %s: %w", m.ODataID, err)
		}
		if len(entry.Oem.Hpe.Targets) == 0 {
			continue
		}
		entries = append(entries, HPEFirmwareEntry{
			Targets:     entry.Oem.Hpe.Targets,
			DeviceClass: entry.Oem.Hpe.DeviceClass,
			Version:     entry.Version,
			Name:        entry.Name,
		})
	}
	return entries, nil
}

// GetSPPManifest fetches manifest/metadata.json from the SPP HTTP server and returns
// one SPPManifestEntry per firmware package that targets at least one device GUID.
func (c *iloClient) GetSPPManifest(ctx context.Context, baseURI, username, password string) ([]SPPManifestEntry, error) {
	metadataURL := baseURI + "/manifest/metadata.json"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building SPP metadata request: %w", err)
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching SPP metadata from %s: %w", metadataURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("SPP metadata server returned %d: %s", resp.StatusCode, body)
	}

	var meta sppMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding SPP metadata JSON: %w", err)
	}

	var result []SPPManifestEntry
	for _, product := range meta.Components {
		for _, ver := range product.Versions {
			// Determine if any firmware image in this version supports direct flash.
			directFlashOK := false
			for _, dev := range ver.Devices.Device {
				for _, img := range dev.FirmwareImages {
					if img.DirectFlashOK {
						directFlashOK = true
					}
				}
			}

			displayName := localizedString(ver.Package.Name, "en")

			for _, f := range ver.Package.Files {
				if len(f.TargetGUIDs) == 0 {
					continue
				}
				result = append(result, SPPManifestEntry{
					TargetGUIDs:   f.TargetGUIDs,
					Version:       f.Version,
					Name:          displayName,
					PackagePath:   f.Name,
					UpdatableBy:   ver.UpdatableBy,
					DirectFlashOK: directFlashOK,
				})
			}
		}
	}
	return result, nil
}

// localizedString picks the value for the requested language tag, falling back to the first entry.
func localizedString(entries []sppLocalizedText, lang string) string {
	for _, e := range entries {
		if e.Lang == lang {
			return e.Value
		}
	}
	if len(entries) > 0 {
		return entries[0].Value
	}
	return ""
}

// AddFromUri instructs iLO to download the package at packageURI into its ComponentRepository.
// iLO only allows one upload at a time; retries up to 10 times with a 30s backoff when busy.
func (c *iloClient) AddFromUri(ctx context.Context, packageURI string) error {
	const actionPath = "/redfish/v1/UpdateService/Actions/Oem/Hpe/HpeiLOUpdateServiceExt.AddFromUri"
	body := map[string]string{"ImageURI": packageURI}
	for attempt := 0; attempt < 10; attempt++ {
		resp, err := c.post(ctx, actionPath, body)
		if err != nil {
			if strings.Contains(err.Error(), "ComponentUploadAlreadyInProgress") {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(30 * time.Second):
					continue
				}
			}
			return fmt.Errorf("AddFromUri %s: %w", packageURI, err)
		}
		resp.Body.Close()
		return nil
	}
	return fmt.Errorf("AddFromUri %s: iLO upload slot still busy after retries", packageURI)
}

// CreateInstallSet creates an HPE InstallSet with one ApplyUpdate entry per filename
// followed by a ResetServer step. Returns the Redfish path of the created resource.
func (c *iloClient) CreateInstallSet(ctx context.Context, name string, componentFilenames []string) (string, error) {
	const installSetsPath = "/redfish/v1/UpdateService/InstallSets"

	seq := make([]iloInstallSetSequenceEntry, 0, len(componentFilenames)+1)
	for _, fn := range componentFilenames {
		seq = append(seq, iloInstallSetSequenceEntry{
			Name:        path.Base(fn),
			Filename:    fn,
			UpdatableBy: []string{"Bmc", "Uefi"},
			Command:     "ApplyUpdate",
		})
	}
	seq = append(seq, iloInstallSetSequenceEntry{
		Name:    "ResetServer",
		Command: "ResetServer",
	})

	resp, err := c.post(ctx, installSetsPath, iloInstallSetBody{Name: name, Sequence: seq})
	if err != nil {
		return "", fmt.Errorf("CreateInstallSet %s: %w", name, err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if location == "" {
		// Fall back to parsing the response body for @odata.id.
		var result struct {
			ODataID string `json:"@odata.id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.ODataID != "" {
			return result.ODataID, nil
		}
		return "", fmt.Errorf("CreateInstallSet: no Location header and no @odata.id in response")
	}
	// Location is a full URL; strip the scheme+host to get the Redfish path.
	if len(location) > len(c.baseURL()) && location[:len(c.baseURL())] == c.baseURL() {
		return location[len(c.baseURL()):], nil
	}
	return location, nil
}

// InvokeInstallSet invokes the install set at the given Redfish path.
// Returns the Redfish path of the resulting task (or empty string if iLO returns none).
func (c *iloClient) InvokeInstallSet(ctx context.Context, installSetURI string) (string, error) {
	actionPath := installSetURI + "/Actions/HpeComponentInstallSet.Invoke"
	body := map[string]bool{"ClearTaskQueue": true}

	resp, err := c.post(ctx, actionPath, body)
	if err != nil {
		return "", fmt.Errorf("InvokeInstallSet %s: %w", installSetURI, err)
	}
	defer resp.Body.Close()

	// iLO may return a task location in the Location header or in the body.
	if location := resp.Header.Get("Location"); location != "" {
		if len(location) > len(c.baseURL()) && location[:len(c.baseURL())] == c.baseURL() {
			return location[len(c.baseURL()):], nil
		}
		return location, nil
	}
	return "", nil
}

// GetInstallSetTasks returns the current status of all tasks in the UpdateTaskQueue.
// Because iLO's invoke clears the queue first (ClearTaskQueue: true), all entries
// in the queue belong to the most recently invoked install set.
func (c *iloClient) GetInstallSetTasks(ctx context.Context, _ string) ([]HPETaskStatus, error) {
	return c.fetchTaskQueue(ctx)
}

func (c *iloClient) fetchTaskQueue(ctx context.Context) ([]HPETaskStatus, error) {
	const taskQueuePath = "/redfish/v1/UpdateService/UpdateTaskQueue"

	var coll iloCollection
	if err := c.get(ctx, taskQueuePath, &coll); err != nil {
		return nil, fmt.Errorf("fetching UpdateTaskQueue: %w", err)
	}

	tasks := make([]HPETaskStatus, 0, len(coll.Members))
	for _, m := range coll.Members {
		var t iloTask
		if err := c.get(ctx, m.ODataID, &t); err != nil {
			return nil, fmt.Errorf("fetching task %s: %w", m.ODataID, err)
		}
		var msg string
		if len(t.Messages) > 0 {
			msg = t.Messages[0].Message
		}
		tasks = append(tasks, HPETaskStatus{
			URI:             t.ODataID,
			Name:            t.Name,
			State:           t.State,
			PercentComplete: t.PercentComplete,
			Message:         msg,
		})
	}
	return tasks, nil
}

// DeleteInstallSet removes the install set at the given Redfish path from iLO.
func (c *iloClient) DeleteInstallSet(ctx context.Context, installSetURI string) error {
	if err := c.delete(ctx, installSetURI); err != nil {
		return fmt.Errorf("DeleteInstallSet %s: %w", installSetURI, err)
	}
	return nil
}
