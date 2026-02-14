// Package metadata provides an IMDSv2-style metadata HTTP service for pods:
// token-protected endpoints to look up Tailscale identity for a tailnet IP.
package metadata

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lstoll/tailscale-cni/internal/tailscale"
)

// IdentityResponse is the JSON returned for GET /metadata/identity?ip=...
type IdentityResponse struct {
	Node        *NodeInfo        `json:"node,omitempty"`
	UserProfile *UserProfileInfo `json:"userProfile,omitempty"`
}

type NodeInfo struct {
	Name         string `json:"name,omitempty"`
	ComputedName string `json:"computedName,omitempty"`
	StableID     string `json:"stableId,omitempty"`
}

type UserProfileInfo struct {
	LoginName   string `json:"loginName,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// Server serves the metadata API (token + identity + cert).
type Server struct {
	tsClient       *tailscale.Client
	tokenStore     *TokenStore
	podResolver    PodResolver
	certAuthorizer CertAuthorizer
	listenAddr     string
	srv            *http.Server
}

// NewServer returns a metadata server. Call Run to start listening.
// certAuthorizer may be nil to disable the cert endpoint.
func NewServer(tsClient *tailscale.Client, tokenStore *TokenStore, podResolver PodResolver, certAuthorizer CertAuthorizer, listenAddr string) *Server {
	s := &Server{
		tsClient:       tsClient,
		tokenStore:     tokenStore,
		podResolver:    podResolver,
		certAuthorizer: certAuthorizer,
		listenAddr:     listenAddr,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(PathToken, s.servePutToken)
	mux.HandleFunc(PathIdentity, s.serveGetIdentity)
	mux.HandleFunc(PathCert, s.serveGetCert)
	s.srv = &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return s
}

// Run listens and serves until ctx is done. Returns when the server is shut down.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = s.srv.Shutdown(context.Background())
	}()
	log.Printf("metadata: listening on %s", s.listenAddr)
	return s.srv.Serve(ln)
}

func (s *Server) servePutToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ttlStr := r.Header.Get(TokenTTLHeader)
	if ttlStr == "" {
		http.Error(w, "missing "+TokenTTLHeader, http.StatusBadRequest)
		return
	}
	ttl, err := strconv.Atoi(ttlStr)
	if err != nil || ttl < 1 {
		http.Error(w, "invalid "+TokenTTLHeader, http.StatusBadRequest)
		return
	}
	if ttl > 21600 {
		ttl = 21600
	}
	token, err := s.tokenStore.Create(ttl)
	if err != nil {
		http.Error(w, "token creation failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(token))
}

func (s *Server) serveGetIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get(TokenHeader)
	if token == "" || !s.tokenStore.Valid(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ip := r.URL.Query().Get("ip")
	if ip == "" {
		http.Error(w, "missing query ip=", http.StatusBadRequest)
		return
	}
	ip = strings.TrimSpace(ip)
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if net.ParseIP(ip) == nil {
		http.Error(w, "invalid ip", http.StatusBadRequest)
		return
	}

	// Optional: log caller pod
	if s.podResolver != nil {
		callerIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ns, name, ok := s.podResolver.PodForIP(callerIP); ok {
			log.Printf("metadata: identity request for %s from pod %s/%s", ip, ns, name)
		}
	}

	who, err := s.tsClient.WhoIs(r.Context(), ip)
	if err != nil {
		log.Printf("metadata: WhoIs(%s): %v", ip, err)
		http.Error(w, "identity lookup failed", http.StatusNotFound)
		return
	}
	resp := &IdentityResponse{}
	if who.Node != nil {
		resp.Node = &NodeInfo{
			Name:         who.Node.Name,
			ComputedName: who.Node.ComputedName,
			StableID:     string(who.Node.StableID),
		}
	}
	if who.UserProfile != nil {
		resp.UserProfile = &UserProfileInfo{
			LoginName:   who.UserProfile.LoginName,
			DisplayName: who.UserProfile.DisplayName,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// CertResponse is the JSON returned for GET /metadata/cert?domain=...
type CertResponse struct {
	CertPEM string `json:"certPEM"`
	KeyPEM  string `json:"keyPEM"`
}

func (s *Server) serveGetCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get(TokenHeader)
	if token == "" || !s.tokenStore.Valid(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.certAuthorizer == nil {
		http.Error(w, "cert endpoint disabled", http.StatusNotFound)
		return
	}
	domain := r.URL.Query().Get("domain")
	domain = strings.TrimSpace(domain)
	if domain == "" {
		http.Error(w, "missing query domain=", http.StatusBadRequest)
		return
	}
	domain = normalizeDomain(domain)
	if !validFQDN(domain) {
		http.Error(w, "invalid domain", http.StatusBadRequest)
		return
	}
	callerIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		callerIP = r.RemoteAddr
	}
	if !s.certAuthorizer.AllowedCertDomain(callerIP, domain) {
		if s.podResolver != nil {
			if ns, name, ok := s.podResolver.PodForIP(callerIP); ok {
				log.Printf("metadata: cert denied for domain %s from pod %s/%s", domain, ns, name)
			}
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	certPEM, keyPEM, err := s.tsClient.CertPair(r.Context(), domain)
	if err != nil {
		log.Printf("metadata: CertPair(%s): %v", domain, err)
		http.Error(w, "cert lookup failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&CertResponse{CertPEM: string(certPEM), KeyPEM: string(keyPEM)})
}

// ParseIdentityURL returns the metadata base URL using the standard metadata IP and port.
// Useful for pods to build URLs: ParseIdentityURL() + "/metadata/identity?ip=" + ip
func ParseIdentityURL() string {
	return "http://" + MetadataIP + ":" + strconv.Itoa(MetadataPort)
}

// TokenURL returns the URL for the token endpoint.
func TokenURL() string {
	return "http://" + MetadataIP + ":" + strconv.Itoa(MetadataPort) + PathToken
}

// IdentityURL returns the URL for the identity endpoint with the given tailnet IP.
func IdentityURL(tailnetIP string) string {
	return "http://" + MetadataIP + ":" + strconv.Itoa(MetadataPort) + PathIdentity + "?ip=" + url.QueryEscape(tailnetIP)
}
