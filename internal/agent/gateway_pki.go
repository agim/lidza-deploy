package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"time"

	"github.com/agim/lidza/pkg/gateway"
)

// All private material is persisted only inside encrypted agent state. The app
// receives its own leaf key, never the CA key or the agent client key.
type gatewayPKI struct {
	CA         string `json:"ca"`
	CAKey      string `json:"ca_key"`
	ServerCert string `json:"server_cert"`
	ServerKey  string `json:"server_key"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
}

func encodeGatewayPEM(kind string, b []byte) string {
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}))
}
func decodeGatewayPEM(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) > 16384 {
		return nil, errors.New("invalid retained gateway credential")
	}
	return b, nil
}
func gatewaySerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
func newGatewayPKI(name string) (gatewayPKI, error) {
	var out gatewayPKI
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return out, err
	}
	serial, err := gatewaySerial()
	if err != nil {
		return out, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Lidza private gateway CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return out, err
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		return out, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return out, err
	}
	out.CA = encodeGatewayPEM("CERTIFICATE", caDER)
	out.CAKey = encodeGatewayPEM("PRIVATE KEY", keyDER)
	issue := func(host string, usage x509.ExtKeyUsage) (string, string, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		serial, err := gatewaySerial()
		if err != nil {
			return "", "", err
		}
		template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			return "", "", err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return "", "", err
		}
		return encodeGatewayPEM("CERTIFICATE", der), encodeGatewayPEM("PRIVATE KEY", keyDER), nil
	}
	out.ServerCert, out.ServerKey, err = issue(name, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return out, err
	}
	out.ClientCert, out.ClientKey, err = issue("lidza-relay.internal", x509.ExtKeyUsageClientAuth)
	return out, err
}
func gatewayCertificate(cert, key string) (tls.Certificate, error) {
	c, err := decodeGatewayPEM(cert)
	if err != nil {
		return tls.Certificate{}, err
	}
	k, err := decodeGatewayPEM(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		return pair, errors.New("retained gateway certificate/key is invalid")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return pair, err
	}
	if time.Until(pair.Leaf.NotAfter) < 24*time.Hour {
		return pair, errors.New("gateway certificate expires within 24 hours; rotate credentials before deployment")
	}
	return pair, nil
}
func gatewayRoots(ca string) (*x509.CertPool, error) {
	b, err := decodeGatewayPEM(ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("gateway CA is invalid")
	}
	return pool, nil
}
func (p gatewayPKI) client() (*gateway.Transport, error) {
	cert, err := gatewayCertificate(p.ClientCert, p.ClientKey)
	if err != nil {
		return nil, err
	}
	pool, err := gatewayRoots(p.CA)
	if err != nil {
		return nil, err
	}
	return gateway.ClientTransport(cert, pool, "lidza-app.internal")
}

// Renew leaf certificates before expiry using the retained keys/CA. Public-key
// pins and old-release trust stay stable; this never invents a replacement CA.
func renewGatewayPKI(p gatewayPKI) (gatewayPKI, bool, error) {
	now := time.Now()
	caPEM, err := decodeGatewayPEM(p.CA)
	if err != nil {
		return p, false, err
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return p, false, errors.New("retained gateway CA is invalid")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || now.After(ca.NotAfter.Add(-48*time.Hour)) {
		return p, false, errors.New("gateway CA needs explicit coordinated rotation")
	}
	keyPEM, err := decodeGatewayPEM(p.CAKey)
	if err != nil {
		return p, false, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return p, false, errors.New("retained gateway CA key is invalid")
	}
	caKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return p, false, errors.New("retained gateway CA key is invalid")
	}
	changed := false
	renew := func(encoded, key string) (string, error) {
		certPEM, err := decodeGatewayPEM(encoded)
		if err != nil {
			return "", err
		}
		privatePEM, err := decodeGatewayPEM(key)
		if err != nil {
			return "", err
		}
		pair, err := tls.X509KeyPair(certPEM, privatePEM)
		if err != nil {
			return "", errors.New("retained gateway leaf key is invalid")
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return "", err
		}
		if cert.NotAfter.After(now.AddDate(0, 0, 30)) {
			return encoded, nil
		}
		serial, err := gatewaySerial()
		if err != nil {
			return "", err
		}
		cert.SerialNumber = serial
		cert.NotBefore = now.Add(-5 * time.Minute)
		cert.NotAfter = now.AddDate(1, 0, 0)
		if cert.NotAfter.After(ca.NotAfter) {
			cert.NotAfter = ca.NotAfter
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, cert.PublicKey, caKey)
		if err != nil {
			return "", errors.New("retained gateway CA cannot renew leaf")
		}
		changed = true
		return encodeGatewayPEM("CERTIFICATE", der), nil
	}
	p.ServerCert, err = renew(p.ServerCert, p.ServerKey)
	if err != nil {
		return p, false, err
	}
	p.ClientCert, err = renew(p.ClientCert, p.ClientKey)
	return p, changed, err
}
