// Package version exposes build-time metadata baked into the binary via -ldflags.
//
// Set at build time:
//
//	go build -ldflags "-X github.com/ricgrangeia/server-space-manager-ai/internal/version.Version=$(git describe --tags --always) \
//	                   -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

// Version is the semantic version of the build (e.g. "v0.1.0"). Defaults to "dev"
// when built without -ldflags.
var Version = "dev"

// Commit is the short git SHA the binary was built from.
var Commit = "none"

// Date is the build timestamp in RFC3339 form.
var Date = "unknown"

// String returns a single-line description of the build, suitable for /healthz.
func String() string {
	return Version + " (" + Commit + ", built " + Date + ")"
}
