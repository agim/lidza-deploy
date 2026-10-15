package agent

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"strings"
)

//go:embed scanner-firewall.py
var scannerFirewallHelper string

// ConfigureScannerFirewall uses Caddy's supported configuration API and the OS
// Fail2ban/nftables services. It does not modify application auth or SSH policy.
func ConfigureScannerFirewall(ctx context.Context, mode, output, allow string, direct bool) error {
	args := []string{"-"}
	switch mode {
	case "prepare":
		if output == "" {
			return errors.New("scanner firewall preparation requires --scanner-firewall-output")
		}
		args = append(args, "--output", output)
	case "enable":
		if !direct {
			return errors.New("automatic network bans require --direct-public-traffic; CDN/proxy setups need edge-side blocking")
		}
		args = append(args, "--enable", "--direct-public-traffic")
	case "disable":
		args = append(args, "--disable")
	default:
		return errors.New("scanner firewall mode must be prepare, enable or disable")
	}
	for _, address := range strings.Split(allow, ",") {
		if strings.TrimSpace(address) != "" {
			args = append(args, "--allow-ip", strings.TrimSpace(address))
		}
	}
	cmd := exec.CommandContext(ctx, "python3", args...)
	cmd.Stdin = strings.NewReader(scannerFirewallHelper)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
