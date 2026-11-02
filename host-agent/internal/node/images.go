package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/osarch"
)

const (
	imageCatalogTimeout = 30 * time.Second
	imageCatalogTTL     = 2 * time.Minute
	maxCatalogBytes     = 64 << 20
	maxCatalogRequests  = 32
	maxCatalogImages    = 50000
)

// ImageSource is a fixed, trusted public SimpleStreams origin. Callers cannot
// supply a URL: discovery and fingerprint resolution use this same allowlist.
type ImageSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

var imageSources = [...]ImageSource{
	{ID: "images", Name: "Incus / Linux Containers", URL: "https://images.linuxcontainers.org", Protocol: "simplestreams"},
}

// RemoteImage identifies an actual artifact, not an OS/release template. In
// particular default and cloud variants, architectures and instance types are
// never substituted for one another. SizeBytes remains lossless in JS clients.
type RemoteImage struct {
	Fingerprint  string   `json:"fingerprint"`
	OSName       string   `json:"os_name"`
	OSRelease    string   `json:"os_release"`
	Architecture string   `json:"architecture"`
	InstanceType string   `json:"instance_type"`
	Variant      string   `json:"variant"`
	CloudInit    bool     `json:"cloud_init"`
	Description  string   `json:"description"`
	Aliases      []string `json:"aliases"`
	SizeBytes    string   `json:"size_bytes"`
}

type remoteImageCatalog struct {
	entries map[string]*remoteImageCatalogEntry
	load    func(context.Context, ImageSource) ([]api.Image, error)
}

type remoteImageCatalogEntry struct {
	// One token serializes refreshes per source, bounding concurrent public
	// downloads. Waiting is context-cancellable, including on cache hits.
	slot    chan struct{}
	images  []RemoteImage
	expires time.Time
}

var errUnsupportedCatalog = errors.New("unsupported image catalog metadata")
var errCatalogBounds = errors.New("image catalog exceeds discovery bounds")

func newRemoteImageCatalog() *remoteImageCatalog {
	catalog := &remoteImageCatalog{
		entries: make(map[string]*remoteImageCatalogEntry, len(imageSources)),
		load:    loadSimpleStreamsImages,
	}
	for _, source := range imageSources {
		slot := make(chan struct{}, 1)
		slot <- struct{}{}
		catalog.entries[source.ID] = &remoteImageCatalogEntry{slot: slot}
	}
	return catalog
}

func (c *remoteImageCatalog) get(ctx context.Context, source ImageSource) ([]RemoteImage, error) {
	entry := c.entries[source.ID]
	if entry == nil {
		return nil, errUnsupportedCatalog
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.slot:
	}
	defer func() { entry.slot <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if time.Now().Before(entry.expires) {
		return entry.images, nil
	}
	// Never serve an expired catalog on failure. Incus' optional disk cache can
	// do that, so it is deliberately not enabled for this authoritative API.
	images, err := c.load(ctx, source)
	if err != nil {
		return nil, err
	}
	items, err := normalizeRemoteImages(images)
	if err != nil {
		return nil, err
	}
	entry.images = items
	entry.expires = time.Now().Add(imageCatalogTTL)
	return items, nil
}

func (s *Service) imageSourcesAPI(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if r.URL.RawPath != "" {
		writeError(w, http.StatusBadRequest, "invalid_image_path", "image source paths must not contain escaped components")
		return
	}
	if r.URL.Path == "/agent/v1/image-sources" {
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "invalid_image_filter", "image source listing does not accept query parameters")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Items []ImageSource `json:"items"`
		}{imageSources[:]})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/agent/v1/image-sources/"), "/")
	if (len(parts) != 2 && len(parts) != 3) || parts[1] != "images" {
		writeError(w, http.StatusNotFound, "not_found", "unknown image source endpoint")
		return
	}
	var source ImageSource
	for _, candidate := range imageSources {
		if candidate.ID == parts[0] {
			source = candidate
			break
		}
	}
	if source.ID == "" {
		writeError(w, http.StatusNotFound, "image_source_not_found", "image source is not supported")
		return
	}
	var filters imageFilters
	if len(parts) == 3 {
		if !fullImageFingerprint(parts[2]) || r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "invalid_image_fingerprint", "resolution requires a full 64-character hexadecimal fingerprint without query parameters")
			return
		}
	} else {
		var err error
		filters, err = parseImageFilters(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_image_filter", err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), imageCatalogTimeout)
	defer cancel()
	items, err := s.imageCatalog.get(ctx, source)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), ctx.Err() != nil:
			writeError(w, http.StatusGatewayTimeout, "image_catalog_timeout", "remote image catalog request timed out or was canceled")
		case errors.Is(err, errUnsupportedCatalog):
			writeError(w, http.StatusUnprocessableEntity, "image_catalog_unsupported", "source contains no supported artifacts or has incomplete image metadata")
		default:
			// SDK errors may contain upstream URLs or entire malformed response
			// bodies. Keep them out of the backend's public-facing response.
			writeError(w, http.StatusBadGateway, "image_catalog_failed", "remote image catalog could not be read within discovery limits")
		}
		return
	}
	if len(parts) == 3 {
		fingerprint := strings.ToLower(parts[2])
		for _, item := range items {
			if item.Fingerprint == fingerprint {
				writeJSON(w, http.StatusOK, item)
				return
			}
		}
		writeError(w, http.StatusNotFound, "remote_image_not_found", "fingerprint is not present in the selected source catalog")
		return
	}
	if filters == (imageFilters{}) {
		writeJSON(w, http.StatusOK, struct {
			Items []RemoteImage `json:"items"`
		}{items})
		return
	}
	filtered := make([]RemoteImage, 0, len(items))
	for _, item := range items {
		if filters.matches(item) {
			filtered = append(filtered, item)
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []RemoteImage `json:"items"`
	}{filtered})
}

type imageFilters struct {
	os           string
	release      string
	architecture string
	instanceType string
	cloudInit    *bool
}

func parseImageFilters(rawQuery string) (imageFilters, error) {
	var filters imageFilters
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return filters, errors.New("invalid image filter query")
	}
	for key, values := range values {
		if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || len(values[0]) > 128 {
			return filters, errors.New("image filters require one non-empty value of at most 128 bytes")
		}
		value := values[0]
		switch key {
		case "os":
			filters.os = value
		case "release":
			filters.release = value
		case "architecture":
			if !nativeImageArchitecture(value) {
				return filters, errors.New("architecture must be a supported native Incus architecture name")
			}
			filters.architecture = value
		case "instance_type":
			if value != "container" && value != "virtual-machine" {
				return filters, errors.New("instance_type must be container or virtual-machine")
			}
			filters.instanceType = value
		case "cloud_init":
			if value != "true" && value != "false" {
				return filters, errors.New("cloud_init must be true or false")
			}
			cloudInit := value == "true"
			filters.cloudInit = &cloudInit
		default:
			return filters, errors.New("unsupported image filter parameter")
		}
	}
	return filters, nil
}

func (f imageFilters) matches(image RemoteImage) bool {
	return (f.os == "" || image.OSName == f.os) &&
		(f.release == "" || image.OSRelease == f.release) &&
		(f.architecture == "" || image.Architecture == f.architecture) &&
		(f.instanceType == "" || image.InstanceType == f.instanceType) &&
		(f.cloudInit == nil || image.CloudInit == *f.cloudInit)
}

func fullImageFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := range value {
		char := value[index]
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func nativeImageArchitecture(value string) bool {
	id, err := osarch.ArchitectureID(value)
	if err != nil {
		return false
	}
	name, err := osarch.ArchitectureName(id)
	return err == nil && name == value
}

func normalizeRemoteImages(images []api.Image) ([]RemoteImage, error) {
	if len(images) == 0 {
		return nil, errUnsupportedCatalog
	}
	if len(images) > maxCatalogImages {
		return nil, errCatalogBounds
	}
	items := make([]RemoteImage, 0, len(images))
	indices := make(map[string]int, len(images))
	for _, image := range images {
		if !fullImageFingerprint(image.Fingerprint) || !nativeImageArchitecture(image.Architecture) ||
			(image.Type != "container" && image.Type != "virtual-machine") || image.Size < 0 ||
			image.Properties["os"] == "" || image.Properties["release"] == "" {
			return nil, errUnsupportedCatalog
		}
		variant := image.Properties["variant"]
		item := RemoteImage{
			Fingerprint: strings.ToLower(image.Fingerprint), OSName: image.Properties["os"],
			OSRelease: image.Properties["release"], Architecture: image.Architecture,
			InstanceType: image.Type, Variant: variant,
			CloudInit:   variant == "cloud",
			Description: image.Properties["description"], Aliases: make([]string, 0, len(image.Aliases)),
			SizeBytes: strconv.FormatInt(image.Size, 10),
		}
		for _, alias := range image.Aliases {
			if alias.Name != "" {
				item.Aliases = append(item.Aliases, alias.Name)
			}
		}
		if index, ok := indices[item.Fingerprint]; ok {
			previous := &items[index]
			if previous.OSName != item.OSName || previous.OSRelease != item.OSRelease ||
				previous.Architecture != item.Architecture || previous.InstanceType != item.InstanceType ||
				previous.Variant != item.Variant || previous.CloudInit != item.CloudInit || previous.SizeBytes != item.SizeBytes {
				return nil, errUnsupportedCatalog
			}
			previous.Aliases = append(previous.Aliases, item.Aliases...)
			if item.Description < previous.Description {
				previous.Description = item.Description
			}
			continue
		}
		indices[item.Fingerprint] = len(items)
		items = append(items, item)
	}
	for index := range items {
		sort.Strings(items[index].Aliases)
		aliases := items[index].Aliases[:0]
		for _, alias := range items[index].Aliases {
			if len(aliases) == 0 || aliases[len(aliases)-1] != alias {
				aliases = append(aliases, alias)
			}
		}
		items[index].Aliases = aliases
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Fingerprint < items[j].Fingerprint })
	return items, nil
}

func loadSimpleStreamsImages(ctx context.Context, source ImageSource) ([]api.Image, error) {
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
	return client.GetImages()
}

func newCatalogClient(ctx context.Context, source ImageSource) (incus.ImageServer, error) {
	base, err := url.Parse(source.URL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errUnsupportedCatalog
	}
	client, err := incus.ConnectSimpleStreams(source.URL, &incus.ConnectionArgs{
		UserAgent:  "101cloud-hostagent",
		HTTPClient: &http.Client{Timeout: imageCatalogTimeout},
		TransportWrapper: func(transport *http.Transport) incus.HTTPTransporter {
			transport.Proxy = nil
			// Use net/http's context-aware, timeout-bounded public-CA TLS
			// handshake rather than the SDK's custom Incus dialer.
			transport.DialTLSContext = nil
			transport.ResponseHeaderTimeout = imageCatalogTimeout
			return &catalogTransport{base: transport, transport: transport, ctx: ctx, origin: base, remaining: maxCatalogBytes}
		},
	})
	if err != nil {
		return nil, err
	}
	// SDK v6.0.5 installs its own unrestricted redirect policy during connect.
	// Override it afterwards; no discovery request may leave the fixed source.
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		client.Disconnect()
		return nil, err
	}
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return client, nil
}

// The SDK's SimpleStreams requests do not carry caller contexts. This wrapper
// attaches one, limits total response bytes/requests for the entire refresh,
// and restricts catalog paths to the selected HTTPS source (not user URLs).
// A client is used serially for one catalog refresh, never shared across calls.
type catalogTransport struct {
	base      http.RoundTripper
	transport *http.Transport
	ctx       context.Context
	origin    *url.URL
	remaining int64
	requests  int
}

func (t *catalogTransport) Transport() *http.Transport { return t.transport }
func (t *catalogTransport) CloseIdleConnections()      { t.transport.CloseIdleConnections() }

func (t *catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host ||
		r.URL.User != nil || r.URL.RawQuery != "" || r.URL.Fragment != "" ||
		!strings.HasPrefix(r.URL.Path, strings.TrimSuffix(t.origin.Path, "/")+"/") {
		return nil, errors.New("catalog request outside trusted source")
	}
	t.requests++
	if t.requests > maxCatalogRequests || t.remaining <= 0 {
		return nil, errCatalogBounds
	}
	response, err := t.base.RoundTrip(r.Clone(t.ctx))
	if err != nil {
		return nil, err
	}
	if response.ContentLength > t.remaining {
		_ = response.Body.Close()
		return nil, errCatalogBounds
	}
	response.Body = &catalogBody{ReadCloser: response.Body, remaining: &t.remaining}
	return response, nil
}

type catalogBody struct {
	io.ReadCloser
	remaining *int64
}

func (b *catalogBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if *b.remaining < 0 {
		return 0, errCatalogBounds
	}
	// Read one extra byte only to distinguish exact-bound EOF from overflow.
	if int64(len(p)) > *b.remaining+1 {
		p = p[:*b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	*b.remaining -= int64(n)
	if *b.remaining < 0 {
		return 0, errCatalogBounds
	}
	return n, err
}
