// cert-fetcher fetches the TLS cert for a service domain from the Tailscale CNI metadata API
// and writes cert and key to a directory. Run as an init container in a pod that serves the
// named service; only such pods are authorized to receive the cert.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	metadataBase = "http://169.254.169.253"
	tokenPath    = "/metadata/api/token"
	certPath     = "/metadata/cert"
)

func main() {
	domain := flag.String("domain", os.Getenv("METADATA_DOMAIN"), "Service domain (e.g. test-nginx.your-tailnet.ts.net)")
	certDir := flag.String("cert-dir", defaultEnv("METADATA_CERT_DIR", "/certs"), "Directory to write tls.crt and tls.key")
	ttl := flag.Int("token-ttl", 60, "Token TTL in seconds for metadata API")
	flag.Parse()
	if *domain == "" {
		fmt.Fprintln(os.Stderr, "domain (or METADATA_DOMAIN) is required")
		os.Exit(1)
	}
	*domain = strings.TrimSpace(*domain)
	if err := fetchAndWrite(*domain, *certDir, *ttl); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fetchAndWrite(domain, certDir string, tokenTTL int) error {
	client := &http.Client{Timeout: 15 * time.Second}

	// 1) Get token
	req, err := http.NewRequest(http.MethodPut, metadataBase+tokenPath, nil)
	if err != nil {
		return fmt.Errorf("new token request: %w", err)
	}
	req.Header.Set("X-Tailscale-Metadata-Token-TTL-Seconds", fmt.Sprintf("%d", tokenTTL))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token request: %s %s", resp.Status, string(body))
	}
	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	tokenStr := strings.TrimSpace(string(token))
	if tokenStr == "" {
		return fmt.Errorf("empty token")
	}

	// 2) Get cert
	certURL := metadataBase + certPath + "?domain=" + url.QueryEscape(domain)
	req, err = http.NewRequest(http.MethodGet, certURL, nil)
	if err != nil {
		return fmt.Errorf("new cert request: %w", err)
	}
	req.Header.Set("X-Tailscale-Metadata-Token", tokenStr)
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("cert request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cert request: %s %s", resp.Status, string(body))
	}
	var out struct {
		CertPEM string `json:"certPEM"`
		KeyPEM  string `json:"keyPEM"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode cert response: %w", err)
	}
	if out.CertPEM == "" || out.KeyPEM == "" {
		return fmt.Errorf("cert response missing certPEM or keyPEM")
	}

	// 3) Write files
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return fmt.Errorf("mkdir cert-dir: %w", err)
	}
	certFile := filepath.Join(certDir, "tls.crt")
	keyFile := filepath.Join(certDir, "tls.key")
	if err := os.WriteFile(certFile, []byte(out.CertPEM), 0644); err != nil {
		return fmt.Errorf("write tls.crt: %w", err)
	}
	if err := os.WriteFile(keyFile, []byte(out.KeyPEM), 0600); err != nil {
		return fmt.Errorf("write tls.key: %w", err)
	}
	return nil
}
