package onboarding

import (
	"os"
	"testing"

	"github.com/agim/lidza-deploy/internal/testsupport"
)

func TestMain(m *testing.M) {
	os.Exit(testsupport.Run(m))
}
