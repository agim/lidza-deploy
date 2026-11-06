package agent

import "testing"

func TestChooseClickHousePortsAvoidsTakenPorts(t *testing.T) {
	takenBy := func(ports ...int) func(int) bool {
		set := map[int]bool{}
		for _, p := range ports {
			set[p] = true
		}
		return func(p int) bool { return set[p] }
	}
	cases := []struct {
		name      string
		cfg       NativeHostDataConfig
		taken     func(int) bool
		http, tcp int
		wantErr   bool
	}{
		{"defaults when free", NativeHostDataConfig{}, takenBy(), 8123, 9000, false},
		{"an application on 9000", NativeHostDataConfig{}, takenBy(9000), 8123, 19000, false},
		{"both defaults and the first fallbacks taken", NativeHostDataConfig{}, takenBy(8123, 9000, 18123, 19000), 18124, 19001, false},
		{"configured ports are kept", NativeHostDataConfig{ClickHouseHTTPPort: 7123, ClickHouseTCPPort: 7000}, takenBy(8123, 9000), 7123, 7000, false},
		{"a configured port in use is refused", NativeHostDataConfig{ClickHouseTCPPort: 9000}, takenBy(9000), 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			http, tcp, err := c.cfg.chooseClickHousePorts(c.taken)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got %d/%d, want an error", http, tcp)
				}
				return
			}
			if err != nil || http != c.http || tcp != c.tcp {
				t.Fatalf("got %d/%d (%v), want %d/%d", http, tcp, err, c.http, c.tcp)
			}
		})
	}
}

func TestClickHousePortsSkipReservedApplicationPorts(t *testing.T) {
	m := &Manager{data: diskState{NativeApps: map[string]NativeApplication{"shop": {Port: "9000"}}}}
	http, tcp, err := NativeHostDataConfig{}.chooseClickHousePorts(m.clickhousePortTaken())
	if err != nil || tcp == 9000 || http == tcp {
		t.Fatalf("got %d/%d (%v); a stopped application's reserved port must not be reused", http, tcp, err)
	}
}
