package serve

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"tailscale.com/tailcfg"
)

// TailscaleServiceName returns the Tailscale Service name (svc:...) for the given
// Kubernetes Service. Uses the annotation if set, otherwise a sanitized default.
// AsServiceName expects the "svc:" prefix.
func TailscaleServiceName(svc *corev1.Service) tailcfg.ServiceName {
	bare := ""
	if a := svc.Annotations[ServiceNameAnnotation]; a != "" {
		bare = strings.TrimSpace(a)
	} else {
		bare = defaultServiceName(svc.Namespace, svc.Name)
	}
	if bare == "" {
		return ""
	}
	return tailcfg.AsServiceName("svc:" + bare)
}

// defaultServiceName returns a DNS-label-safe name from namespace and name.
func defaultServiceName(ns, name string) string {
	s := "k8s-" + ns + "-" + name
	s = dnsLabelSanitize(s)
	if s == "" {
		s = "k8s-svc"
	}
	return s
}

// dnsLabelSanitize reduces s to a valid DNS label (lowercase alphanumeric and hyphen).
var dnsLabelRe = regexp.MustCompile(`[^a-z0-9-]+`)

func dnsLabelSanitize(s string) string {
	s = strings.ToLower(s)
	s = dnsLabelRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
		s = strings.Trim(s, "-")
	}
	return s
}
