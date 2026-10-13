package agent

import (
	"errors"
	"fmt"
)

// Only errors scrubbed at the provisioning boundary may enter the public view.
type cacheProvisionError struct{ stage, detail string }

func (e *cacheProvisionError) Error() string { return e.stage + ": " + e.detail }

func cacheFailure(stage string, err error, c CacheResource) error {
	secrets := append(outputSecrets(map[string]string{"CACHE_URL": c.URL}), c.LocalPassword)
	return &cacheProvisionError{stage: stage, detail: boundedErrorText(scrubLiveOutput(err.Error(), secrets), 2048)}
}

func publicCacheFailure(err error) error {
	var safe *cacheProvisionError
	if errors.As(err, &safe) {
		return fmt.Errorf("managed cache unavailable: %w; check Application → Cache and hosting agent service access; previous release retained", safe)
	}
	return errors.New("managed cache unavailable; check Application → Cache and hosting agent service access; previous release retained")
}
