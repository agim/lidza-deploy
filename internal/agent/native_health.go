package agent

import (
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type NativeReadiness struct {
	Path       string `json:"path"`
	Ready      bool   `json:"ready"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message"`
}

func (m *Manager) nativeEnvironment(name string) map[string]string {
	values := map[string]string{}
	for _, path := range nativeEnvironmentFiles {
		parseNativeEnvironment(m.nativeMetadataFile(filepath.Join(name, path)), values)
	}
	return values
}

// parseNativeEnvironment adds the KEY=value lines of an environment file.
func parseNativeEnvironment(data []byte, values map[string]string) {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if ok && envPattern.MatchString(key) {
			values[key] = strings.Trim(strings.TrimSpace(val), "\"'")
		}
	}
}

func nativePort(values map[string]string) string {
	raw := values["PORT"]
	if !regexp.MustCompile(`^[0-9]{1,5}$`).MatchString(raw) {
		return ""
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	return strconv.Itoa(port)
}

func validateNativeHealth(a NativeApplication) error {
	runtime := a.Runtime
	if runtime == "" {
		runtime = "rails"
	}
	probe := App{ID: a.ID, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: a.AdditionalDomains, Runtime: runtime}
	if runtime == "rails" {
		probe.Rails = &RailsConfig{HealthPath: a.HealthPath}
	} else if runtime == "astro" {
		probe.Astro = &AstroConfig{Mode: "static", HealthPath: a.HealthPath}
	} else {
		return errors.New("unsupported native runtime")
	}
	if err := probe.Validate(); err != nil {
		return errors.New("native source, hostname or readiness path is invalid")
	}
	return nil
}
