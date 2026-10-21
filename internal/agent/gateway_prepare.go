package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agim/lidza/pkg/gateway"
)

// PrepareGateway writes reviewable host credentials/configuration into a new
// private directory. It never edits Caddy, firewall, agent configuration or live
// services, and refuses to overwrite an existing credential directory.
func PrepareGateway(output string) error {
	if !filepath.IsAbs(output) {
		return errors.New("gateway output must be an absolute new directory")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return errors.New("gateway output directory must not already exist")
	}
	pki, err := newGatewayPKI("lidza-gateway.internal")
	if err != nil {
		return err
	}
	files := map[string]string{"ca.pem": pki.CA, "ca-key.pem": pki.CAKey, "agent.pem": pki.ServerCert, "agent-key.pem": pki.ServerKey, "edge.pem": pki.ClientCert, "edge-key.pem": pki.ClientKey}
	for name, value := range files {
		b, err := decodeGatewayPEM(value)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(output, name), b, 0600); err != nil {
			return err
		}
	}
	cert, err := gatewayCertificate(pki.ClientCert, pki.ClientKey)
	if err != nil {
		return err
	}
	cfg := GatewayConfig{Listen: "127.0.0.1:8444", Certificate: filepath.Join(output, "agent.pem"), PrivateKey: filepath.Join(output, "agent-key.pem"), ClientCA: filepath.Join(output, "ca.pem"), PeerPins: []string{gateway.Pin(cert.Leaf)}}
	b, err := json.MarshalIndent(map[string]any{"gateway": cfg}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "gateway-config.json"), b, 0600); err != nil {
		return err
	}
	snippet := fmt.Sprintf(`# Replace only the HTTPS site's existing reverse_proxy block after review.
# Keep the existing HTTP domain-proof route and ACME/on-demand policy unchanged.
reverse_proxy https://127.0.0.1:8444 {
 header_up Host {http.request.hostport}
 header_up Lidza-Gateway-Transport edge-headers-v1
 header_up Lidza-Gateway-Tls-Version {http.request.tls.version}
 header_up Lidza-Gateway-Authority {http.request.hostport}
 header_up Lidza-Gateway-Server-Name {http.request.tls.server_name}
 header_up Lidza-Gateway-Method {http.request.method}
 header_up Lidza-Gateway-Target {http.request.uri}
 header_up Lidza-Gateway-Issued {time.now.unix}
 header_up Lidza-Gateway-Nonce {http.request.uuid}
 transport http {
  tls_server_name lidza-gateway.internal
  tls_trust_pool file %s
  tls_client_auth %s %s
 }
}
`, filepath.Join(output, "ca.pem"), filepath.Join(output, "edge.pem"), filepath.Join(output, "edge-key.pem"))
	if err := os.WriteFile(filepath.Join(output, "Caddyfile.gateway"), []byte(snippet), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, "README.txt"), []byte("Merge gateway-config.json into the existing agent configuration; do not replace unrelated settings. Install the reviewed Caddy snippet only in the HTTPS site's proxy block. Grant the Caddy service read access to only ca.pem, edge.pem and edge-key.pem (for example root:caddy directory0750 and those files0640); keep all other keys root-only0600. Validate both configurations before coordinating service reloads. No production configuration has been changed. Back up all credentials securely. Rotation uses a new directory and overlapping CA/pin trust; do not regenerate an active identity in place.\n"), 0600)
}
