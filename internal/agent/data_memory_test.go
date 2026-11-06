package agent

import (
	"context"
	"strings"
	"testing"
)

func TestLiftContainerCapsRaisesOnlyCappedContainers(t *testing.T) {
	var calls []string
	fake := func(inspect string) func(context.Context, ...string) (string, error) {
		return func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "inspect" {
				return inspect + "\n", nil
			}
			return "", nil
		}
	}
	lifted, err := liftContainerCaps(context.Background(), fake("150000000 1073741824"), "db")
	if err != nil || !lifted || !strings.Contains(calls[1], "update --cpus ") || !strings.Contains(calls[1], "--memory ") || !strings.HasSuffix(calls[1], " db") {
		t.Fatalf("a capped container must be lifted: lifted=%v err=%v calls=%v", lifted, err, calls)
	}
	calls = nil
	if lifted, _ := liftContainerCaps(context.Background(), fake("0 0"), "db"); lifted || len(calls) != 1 {
		t.Fatalf("an uncapped container must be left alone: %v", calls)
	}
}
