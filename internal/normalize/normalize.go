package normalize

import (
	"path/filepath"
	"strings"
)

// ProcessName normalizes a full process path to a canonical short name.
//
//	/Applications/Spotify.app/Contents/Frameworks/Spotify → Spotify
//	/System/Library/.../WindowServer → WindowServer
//	/usr/libexec/trustd → trustd
//	claude → claude
func ProcessName(path string) string {
	if !strings.Contains(path, "/") {
		return path
	}

	// /Applications/Foo.app/... → Foo
	if strings.HasPrefix(path, "/Applications/") {
		parts := strings.SplitN(path, "/", 4) // ["", "Applications", "Foo.app", ...]
		if len(parts) >= 3 {
			appName := parts[2]
			appName = strings.TrimSuffix(appName, ".app")
			// Handle names like "Google Chrome" (folder name)
			return appName
		}
	}

	// For everything else, use the last path component
	base := filepath.Base(path)
	if base == "." || base == "/" {
		return path
	}
	return base
}
