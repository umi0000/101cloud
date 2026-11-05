package node

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type cloudInitFixture struct {
	instance     api.Instance
	files        map[string]error
	reads        []string
	deletes      []string
	observations int
	change       func(*api.Instance)
	deleteError  error
	fileType     string
	writes       []string
	contents     map[string]string
	infos        map[string]*incus.InstanceFileResponse
	writeError   error
}

func newCloudInitFixture() (*cloudInitFixture, cloudInitPrepareRequest) {
	request := cloudInitPrepareRequest{Project: "tenant", Name: "worker", ExpectedUUID: "new-uuid", ExpectedInstanceID: "42", ExpectedImageFingerprint: strings.Repeat("a", 64)}
	return &cloudInitFixture{
		instance: api.Instance{Name: request.Name, Project: request.Project, Type: "container", Status: "Stopped", StatusCode: api.Stopped,
			InstancePut: api.InstancePut{Config: map[string]string{"volatile.uuid": request.ExpectedUUID, "user.101cloud.instance_id": request.ExpectedInstanceID, "volatile.base_image": request.ExpectedImageFingerprint}}},
		files: map[string]error{cloudInitHostnamePath: api.StatusErrorf(404, "missing")}, fileType: "file",
		contents: make(map[string]string), infos: make(map[string]*incus.InstanceFileResponse),
	}, request
}

func (f *cloudInitFixture) GetInstance(string) (*api.Instance, string, error) {
	f.observations++
	if f.observations == 2 && f.change != nil {
		f.change(&f.instance)
	}
	return &f.instance, "etag", nil
}

func (f *cloudInitFixture) GetInstanceFile(name, path string) (io.ReadCloser, *incus.InstanceFileResponse, error) {
	f.reads = append(f.reads, path)
	if err := f.files[path]; err != nil {
		return nil, nil, err
	}
	info := f.infos[path]
	if info == nil {
		info = &incus.InstanceFileResponse{Type: f.fileType, Mode: 0644}
		if path == "/etc" || path == "/etc/cloud" || path == "/etc/cloud/cloud.cfg.d" {
			info = &incus.InstanceFileResponse{Type: "directory", Mode: 0755}
		}
	}
	return io.NopCloser(strings.NewReader(f.contents[path])), info, nil
}

func (f *cloudInitFixture) DeleteInstanceFile(name, path string) error {
	f.deletes = append(f.deletes, path)
	if f.deleteError != nil {
		return f.deleteError
	}
	f.files[path] = api.StatusErrorf(404, "missing")
	return nil
}

func (f *cloudInitFixture) CreateInstanceFile(name, path string, args incus.InstanceFileArgs) error {
	f.writes = append(f.writes, path)
	if f.writeError != nil {
		return f.writeError
	}
	content, err := io.ReadAll(args.Content)
	if err != nil {
		return err
	}
	delete(f.files, path)
	f.contents[path] = string(content)
	f.infos[path] = &incus.InstanceFileResponse{Type: args.Type, UID: args.UID, GID: args.GID, Mode: args.Mode}
	return nil
}

func TestCloudInitPrepareFixedPathsAndIdempotency(t *testing.T) {
	fixture, request := newCloudInitFixture()
	result, err := prepareCloudInit(fixture, request)
	if err != nil || !result.Checked || !result.DisabledMarkerRemoved {
		t.Fatalf("preparation: %+v %v", result, err)
	}
	if !reflect.DeepEqual(fixture.reads, []string{"/usr/bin/cloud-init", "/etc/cloud/cloud.cfg", "/etc", "/etc/cloud", "/etc/cloud/cloud.cfg.d", cloudInitHostnamePath, cloudInitHostnamePath, cloudInitDisabledPath}) || !reflect.DeepEqual(fixture.deletes, []string{cloudInitDisabledPath}) || !reflect.DeepEqual(fixture.writes, []string{cloudInitHostnamePath}) || fixture.contents[cloudInitHostnamePath] != cloudInitHostnameContent || fixture.infos[cloudInitHostnamePath].Mode != 0644 || fixture.observations != 3 {
		t.Fatalf("unexpected access: %+v", fixture)
	}
	result, err = prepareCloudInit(fixture, request)
	if err != nil || !result.Checked || result.DisabledMarkerRemoved || len(fixture.deletes) != 1 || len(fixture.writes) != 1 {
		t.Fatalf("idempotent preparation: %+v %v deletes=%v", result, err, fixture.deletes)
	}
}

func TestCloudInitHostnamePreparationRejectsConflicts(t *testing.T) {
	for _, scenario := range []string{"content", "symlink", "owner", "group", "mode", "oversized", "parent-type", "parent-owner", "parent-group", "parent-writable", "parent-missing", "read-error", "write-error"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, request := newCloudInitFixture()
			if scenario != "write-error" {
				delete(fixture.files, cloudInitHostnamePath)
				fixture.contents[cloudInitHostnamePath] = cloudInitHostnameContent
				fixture.infos[cloudInitHostnamePath] = &incus.InstanceFileResponse{Type: "file", Mode: 0644}
			}
			switch scenario {
			case "content":
				fixture.contents[cloudInitHostnamePath] = "preserve_hostname: false\n"
			case "symlink":
				fixture.infos[cloudInitHostnamePath].Type = "symlink"
			case "owner":
				fixture.infos[cloudInitHostnamePath].UID = 1000
			case "group":
				fixture.infos[cloudInitHostnamePath].GID = 1000
			case "mode":
				fixture.infos[cloudInitHostnamePath].Mode = 0666
			case "oversized":
				fixture.contents[cloudInitHostnamePath] += strings.Repeat("x", 1000)
			case "parent-type":
				fixture.infos["/etc/cloud/cloud.cfg.d"] = &incus.InstanceFileResponse{Type: "symlink", Mode: 0755}
			case "parent-owner":
				fixture.infos["/etc/cloud/cloud.cfg.d"] = &incus.InstanceFileResponse{Type: "directory", UID: 1000, Mode: 0755}
			case "parent-group":
				fixture.infos["/etc/cloud/cloud.cfg.d"] = &incus.InstanceFileResponse{Type: "directory", GID: 1000, Mode: 0755}
			case "parent-writable":
				fixture.infos["/etc/cloud/cloud.cfg.d"] = &incus.InstanceFileResponse{Type: "directory", Mode: 0777}
			case "parent-missing":
				fixture.files["/etc/cloud/cloud.cfg.d"] = api.StatusErrorf(404, "missing")
			case "read-error":
				fixture.files[cloudInitHostnamePath] = api.StatusErrorf(403, "forbidden")
			case "write-error":
				fixture.writeError = api.StatusErrorf(500, "failure")
			}
			if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.deletes) != 0 {
				t.Fatalf("unsafe hostname preparation removed disabled marker: %v", err)
			}
			if scenario != "write-error" && len(fixture.writes) != 0 {
				t.Fatal("existing or unsafe guest configuration was overwritten")
			}
		})
	}
}

func TestCloudInitHostnameExactExistingConfigurationIsNotRewritten(t *testing.T) {
	fixture, request := newCloudInitFixture()
	delete(fixture.files, cloudInitHostnamePath)
	fixture.contents[cloudInitHostnamePath] = cloudInitHostnameContent
	if result, err := prepareCloudInit(fixture, request); err != nil || !result.Checked || len(fixture.writes) != 0 {
		t.Fatalf("exact root-owned configuration was not idempotent: %+v %v", result, err)
	}
}

func TestCloudInitPrepareRejectsUnsafeIdentity(t *testing.T) {
	for _, field := range []string{"name", "project", "uuid", "owner", "image", "running", "inconsistent_state", "vm", "unknown_type"} {
		t.Run(field, func(t *testing.T) {
			fixture, request := newCloudInitFixture()
			switch field {
			case "name":
				fixture.instance.Name = "other"
			case "project":
				fixture.instance.Project = "default"
			case "uuid":
				fixture.instance.Config["volatile.uuid"] = "other"
			case "owner":
				delete(fixture.instance.Config, "user.101cloud.instance_id")
			case "image":
				fixture.instance.Config["volatile.base_image"] = strings.Repeat("b", 64)
			case "running":
				fixture.instance.Status = "Running"
				fixture.instance.StatusCode = api.Running
			case "inconsistent_state":
				fixture.instance.StatusCode = api.Running
			case "vm":
				fixture.instance.Type = "virtual-machine"
			case "unknown_type":
				fixture.instance.Type = ""
			}
			if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.reads) != 0 || len(fixture.deletes) != 0 {
				t.Fatalf("unsafe target accessed: err=%v reads=%v deletes=%v", err, fixture.reads, fixture.deletes)
			}
		})
	}
}

func TestCloudInitPrepareFileErrorsFailClosed(t *testing.T) {
	for _, path := range []string{"/usr/bin/cloud-init", "/etc/cloud/cloud.cfg", cloudInitDisabledPath} {
		for _, cause := range []error{api.StatusErrorf(403, "forbidden"), api.StatusErrorf(500, "failure"), errors.New("404 not found"), io.ErrUnexpectedEOF} {
			fixture, request := newCloudInitFixture()
			fixture.files[path] = cause
			if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.deletes) != 0 {
				t.Fatalf("%s error %v permitted deletion: %v", path, cause, err)
			}
		}
	}
	for _, path := range []string{"/usr/bin/cloud-init", "/etc/cloud/cloud.cfg"} {
		fixture, request := newCloudInitFixture()
		fixture.files[path] = api.StatusErrorf(404, "missing")
		if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.deletes) != 0 {
			t.Fatalf("missing installation witness %s permitted deletion", path)
		}
	}
	fixture, request := newCloudInitFixture()
	fixture.fileType = "directory"
	if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.deletes) != 0 {
		t.Fatal("non-file installation witness permitted deletion")
	}
}

func TestCloudInitPrepareRechecksBeforeRemoval(t *testing.T) {
	for _, change := range []func(*api.Instance){
		func(i *api.Instance) { i.Config["volatile.uuid"] = "replacement" },
		func(i *api.Instance) { i.Status = "Running"; i.StatusCode = api.Running },
		func(i *api.Instance) { i.Config["user.101cloud.instance_id"] = "99" },
		func(i *api.Instance) { i.Config["volatile.base_image"] = strings.Repeat("b", 64) },
	} {
		fixture, request := newCloudInitFixture()
		fixture.change = change
		if _, err := prepareCloudInit(fixture, request); err == nil || len(fixture.deletes) != 0 {
			t.Fatal("changed instance permitted deletion")
		}
	}
}

func TestCloudInitPrepareDeleteErrors(t *testing.T) {
	for _, cause := range []error{api.StatusErrorf(404, "missing"), api.StatusErrorf(403, "forbidden"), errors.New("404 not found")} {
		fixture, request := newCloudInitFixture()
		fixture.deleteError = cause
		result, err := prepareCloudInit(fixture, request)
		missing := api.StatusErrorCheck(cause, 404)
		if (err == nil) != missing || result.Checked != missing || result.DisabledMarkerRemoved {
			t.Fatalf("delete error %v: %+v %v", cause, result, err)
		}
	}
}

func TestCloudInitPrepareRejectsMalformedRequests(t *testing.T) {
	valid := `"project":"tenant","name":"worker","expected_uuid":"uuid","expected_instance_id":"42","expected_image_fingerprint":"` + strings.Repeat("a", 64) + `"`
	for _, body := range []string{"{}", "null", "[{}]", "{" + valid + `,"project":"other"}`, "{" + valid + `,"path":"/etc/passwd"}`, "{" + valid + "} {}", strings.Replace("{"+valid+"}", `"tenant"`, `null`, 1), strings.Replace("{"+valid+"}", `"worker"`, `"../worker"`, 1)} {
		r := httptest.NewRequest(http.MethodPost, "/agent/v1/instances/cloud-init/prepare", strings.NewReader(body))
		if _, err := decodeCloudInitPrepare(httptest.NewRecorder(), r); err == nil {
			t.Fatalf("unsafe request accepted: %s", body)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/agent/v1/instances/cloud-init/prepare", strings.NewReader("{"+valid+"}"))
	if _, err := decodeCloudInitPrepare(httptest.NewRecorder(), r); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	service, _ := newFixture(t, "")
	for _, test := range []struct {
		method, suffix string
		status         int
	}{{http.MethodGet, "", 405}, {http.MethodPost, "?project=tenant", 400}, {http.MethodPost, "?", 400}} {
		response := httptest.NewRecorder()
		service.ServeHTTP(response, httptest.NewRequest(test.method, "/agent/v1/instances/cloud-init/prepare"+test.suffix, strings.NewReader("{"+valid+"}")))
		if response.Code != test.status {
			t.Fatalf("route validation: got %d want %d", response.Code, test.status)
		}
	}
}

type cloudInitRoundTripper func(*http.Request) (*http.Response, error)

func (f cloudInitRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCloudInitFileTransportRequiresTypedMatchingErrors(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"type":"error","error_code":404,"error":"missing"}`, true},
		{`{"type":"error","error_code":403,"error":"forbidden"}`, false},
		{`{"type":"sync","status_code":404}`, false},
		{`{"error_code":404}`, false},
		{"404 not found", false},
	} {
		transport := cloudInitFileTransport{base: cloudInitRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
		})}
		request := httptest.NewRequest(http.MethodGet, "http://unix.socket/1.0/instances/worker/files?project=tenant&path=/etc/cloud/cloud-init.disabled", nil)
		response, err := transport.RoundTrip(request)
		if (err == nil) != test.valid {
			t.Fatalf("body=%s error=%v", test.body, err)
		}
		if response != nil {
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != test.body {
				t.Fatalf("typed response was not preserved: %q %v", body, err)
			}
		}
	}
}
