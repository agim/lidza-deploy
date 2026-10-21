package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func nativeStackPlatform(ctx context.Context) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := command(bounded, "", nil, "docker", "info", "--format", `{{.OSType}}/{{.Architecture}}`)
	if err != nil {
		return "", errors.New("cannot determine Docker host architecture")
	}
	switch raw {
	case "linux/amd64", "linux/x86_64", "linux/amd64/v3":
		return "linux/amd64", nil
	case "linux/arm64", "linux/aarch64":
		return "linux/arm64", nil
	}
	return "", errors.New("stack runtime requires a native Linux AMD64 or ARM64 Docker host")
}
func validateStackImages(ctx context.Context, manifest StackManifest, images map[string]string) error {
	host, err := nativeStackPlatform(ctx)
	if err != nil {
		return err
	}
	for _, s := range manifest.Services {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		raw, err := command(bounded, "", nil, "docker", "image", "inspect", "--format", `{"os":{{json .Os}},"architecture":{{json .Architecture}}}`, images[s.Name])
		cancel()
		var image struct{ OS, Architecture string }
		if err != nil || json.Unmarshal([]byte(raw), &image) != nil || image.OS == "" || image.Architecture == "" {
			return fmt.Errorf("stack service %s image architecture inspection failed", s.Name)
		}
		if image.OS+"/"+image.Architecture != host {
			return fmt.Errorf("stack service %s image architecture %s/%s is incompatible with Docker host %s; use a native image or multi-architecture digest", s.Name, image.OS, image.Architecture, host)
		}
	}
	return nil
}

// A native-labelled derivative can still inherit a foreign pinned base. Watch
// BuildKit's check as well as checking final image metadata before activation.
func buildStackImage(ctx context.Context, source, service string, args []string) error {
	mismatch := false
	original, _ := ctx.Value(diagnosticStreamKey{}).(func(string) func(string))
	logged := ctx
	ctx = context.WithValue(ctx, diagnosticStreamKey{}, func(name string) func(string) {
		var forward func(string)
		if original != nil {
			forward = original(name)
		}
		return func(output string) {
			if strings.Contains(output, "InvalidBaseImagePlatform") || strings.Contains(output, "does not match the specified platform") {
				mismatch = true
			}
			if forward != nil {
				forward(output)
			} else {
				commandDiagnostic(logged, name, output)
			}
		}
	})
	_, err := command(ctx, source, nil, "docker", args...)
	if mismatch {
		return fmt.Errorf("stack service %s Dockerfile base image architecture does not match the Docker host; use a native or multi-architecture FROM digest", service)
	}
	if err != nil {
		return fmt.Errorf("stack %s image build failed: %w", service, err)
	}
	return nil
}
