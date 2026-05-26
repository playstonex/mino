package auth

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/lib"
)

var (
	tlsCert *tls.Certificate
	rootCAs *x509.CertPool
)

func SetTLSCredentials(ctx *lib.VPNContext, cert tls.Certificate, roots *x509.CertPool) {
	if ctx == nil {
		return
	}
	c := cert
	ctx.TLSCert = &c
	ctx.RootCAs = roots
	tlsCert = ctx.TLSCert
	rootCAs = ctx.RootCAs
}

func ClearTLSCredentials(ctx *lib.VPNContext) {
	if ctx != nil {
		ctx.TLSCert = nil
		ctx.RootCAs = nil
	}
	tlsCert = nil
	rootCAs = nil
}

func applyTLSOptions(config *tls.Config) error {
	if tlsCert != nil {
		config.Certificates = []tls.Certificate{*tlsCert}
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return tlsCert, nil
		}
	}
	if rootCAs != nil {
		config.RootCAs = rootCAs
	}

	pin := strings.TrimSpace(base.Cfg.ServerCertPin)
	if pin == "" {
		return nil
	}
	config.InsecureSkipVerify = true
	config.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyServerCertPin(pin, state)
	}
	return nil
}

func verifyServerCertPin(pin string, state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("server certificate pinning failed: no peer certificate")
	}

	leafDER := state.PeerCertificates[0].Raw
	pin = strings.TrimSpace(pin)
	lower := strings.ToLower(pin)

	switch {
	case strings.HasPrefix(lower, "sha256:"):
		expected, err := decodeHexFingerprint(pin[len("sha256:"):])
		if err != nil {
			return err
		}
		actual := sha256.Sum256(leafDER)
		if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
			return fmt.Errorf("server certificate pin mismatch for sha256")
		}
		return nil
	case strings.HasPrefix(lower, "sha1:"):
		expected, err := decodeHexFingerprint(pin[len("sha1:"):])
		if err != nil {
			return err
		}
		actual := sha1.Sum(leafDER)
		if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
			return fmt.Errorf("server certificate pin mismatch for sha1")
		}
		return nil
	case strings.HasPrefix(lower, "pin-sha256:"):
		expected := strings.TrimSpace(pin[len("pin-sha256:"):])
		sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
		actual := base64.StdEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(strings.TrimRight(expected, "=")), []byte(strings.TrimRight(actual, "="))) != 1 {
			return fmt.Errorf("server certificate pin mismatch for pin-sha256")
		}
		return nil
	default:
		return fmt.Errorf("unsupported server_cert format %q", pin)
	}
}

func decodeHexFingerprint(raw string) ([]byte, error) {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	normalized = strings.ReplaceAll(normalized, ":", "")
	normalized = strings.ReplaceAll(normalized, " ", "")
	if normalized == "" {
		return nil, fmt.Errorf("empty server certificate fingerprint")
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("invalid server certificate fingerprint %q: %w", raw, err)
	}
	return decoded, nil
}
