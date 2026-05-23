// Package version exposes the release identity of the binary.
//
// Version is the single source of truth and lives in source — bump it here
// when cutting a release, then tag the git commit to match.
//
// Commit and Date are optional build-time provenance, injected via -ldflags
// when available (e.g. from a release workflow). They default to "none" /
// "unknown" for local builds, which is fine — Version alone identifies the
// release.
//
//	go build -ldflags "\
//	    -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Commit=$(git rev-parse --short HEAD) \
//	    -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

// Version is the semantic version of this release. Bump here, then git tag.
const Version = "v0.3.0"

// Commit is the short git SHA the binary was built from. Optional; set via
// -ldflags. Defaults to "none" for local builds.
var Commit = "none"

// Date is the build timestamp in RFC3339 form. Optional; set via -ldflags.
var Date = "unknown"

// String returns a single-line description of the build, suitable for /healthz.
func String() string {
	return Version + " (" + Commit + ", built " + Date + ")"
}
