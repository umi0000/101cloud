package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const cloudInitDisabledPath = "/etc/cloud/cloud-init.disabled"

const cloudInitHostnamePath = "/etc/cloud/cloud.cfg.d/99-101cloud-preserve-hostname.cfg"
const cloudInitHostnameContent = "preserve_hostname: true\n"

type cloudInitPrepareRequest struct {
	Project                  string `json:"project"`
	Name                     string `json:"name"`
	ExpectedUUID             string `json:"expected_uuid"`
	ExpectedInstanceID       string `json:"expected_instance_id"`
	ExpectedImageFingerprint string `json:"expected_image_fingerprint"`
}

type cloudInitPrepareResult struct {
	Checked               bool `json:"checked"`
	DisabledMarkerRemoved bool `json:"disabled_marker_removed"`
}

// Only the file operations needed for preparation are exposed here. No guest
// execution, arbitrary path, source-image mutation or instance start is allowed.
type cloudInitClient interface {
	GetInstance(string) (*api.Instance, string, error)
	GetInstanceFile(string, string) (io.ReadCloser, *incus.InstanceFileResponse, error)
	CreateInstanceFile(string, string, incus.InstanceFileArgs) error
	DeleteInstanceFile(string, string) error
}

func decodeCloudInitPrepare(w http.ResponseWriter, r *http.Request) (cloudInitPrepareRequest, error) {
	var request cloudInitPrepareRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return request, errors.New("request must be a JSON object")
	}
	fields := map[string]*string{
		"project": &request.Project, "name": &request.Name,
		"expected_uuid": &request.ExpectedUUID, "expected_instance_id": &request.ExpectedInstanceID,
		"expected_image_fingerprint": &request.ExpectedImageFingerprint,
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return request, err
		}
		key, ok := token.(string)
		field := fields[key]
		if !ok || field == nil || seen[key] {
			return request, fmt.Errorf("unknown or duplicate field %q", key)
		}
		seen[key] = true
		if err := decoder.Decode(field); err != nil {
			return request, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return request, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return request, errors.New("only one JSON object is permitted")
	}
	for key, field := range fields {
		if !seen[key] || *field == "" || strings.TrimSpace(*field) != *field || strings.ContainsAny(*field, "/\\\x00") || *field == "." || *field == ".." {
			return request, fmt.Errorf("%s must be a non-empty identity without path separators", key)
		}
	}
	if !fullImageFingerprint(request.ExpectedImageFingerprint) {
		return request, errors.New("expected_image_fingerprint must be a full image fingerprint")
	}
	return request, nil
}

func (s *Service) prepareCloudInit(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusBadRequest, "invalid_cloud_init_prepare", "query parameters are not permitted; project is required in the body")
		return
	}
	request, err := decodeCloudInitPrepare(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cloud_init_prepare", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
	defer cancel()
	release, err := s.acquire(ctx, instanceKey{project: request.Project, name: request.Name})
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, "cloud_init_prepare_failed", err.Error())
		return
	}
	defer release()
	client, err := s.connect(ctx, request.Project)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud_init_prepare_failed", err.Error())
		return
	}
	defer closeIncus(client)
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud_init_prepare_failed", err.Error())
		return
	}
	// SDK v6 accepts some non-200 file responses without an error. Validate
	// error envelopes before its file parser can mistake them for a file.
	httpClient.Transport = cloudInitFileTransport{base: httpClient.Transport}
	result, err := prepareCloudInit(client, request)
	if err != nil {
		writeError(w, upstreamStatus(err), "cloud_init_prepare_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func checkCloudInitIdentity(client cloudInitClient, request cloudInitPrepareRequest) error {
	instance, _, err := client.GetInstance(request.Name)
	if err != nil {
		return err
	}
	if instance == nil || instance.Name != request.Name || instance.Project != request.Project ||
		instance.Config["volatile.uuid"] != request.ExpectedUUID ||
		instance.Config["user.101cloud.instance_id"] != request.ExpectedInstanceID ||
		!strings.EqualFold(instance.Config["volatile.base_image"], request.ExpectedImageFingerprint) {
		return api.StatusErrorf(http.StatusConflict, "instance project, UUID, ownership or base image does not match")
	}
	if instance.StatusCode != api.Stopped || instance.Status != "Stopped" {
		return api.StatusErrorf(http.StatusConflict, "cloud-init preparation requires a stopped new instance")
	}
	if instance.Type != "container" {
		return api.StatusErrorf(http.StatusUnprocessableEntity, "stopped-instance file preparation is supported only for containers, not virtual machines")
	}
	return nil
}

type cloudInitFileTransport struct {
	base http.RoundTripper
}

func (t cloudInitFileTransport) CloseIdleConnections() {
	if transport, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

func (t cloudInitFileTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || !strings.HasSuffix(request.URL.Path, "/files") || response.StatusCode == http.StatusOK {
		return response, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBody+1))
	if err != nil || len(data) > maxRequestBody {
		return nil, fmt.Errorf("invalid Incus file response: HTTP %d", response.StatusCode)
	}
	var envelope api.Response
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Type != api.ErrorResponse || envelope.Code != response.StatusCode {
		return nil, fmt.Errorf("untyped Incus file failure: HTTP %d", response.StatusCode)
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func cloudInitFileExists(client cloudInitClient, name, path string) (bool, error) {
	body, info, err := client.GetInstanceFile(name, path)
	if body != nil {
		defer body.Close()
	}
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info == nil || info.Type != "file" {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}

// User-data is merged after cloud-init's local-stage hostname attempt. Install
// this fixed system configuration before boot, without replacing guest config.
func prepareCloudInitHostname(client cloudInitClient, request cloudInitPrepareRequest) error {
	for _, path := range []string{"/etc", "/etc/cloud", "/etc/cloud/cloud.cfg.d"} {
		body, info, err := client.GetInstanceFile(request.Name, path)
		if body != nil {
			body.Close()
		}
		if err != nil {
			return err
		}
		if info == nil || info.Type != "directory" || info.UID != 0 || info.GID != 0 || info.Mode&0022 != 0 {
			return api.StatusErrorf(http.StatusConflict, "cloud-init hostname configuration requires root-owned non-writable parent directories")
		}
	}
	exists, err := cloudInitHostnameExists(client, request.Name)
	if err != nil || exists {
		return err
	}
	if err := checkCloudInitIdentity(client, request); err != nil {
		return err
	}
	if err := client.CreateInstanceFile(request.Name, cloudInitHostnamePath, incus.InstanceFileArgs{
		Content: strings.NewReader(cloudInitHostnameContent), UID: 0, GID: 0, Mode: 0644, Type: "file", WriteMode: "overwrite",
	}); err != nil {
		return err
	}
	exists, err = cloudInitHostnameExists(client, request.Name)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("cloud-init hostname configuration write was not confirmed")
	}
	return nil
}

func cloudInitHostnameExists(client cloudInitClient, name string) (bool, error) {
	body, info, err := client.GetInstanceFile(name, cloudInitHostnamePath)
	if body != nil {
		defer body.Close()
	}
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if body == nil || info == nil || info.Type != "file" || info.UID != 0 || info.GID != 0 || info.Mode&07777 != 0644 {
		return false, api.StatusErrorf(http.StatusConflict, "existing platform hostname configuration has unsafe type, ownership or mode; it was not replaced")
	}
	content, err := io.ReadAll(io.LimitReader(body, int64(len(cloudInitHostnameContent)+1)))
	if err != nil {
		return false, err
	}
	if string(content) != cloudInitHostnameContent {
		return false, api.StatusErrorf(http.StatusConflict, "existing platform hostname configuration has different content; it was not replaced")
	}
	return true, nil
}

func prepareCloudInit(client cloudInitClient, request cloudInitPrepareRequest) (cloudInitPrepareResult, error) {
	var result cloudInitPrepareResult
	if err := checkCloudInitIdentity(client, request); err != nil {
		return result, err
	}
	// Both installation witnesses are required; missing cloud-init is a failed
	// preparation, never permission to delete its disabled marker.
	for _, path := range []string{"/usr/bin/cloud-init", "/etc/cloud/cloud.cfg"} {
		exists, err := cloudInitFileExists(client, request.Name, path)
		if err != nil {
			return result, err
		}
		if !exists {
			return result, api.StatusErrorf(http.StatusUnprocessableEntity, "cloud-init is not installed: %s is missing", path)
		}
	}
	if err := prepareCloudInitHostname(client, request); err != nil {
		return result, err
	}
	exists, err := cloudInitFileExists(client, request.Name, cloudInitDisabledPath)
	if err != nil {
		return result, err
	}
	// Recheck identity and state after file reads, including idempotent calls.
	if err := checkCloudInitIdentity(client, request); err != nil {
		return result, err
	}
	if exists {
		err = client.DeleteInstanceFile(request.Name, cloudInitDisabledPath)
		if err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
			return result, err
		}
		result.DisabledMarkerRemoved = err == nil
	}
	result.Checked = true
	return result, nil
}
