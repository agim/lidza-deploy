package control

import (
	"net/url"
	"strings"
)

// consoleBase is the control panel's base path. The web frontend serves
// console.html for every path below it, so these links open the exact view.
const consoleBase = "/console"

// consolePath builds a panel path from unescaped segments.
func consolePath(segments ...string) string {
	path := consoleBase
	for _, segment := range segments {
		path += "/" + url.PathEscape(segment)
	}
	return path
}

// alertPath is the panel view that explains an operational alert, by its
// observation key (kind:server[:app[:item]]).
func alertPath(key string) string {
	parts := strings.Split(key, ":")
	at := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	server, app := at(1), at(2)
	switch parts[0] {
	case "server":
		if server != "" {
			return consolePath("servers", server)
		}
	case "disk":
		if server != "" {
			return consolePath("servers", server, "cleanup")
		}
	case "memory", "load":
		if server != "" {
			return consolePath("servers", server, "containers")
		}
	case "storage":
		return consolePath("remote-backups")
	case "database", "backup-age":
		if app != "" {
			return consolePath("databases", app)
		}
	case "health":
		if app != "" {
			return consolePath("applications", app, "logs")
		}
	case "deploy":
		return consolePath("deployments")
	case "security-burst", "security-review":
		if app != "" {
			return consolePath("security", app)
		}
	case "task":
		if app != "" {
			return consolePath("applications", app, "settings", "processes")
		}
	}
	return consolePath("applications")
}
