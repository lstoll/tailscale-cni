// Package serve builds Tailscale ServeConfig from Kubernetes Services (type LoadBalancer
// with our loadBalancerClass) and EndpointSlices, for nodes that have local backends.
package serve

// Kubernetes Service spec.loadBalancerClass value that opts in to Tailscale Services.
const LoadBalancerClass = "lds.li/tailscale-cni"

// Service annotation for the Tailscale Service name (DNS label). If unset, we derive from metadata.name.
// ServiceNameAnnotation is the Service annotation for the Tailscale Service name (DNS label).
const ServiceNameAnnotation = "tailscale-cni.lds.li/service-name"
