package metrics

import "net/http"

// contentType is the Prometheus text exposition content type (version 0.0.4).
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// defaultRegistry is the package-level registry used by the top-level helpers.
var defaultRegistry = NewRegistry()

// Default returns the package-level registry.
func Default() *Registry { return defaultRegistry }

// Handler returns an http.Handler that scrapes reg in the Prometheus text
// exposition format. A nil reg uses the default registry.
func Handler(reg *Registry) http.Handler {
	if reg == nil {
		reg = defaultRegistry
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_ = reg.WriteText(w)
	})
}
