package daemon

import (
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// UninstallSupported reports whether daemon uninstall can remove a managed
// service on this platform. Only macOS keeps a service definition after stop.
func UninstallSupported() bool {
	return runtimeGOOS == "darwin"
}

// Uninstall stops this instance's managed daemon and removes its LaunchAgent.
// It returns the removed plist path, or an empty string when no LaunchAgent is
// installed or the platform has none. Application data is kept.
func Uninstall(p *paths.Paths) (string, error) {
	if serviceManagerBypassed() || !UninstallSupported() {
		return "", nil
	}
	if _, err := serviceUserHomeDir(); err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	definition := launchAgentPath(p)
	if _, err := os.Stat(definition); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect managed service %s: %w", definition, err)
	}
	if err := stopPlannedDaemon(p, func() error { return removeLaunchAgent(p) }); err != nil {
		return "", fmt.Errorf("stop daemon before removing managed service: %w", err)
	}
	return definition, nil
}

// InstalledLaunchAgentPath returns the plist left by daemon stop on macOS.
func InstalledLaunchAgentPath(p *paths.Paths) string {
	if UninstallSupported() && managedServiceInstalled(p) {
		return launchAgentPath(p)
	}
	return ""
}
