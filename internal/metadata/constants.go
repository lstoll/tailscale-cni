package metadata

const (
	// MetadataIP is the link-local IP pods use to reach the metadata service (distinct from AWS 169.254.169.254).
	MetadataIP = "169.254.169.253"
	// MetadataPort is the port on MetadataIP (and the port we redirect to on loopback).
	MetadataPort = 80

	// TokenTTLHeader is the header for PUT token request (seconds, 1–21600).
	TokenTTLHeader = "X-Tailscale-Metadata-Token-TTL-Seconds"
	// TokenHeader is the header for GET requests that require a token.
	TokenHeader = "X-Tailscale-Metadata-Token"

	// PathToken is the path for PUT to obtain a session token (IMDSv2-style).
	PathToken = "/metadata/api/token"
	// PathIdentity is the path for GET identity for a tailnet IP (query: ip=).
	PathIdentity = "/metadata/identity"
	// PathCert is the path for GET TLS cert+key for a service domain (query: domain=).
	PathCert = "/metadata/cert"
)
