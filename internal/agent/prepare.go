package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/agim/lidza/pkg/config"
	"github.com/agim/lidza/pkg/scaffold"
	"golang.org/x/mod/modfile"
)

// prepareBuild uses the framework's deployment generator inside the disposable
// checkout. It never modifies or pushes the user's source repository.
func prepareBuild(source string) error {
	if err := regularFile(filepath.Join(source, "lidza.json"), false); err != nil {
		return errors.New("this repository is missing lidza.json, the project manifest required by Līdza")
	}
	dockerfile := filepath.Join(source, "Dockerfile")
	if err := regularFile(dockerfile, true); err != nil {
		return err
	}
	if _, err := os.Stat(dockerfile); err == nil {
		return nil
	}
	cfg, err := config.Load(source)
	if err != nil {
		return fmt.Errorf("application manifest: %w", err)
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`).MatchString(cfg.Name) {
		return errors.New("application name is not safe for generated deployment paths")
	}
	// Reject symlinks before the framework generator can write through them.
	if err := regularFile(filepath.Join(source, "go.mod"), false); err != nil {
		return err
	}
	if err := regularFile(filepath.Join(source, ".dockerignore"), true); err != nil {
		return err
	}
	deployDir := filepath.Join(source, "deploy")
	if info, e := os.Lstat(deployDir); e == nil {
		if !info.IsDir() {
			return errors.New("deploy must be a regular directory")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if err := regularFile(filepath.Join(deployDir, cfg.Name+".service"), true); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		return err
	}
	module, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return errors.New("could not parse application go.mod")
	}
	pins := scaffold.AppVersions(source)
	if pins.Lidza == "" {
		return errors.New("go.mod must pin the Līdza framework")
	}
	if pins.LocalPath != "" {
		return errors.New("automatic build cannot use a local Līdza checkout")
	}
	for _, replacement := range module.Replace {
		if replacement.New.Version == "" {
			return errors.New("automatic build cannot use local module replacements")
		}
	}
	if pins.Go == "" {
		return errors.New("go.mod must declare its Go version")
	}
	if _, _, err = scaffold.DeployFiles(source, cfg, false); err != nil {
		return fmt.Errorf("generate deployment files: %w", err)
	}
	return nil
}
func regularFile(path string, optional bool) error {
	info, err := os.Lstat(path)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("required file %s is unavailable", filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", filepath.Base(path))
	}
	return nil
}
