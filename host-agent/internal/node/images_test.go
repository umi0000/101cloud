package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/simplestreams"
)

type catalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f catalogRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Fixtures are consumed by the installed Incus v6.0.5 SimpleStreams client,
// rather than replacing SDK normalization with a parallel test implementation.
func catalogFixture(t *testing.T) map[string]string {
	t.Helper()
	products := simplestreams.Products{DataType: "image-downloads", Format: "products:1.0", Products: map[string]simplestreams.Product{}}
	add := func(name, architecture, variant, rootType, fingerprint string, size int64) {
		meta := simplestreams.ProductVersionItem{FileType: "incus.tar.xz", Path: name + "/incus.tar.xz", Size: 10}
		switch rootType {
		case "root.tar.xz":
			meta.CombinedSha256RootXz = fingerprint
		case "squashfs":
			meta.CombinedSha256SquashFs = fingerprint
		case "disk-kvm.img":
			meta.CombinedSha256DiskKvmImg = fingerprint
		}
		products.Products[name] = simplestreams.Product{
			Aliases: "alpine/3.23/" + variant, Architecture: architecture, OperatingSystem: "Alpine",
			Release: "3.23", ReleaseTitle: "3.23", Variant: variant,
			Versions: map[string]simplestreams.ProductVersion{
				"20261102_01:00": {Items: map[string]simplestreams.ProductVersionItem{
					"metadata": meta, "root": {FileType: rootType, Path: name + "/root", Size: size},
				}},
			},
		}
	}
	add("default-amd64", "amd64", "default", "root.tar.xz", strings.Repeat("1", 64), 100)
	add("cloud-amd64", "amd64", "cloud", "squashfs", strings.Repeat("2", 64), 200)
	add("cloud-arm64", "arm64", "cloud", "root.tar.xz", strings.Repeat("3", 64), 300)
	add("cloud-vm", "amd64", "cloud", "disk-kvm.img", strings.Repeat("4", 64), 400)
	add("no-variant", "amd64", "", "root.tar.xz", strings.Repeat("5", 64), 500)
	index := simplestreams.Stream{Format: "index:1.0", Index: map[string]simplestreams.StreamIndex{
		"images": {DataType: "image-downloads", Path: "streams/v1/images.json", Products: []string{"alpine"}},
	}}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	productsJSON, err := json.Marshal(products)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"streams/v1/index.json": string(indexJSON), "streams/v1/images.json": string(productsJSON)}
}

func fixtureCatalogLoader(t *testing.T, fixture map[string]string, calls *atomic.Int32) func(context.Context, ImageSource) ([]api.Image, error) {
	t.Helper()
	return func(ctx context.Context, source ImageSource) ([]api.Image, error) {
		calls.Add(1)
		client, err := newCatalogClient(ctx, source)
		if err != nil {
			return nil, err
		}
		defer client.Disconnect()
		httpClient, err := client.GetHTTPClient()
		if err != nil {
			return nil, err
		}
		defer httpClient.CloseIdleConnections()
		transport := httpClient.Transport.(*catalogTransport)
		transport.base = catalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			prefix := strings.TrimSuffix(transport.origin.Path, "/") + "/"
			body, ok := fixture[strings.TrimPrefix(r.URL.Path, prefix)]
			if !ok {
				return nil, errors.New("unexpected fixture request")
			}
			if r.Header.Get("User-Agent") != "101cloud-hostagent" {
				return nil, errors.New("missing SDK user agent")
			}
			return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		return client.GetImages()
	}
}

func imageRequest(s *Service, method, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}

func imageTestService(t *testing.T) *Service {
	t.Helper()
	s, err := New("/absent-image-test-incus.socket")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImageSourcesFixedAndIndependentOfIncus(t *testing.T) {
	s := imageTestService(t)
	s.imageCatalog.load = func(context.Context, ImageSource) ([]api.Image, error) {
		t.Fatal("listing sources must not fetch catalogs")
		return nil, nil
	}
	response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources")
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Items []ImageSource `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Items, imageSources[:]) {
		t.Fatalf("sources=%+v", result.Items)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
}

func TestImageDiscoverySDKVariantsFiltersAndResolution(t *testing.T) {
	s := imageTestService(t)
	var calls atomic.Int32
	s.imageCatalog.load = fixtureCatalogLoader(t, catalogFixture(t), &calls)
	response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images")
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Items []RemoteImage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 5 {
		t.Fatalf("items=%+v", result.Items)
	}
	for index, item := range result.Items {
		if item.Fingerprint != strings.Repeat(string(rune('1'+index)), 64) {
			t.Fatalf("unordered/invented fingerprint: %+v", item)
		}
		if item.OSName != "Alpine" || item.OSRelease != "3.23" || item.Description == "" {
			t.Fatalf("source metadata lost: %+v", item)
		}
		if len(item.Aliases) == 0 {
			t.Fatalf("SDK aliases lost: %+v", item)
		}
	}
	if result.Items[0].Variant != "default" || result.Items[0].CloudInit || result.Items[0].Architecture != "x86_64" || result.Items[0].SizeBytes != "110" {
		t.Fatalf("default artifact changed: %+v", result.Items[0])
	}
	if !result.Items[1].CloudInit || result.Items[1].Variant != "cloud" || result.Items[1].SizeBytes != "210" {
		t.Fatalf("cloud artifact changed: %+v", result.Items[1])
	}
	if result.Items[2].Architecture != "aarch64" || !result.Items[2].CloudInit {
		t.Fatalf("arm variant lost: %+v", result.Items[2])
	}
	if result.Items[3].InstanceType != "virtual-machine" || result.Items[3].SizeBytes != "410" {
		t.Fatalf("VM artifact changed: %+v", result.Items[3])
	}
	if result.Items[4].Variant != "" || result.Items[4].CloudInit {
		t.Fatalf("absent variant was fabricated: %+v", result.Items[4])
	}

	for _, test := range []struct {
		query        string
		fingerprints string
	}{
		{"?os=Alpine&release=3.23&architecture=aarch64&instance_type=container&cloud_init=true", "3"},
		{"?cloud_init=false", "15"},
		{"?instance_type=virtual-machine", "4"},
		{"?os=not-present", ""},
		{"?architecture=ppc64le", ""},
	} {
		response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images"+test.query)
		if response.Code != 200 {
			t.Fatalf("query=%s status=%d body=%s", test.query, response.Code, response.Body.String())
		}
		var filtered struct {
			Items []RemoteImage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
			t.Fatal(err)
		}
		if len(filtered.Items) != len(test.fingerprints) {
			t.Fatalf("query=%s items=%+v", test.query, filtered.Items)
		}
		for index, item := range filtered.Items {
			if item.Fingerprint != strings.Repeat(test.fingerprints[index:index+1], 64) {
				t.Fatalf("wrong artifact: %+v", item)
			}
		}
		if len(filtered.Items) == 0 && !strings.Contains(response.Body.String(), `"items":[]`) {
			t.Fatal("empty result must be an array")
		}
	}
	response = imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images/"+strings.Repeat("3", 64))
	if response.Code != 200 {
		t.Fatalf("resolution=%d %s", response.Code, response.Body.String())
	}
	var resolved RemoteImage
	if err := json.Unmarshal(response.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, result.Items[2]) {
		t.Fatalf("resolver disagrees with list: %+v", resolved)
	}
	if calls.Load() != 1 {
		t.Fatalf("list/resolve did not share bounded cache: loads=%d", calls.Load())
	}
}

func TestImageDiscoveryRejectsInvalidRequestsBeforeLoading(t *testing.T) {
	s := imageTestService(t)
	s.imageCatalog.load = func(context.Context, ImageSource) ([]api.Image, error) {
		t.Fatal("invalid request reached source")
		return nil, nil
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/agent/v1/image-sources", 405},
		{http.MethodGet, "/agent/v1/image-sources?server=https://evil.example", 400},
		{http.MethodGet, "/agent/v1/image-sources/evil/images", 404},
		{http.MethodGet, "/agent/v1/image-sources/images", 404},
		{http.MethodGet, "/agent/v1/image-sources/images/images/extra/path", 404},
		{http.MethodGet, "/agent/v1/image-sources/images/images/short", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images/" + strings.Repeat("z", 64), 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images/" + strings.Repeat("1", 64) + "?os=Alpine", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?architecture=amd64", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?instance_type=vm", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?cloud_init=1", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?os=Alpine&os=Ubuntu", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?release=", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?server=https://evil.example", 400},
		{http.MethodGet, "/agent/v1/image-sources/images/images?os=%zz", 400},
		{http.MethodGet, "/agent/v1/image-sources/%69mages/images", 400},
	} {
		response := imageRequest(s, test.method, test.path)
		if response.Code != test.status {
			t.Errorf("%s %s: status=%d want=%d body=%s", test.method, test.path, response.Code, test.status, response.Body.String())
		}
		if test.status == 405 && response.Header().Get("Allow") != "GET" {
			t.Fatal("missing Allow header")
		}
	}
}

func TestImageDiscoveryFailuresAreExplicitAndPublicSafe(t *testing.T) {
	for _, test := range []struct {
		name   string
		images []api.Image
		err    error
		status int
		code   string
	}{
		{"fetch", nil, errors.New("https://private.example?secret=token upstream response private-body"), 502, "image_catalog_failed"},
		{"timeout", nil, context.DeadlineExceeded, 504, "image_catalog_timeout"},
		{"canceled", nil, context.Canceled, 504, "image_catalog_timeout"},
		{"empty-unsupported", []api.Image{}, nil, 422, "image_catalog_unsupported"},
		{"invalid-metadata", []api.Image{{Fingerprint: "short"}}, nil, 422, "image_catalog_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := imageTestService(t)
			s.imageCatalog.load = func(context.Context, ImageSource) ([]api.Image, error) { return test.images, test.err }
			response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images")
			body := response.Body.String()
			if response.Code != test.status || !strings.Contains(body, test.code) {
				t.Fatalf("status=%d body=%s", response.Code, body)
			}
			if strings.Contains(body, "private") || strings.Contains(body, "secret") || strings.Contains(body, `"items"`) {
				t.Fatalf("failure leaked or fabricated a catalog: %s", body)
			}
		})
	}
}

func TestImageCatalogNoExpiredFallbackAndMissingResolution(t *testing.T) {
	s := imageTestService(t)
	var calls atomic.Int32
	s.imageCatalog.load = fixtureCatalogLoader(t, catalogFixture(t), &calls)
	response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images/"+strings.Repeat("f", 64))
	if response.Code != 404 || !strings.Contains(response.Body.String(), "remote_image_not_found") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	s.imageCatalog.entries["images"].expires = time.Now().Add(-time.Second)
	s.imageCatalog.load = func(context.Context, ImageSource) ([]api.Image, error) { return nil, errors.New("source unavailable") }
	response = imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images")
	if response.Code != 502 || strings.Contains(response.Body.String(), `"items"`) {
		t.Fatalf("expired catalog served on failure: %d %s", response.Code, response.Body.String())
	}
}

func TestImageCatalogRejectsUnsupportedCanonicalSource(t *testing.T) {
	s := imageTestService(t)
	s.imageCatalog.load = func(context.Context, ImageSource) ([]api.Image, error) {
		t.Fatal("unsupported Canonical source reached discovery loader")
		return nil, nil
	}
	response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/ubuntu/images")
	if response.Code != 404 || !strings.Contains(response.Body.String(), "image_source_not_found") {
		t.Fatalf("unsupported source was selectable: %d %s", response.Code, response.Body.String())
	}
}

func TestImageCatalogUnsupportedArtifactDoesNotBecomeFakeImage(t *testing.T) {
	s := imageTestService(t)
	fixture := catalogFixture(t)
	// Incus v6.0.5 recognizes incus.tar.xz, not legacy lxd.tar.xz.
	// A source with only unsupported artifacts must report 422, not invent a
	// cloud/container row or rewrite its on-wire product format.
	fixture["streams/v1/images.json"] = strings.ReplaceAll(fixture["streams/v1/images.json"], "incus.tar.xz", "lxd.tar.xz")
	var calls atomic.Int32
	s.imageCatalog.load = fixtureCatalogLoader(t, fixture, &calls)
	response := imageRequest(s, http.MethodGet, "/agent/v1/image-sources/images/images")
	if response.Code != 422 || !strings.Contains(response.Body.String(), "image_catalog_unsupported") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestImageCatalogWaitingRefreshHonorsCancellation(t *testing.T) {
	catalog := newRemoteImageCatalog()
	entry := catalog.entries["images"]
	<-entry.slot
	defer func() { entry.slot <- struct{}{} }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.get(ctx, imageSources[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestImageCatalogTransportBoundsAndTrust(t *testing.T) {
	origin, err := url.Parse(imageSources[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"http://images.linuxcontainers.org/streams/v1/index.json",
		"https://evil.example/streams/v1/index.json",
		"https://user@images.linuxcontainers.org/streams/v1/index.json",
		"https://images.linuxcontainers.org/streams/v1/index.json?secret=1",
	} {
		transport := &catalogTransport{origin: origin, ctx: context.Background(), remaining: maxCatalogBytes, base: catalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsafe request reached network")
			return nil, nil
		})}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	subpathOrigin, _ := url.Parse(imageSources[0].URL + "/catalog")
	transport := &catalogTransport{origin: subpathOrigin, ctx: context.Background(), remaining: maxCatalogBytes}
	if _, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, imageSources[0].URL+"/other/index.json", nil)); err == nil {
		t.Fatal("escaped source prefix")
	}

	for _, contentLength := range []int64{-1, 5} {
		transport := &catalogTransport{origin: origin, ctx: context.Background(), remaining: 4, base: catalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: contentLength, Body: io.NopCloser(strings.NewReader("12345"))}, nil
		})}
		response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, imageSources[0].URL+"/streams/v1/index.json", nil))
		if err == nil {
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}
		if !errors.Is(err, errCatalogBounds) {
			t.Fatalf("length=%d error=%v", contentLength, err)
		}
	}
	transport = &catalogTransport{origin: origin, ctx: context.Background(), remaining: maxCatalogBytes, requests: maxCatalogRequests}
	if _, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, imageSources[0].URL+"/streams/v1/index.json", nil)); !errors.Is(err, errCatalogBounds) {
		t.Fatalf("request bound error=%v", err)
	}
}

func TestImageCatalogSDKTransportCarriesContextAndDisablesRedirects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client, err := newCatalogClient(ctx, imageSources[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.CloseIdleConnections()
	transport := httpClient.Transport.(*catalogTransport)
	if transport.transport.Proxy != nil {
		t.Fatal("catalog uses ambient proxy")
	}
	if err := httpClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy=%v", err)
	}
	transport.base = catalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context() != ctx {
			t.Error("SDK did not receive discovery context")
		}
		return nil, r.Context().Err()
	})
	cancel()
	if _, err := client.GetImages(); !errors.Is(err, context.Canceled) {
		t.Fatalf("SDK context error=%v", err)
	}
}

func TestNormalizeRemoteImageMetadataNeverGuesses(t *testing.T) {
	valid := api.Image{Fingerprint: strings.Repeat("a", 64), Architecture: "x86_64", Type: "container", Size: 9007199254740993,
		ImagePut: api.ImagePut{Properties: map[string]string{"os": "Alpine", "release": "3.23", "variant": "cloud"}},
	}
	items, err := normalizeRemoteImages([]api.Image{valid})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].SizeBytes != "9007199254740993" || !items[0].CloudInit {
		t.Fatalf("metadata=%+v", items[0])
	}
	for _, mutate := range []func(*api.Image){
		func(i *api.Image) { i.Properties = map[string]string{"release": "3.23"} },
		func(i *api.Image) { i.Properties = map[string]string{"os": "Alpine"} },
		func(i *api.Image) { i.Architecture = "amd64" },
		func(i *api.Image) { i.Type = "" },
		func(i *api.Image) { i.Size = -1 },
	} {
		invalid := valid
		mutate(&invalid)
		if _, err := normalizeRemoteImages([]api.Image{invalid}); !errors.Is(err, errUnsupportedCatalog) {
			t.Fatalf("accepted incomplete artifact: %+v error=%v", invalid, err)
		}
	}
	conflict := valid
	conflict.Properties = map[string]string{"os": "Alpine", "release": "3.23", "variant": "default"}
	if _, err := normalizeRemoteImages([]api.Image{valid, conflict}); !errors.Is(err, errUnsupportedCatalog) {
		t.Fatalf("accepted conflicting fingerprint metadata: %v", err)
	}
}
