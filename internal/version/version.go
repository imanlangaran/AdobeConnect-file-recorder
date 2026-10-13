// Package version exposes the build version.
//
// Version is stamped at release time via:
//
//	go build -ldflags "-X github.com/iman-langaran/AdobeConnect-file-recorder/internal/version.Version=v1.2.3"
//
// The default below is a fallback for dev builds (plain `go build`), which
// report "dev" rather than a stale release number.
package version

// Version is the build version. Overridden at release via -ldflags -X.
var Version = "dev"
