// Package webui serves the frontend bundled into the backend executable.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// The production build must generate dist before compiling this package.
//
//go:embed all:dist
var bundle embed.FS

// These are the content-hashed output names configured by Vite. Public files
// outside these output directories must remain revalidatable.
var immutableAsset = regexp.MustCompile(`^(?:js/[^/]+-[A-Za-z0-9_-]{8}\.js|jse/index-[^/]+-[A-Za-z0-9_-]{8}\.js|css/[^/]+-[A-Za-z0-9_-]{8}\.css)$`)

// Handler serves only embedded files; it never consults the working directory.
// API routing and its JSON error responses remain the server's responsibility.
func Handler() http.Handler {
	files, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err)
	}
	return &handler{files: files}
}

type handler struct {
	files fs.FS
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	name, ok := requestPath(r)
	if !ok || name == "api" || strings.HasPrefix(name, "api/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if name == "" {
		name = "index.html"
	}
	file, err := h.files.Open(name)
	if err != nil {
		if !h.spaRoute(name, r) {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
		file, err = h.files.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	// embed files implement io.ReadSeeker over their read-only backing bytes.
	// Open creates an independent cursor without copying the asset per request.
	content, ok := file.(io.ReadSeeker)
	if !ok {
		http.Error(w, "embedded asset is not seekable", http.StatusInternalServerError)
		return
	}
	switch {
	case name == "index.html" || name == "_app.config.js":
		w.Header().Set("Cache-Control", "no-store")
	case immutableAsset.MatchString(name):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	// Set known web types explicitly rather than depend on host MIME databases.
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
	}
	http.ServeContent(w, r, name, time.Time{}, content)
}

// Reject ambiguous paths instead of cleaning or redirecting them. URL.Path has
// already been decoded once; encoded separators and double encoding are not
// alternative spellings of an embedded asset or client route.
func requestPath(r *http.Request) (string, bool) {
	p := r.URL.Path
	escaped := r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\%\x00") ||
		strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%2F") || strings.Contains(escaped, "%5c") || strings.Contains(escaped, "%5C") || strings.Contains(p, "//") {
		return "", false
	}
	name := strings.TrimPrefix(p, "/")
	for component := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(component, ".") || strings.EqualFold(path.Ext(component), ".map") {
			return "", false
		}
		for _, ch := range component {
			if ch < 0x20 || ch == 0x7f {
				return "", false
			}
		}
	}
	return name, true
}

func (h *handler) spaRoute(name string, r *http.Request) bool {
	// Extension-bearing paths and asset namespaces are never client routes,
	// including missing extensionless files under those namespaces.
	if strings.Contains(name, ".") {
		return false
	}
	root, _, _ := strings.Cut(name, "/")
	switch root {
	case "js", "jse", "css", "images", "assets":
		return false
	}
	if info, err := fs.Stat(h.files, root); err == nil && info.IsDir() {
		return false
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	for item := range strings.SplitSeq(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err == nil && params["q"] != "0" && (mediaType == "text/html" || mediaType == "*/*") {
			return true
		}
	}
	return false
}
