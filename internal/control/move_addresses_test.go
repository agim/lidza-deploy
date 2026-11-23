package control

import (
	"slices"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Unit tests never look hostnames up on the network.
func init() { moveResolveHost = func(string) []string { return nil } }

func TestMoveTargetAddressesFallBackToTheServersHostname(t *testing.T) {
	saved := moveResolveHost
	t.Cleanup(func() { moveResolveHost = saved })
	asked := []string{}
	moveResolveHost = func(host string) []string {
		asked = append(asked, host)
		return []string{"192.168.60.192", "203.0.113.7", "2001:db8::7", "203.0.113.7"}
	}
	server := Server{ID: "home", URL: "https://server1.example.com"}
	// Behind a router the agent reports only a private address: the public
	// addresses of the name the panel reaches it by are used.
	got := moveTargetAddresses(agent.MoveHost{IPv4: "192.168.60.192"}, server, nil)
	if !slices.Equal(got, []string{"203.0.113.7", "2001:db8::7"}) || !slices.Equal(asked, []string{"server1.example.com"}) {
		t.Fatalf("addresses %v (asked %v)", got, asked)
	}
	// A detected public address, or addresses the operator entered, need no lookup.
	asked = nil
	if got := moveTargetAddresses(agent.MoveHost{IPv4: "198.51.100.4"}, server, nil); !slices.Equal(got, []string{"198.51.100.4"}) || len(asked) != 0 {
		t.Fatalf("detected address %v (asked %v)", got, asked)
	}
	if got := moveTargetAddresses(agent.MoveHost{}, server, []string{"198.51.100.9"}); !slices.Equal(got, []string{"198.51.100.9"}) || len(asked) != 0 {
		t.Fatalf("entered address %v (asked %v)", got, asked)
	}
	// A server addressed by IP is not looked up either.
	if got := moveTargetAddresses(agent.MoveHost{}, Server{URL: "https://198.51.100.20"}, nil); !slices.Equal(got, []string{"198.51.100.20"}) || len(asked) != 0 {
		t.Fatalf("literal address %v (asked %v)", got, asked)
	}
}
