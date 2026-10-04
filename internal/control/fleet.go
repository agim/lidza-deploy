package control

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type fleetResult struct {
	Server Server
	Body   json.RawMessage
	Err    error
}

// A slow agent must not hold a fleet page for one timeout per configured host.
// Bound both the overall deadline and simultaneous remote requests.
func (c *Control) readFleet(r *http.Request, path string) []fleetResult {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	servers := c.servers()
	out := make([]fleetResult, len(servers))
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].Server = s
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				out[i].Err = ctx.Err()
				return
			}
			out[i].Err = c.agentRequest(r, s, "GET", path, nil, &out[i].Body)
		}()
	}
	wg.Wait()
	return out
}
