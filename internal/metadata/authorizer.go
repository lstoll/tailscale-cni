package metadata

import (
	"strings"
	"sync"
)

// validFQDN returns true if domain looks like a reasonable FQDN (e.g. svc.magic.ts.net).
func validFQDN(domain string) bool {
	domain = strings.TrimSpace(domain)
	if domain == "" || len(domain) > 253 {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	for _, r := range domain {
		if r != '.' && r != '-' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// CertAuthorizer answers whether a pod (by IP) is allowed to request the TLS cert for a domain.
// Only pods that are current local backends for a Tailscale Service with that MagicDNS domain may request it.
type CertAuthorizer interface {
	AllowedCertDomain(callerPodIP, domain string) bool
	SetAllowedDomains(domainToPodIPs map[string][]string)
}

// CertAuthorizerImpl is a thread-safe implementation of CertAuthorizer.
type CertAuthorizerImpl struct {
	mu   sync.RWMutex
	byIP map[string]map[string]struct{} // domain -> set of pod IPs
}

// NewCertAuthorizer returns a new cert authorizer.
func NewCertAuthorizer() *CertAuthorizerImpl {
	return &CertAuthorizerImpl{byIP: make(map[string]map[string]struct{})}
}

// AllowedCertDomain reports whether the given caller pod IP may request the cert for domain.
// Domain is normalized (lowercase, trim) before lookup.
func (c *CertAuthorizerImpl) AllowedCertDomain(callerPodIP, domain string) bool {
	domain = normalizeDomain(domain)
	if domain == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ips, ok := c.byIP[domain]
	if !ok {
		return false
	}
	_, allowed := ips[callerPodIP]
	return allowed
}

// SetAllowedDomains replaces the allowed map: domain -> list of pod IPs that may request that domain's cert.
func (c *CertAuthorizerImpl) SetAllowedDomains(domainToPodIPs map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byIP = make(map[string]map[string]struct{})
	for domain, ips := range domainToPodIPs {
		domain = normalizeDomain(domain)
		if domain == "" {
			continue
		}
		set := make(map[string]struct{})
		for _, ip := range ips {
			if ip != "" {
				set[ip] = struct{}{}
			}
		}
		if len(set) > 0 {
			c.byIP[domain] = set
		}
	}
}

func normalizeDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}
