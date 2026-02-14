package serve

import (
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

// BuildDesiredServices computes the Tailscale ServeConfig.Services entries we should
// manage for this node: Services with our loadBalancerClass that have at least one
// local endpoint. Backend is pod IP:port. Returns the map to merge into ServeConfig.Services
// and the list of Tailscale service names we manage (so the caller can remove any not in this set).
func BuildDesiredServices(
	nodeName string,
	podCIDR string,
	services []*corev1.Service,
	allEndpointSlices []*discoveryv1.EndpointSlice,
) (map[tailcfg.ServiceName]*ipn.ServiceConfig, []tailcfg.ServiceName) {
	desired := make(map[tailcfg.ServiceName]*ipn.ServiceConfig)
	var managed []tailcfg.ServiceName

	for _, svc := range services {
		if !IsOurLoadBalancerService(svc) {
			continue
		}
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		slices := endpointSlicesForService(svc, allEndpointSlices)
		localEndpoints := localEndpointsForService(nodeName, podCIDR, slices)
		if len(localEndpoints) == 0 {
			continue
		}
		svcName := TailscaleServiceName(svc)
		cfg := buildServiceConfig(svc, localEndpoints)
		if cfg == nil {
			continue
		}
		desired[svcName] = cfg
		managed = append(managed, svcName)
	}
	return desired, managed
}

// IsOurLoadBalancerService reports whether the Service uses our loadBalancerClass.
func IsOurLoadBalancerService(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	if svc.Spec.LoadBalancerClass == nil {
		return false
	}
	return *svc.Spec.LoadBalancerClass == LoadBalancerClass
}

func endpointSlicesForService(svc *corev1.Service, all []*discoveryv1.EndpointSlice) []*discoveryv1.EndpointSlice {
	var out []*discoveryv1.EndpointSlice
	for _, es := range all {
		if es.Namespace != svc.Namespace {
			continue
		}
		if es.Labels[discoveryv1.LabelServiceName] != svc.Name {
			continue
		}
		out = append(out, es)
	}
	return out
}

// localEndpoint is a pod address and its ports (port name -> port number).
type localEndpoint struct {
	address string
	ports   map[string]int32 // port name -> port number
}

func localEndpointsForService(nodeName, podCIDR string, slices []*discoveryv1.EndpointSlice) []localEndpoint {
	var out []localEndpoint
	seen := make(map[string]bool)
	for _, es := range slices {
		portByName := make(map[string]int32)
		for _, p := range es.Ports {
			if p.Port != nil && p.Name != nil {
				portByName[*p.Name] = *p.Port
			}
		}
		for i := range es.Endpoints {
			ep := &es.Endpoints[i]
			if !isEndpointOnNode(ep, nodeName, podCIDR) {
				continue
			}
			if len(ep.Addresses) == 0 {
				continue
			}
			addr := ep.Addresses[0]
			if seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, localEndpoint{address: addr, ports: portByName})
		}
	}
	return out
}

func isEndpointOnNode(ep *discoveryv1.Endpoint, nodeName, podCIDR string) bool {
	if ep.NodeName != nil && *ep.NodeName == nodeName {
		return true
	}
	if podCIDR == "" {
		return false
	}
	if len(ep.Addresses) == 0 {
		return false
	}
	ip := net.ParseIP(ep.Addresses[0])
	if ip == nil {
		return false
	}
	_, cidr, err := net.ParseCIDR(podCIDR)
	if err != nil {
		return false
	}
	return cidr.Contains(ip)
}

func buildServiceConfig(svc *corev1.Service, localEndpoints []localEndpoint) *ipn.ServiceConfig {
	if len(localEndpoints) == 0 {
		return nil
	}
	// Use first local endpoint as the backend for all ports (Tailscale allows one TCPForward per port).
	first := localEndpoints[0]
	cfg := &ipn.ServiceConfig{TCP: make(map[uint16]*ipn.TCPPortHandler)}
	for _, p := range svc.Spec.Ports {
		if p.Protocol != corev1.ProtocolTCP {
			continue
		}
		backendPort := resolvePort(first.ports, &p)
		if backendPort <= 0 {
			continue
		}
		servePort := uint16(p.Port)
		cfg.TCP[servePort] = &ipn.TCPPortHandler{
			TCPForward: net.JoinHostPort(first.address, strconv.Itoa(int(backendPort))),
		}
	}
	if len(cfg.TCP) == 0 {
		return nil
	}
	return cfg
}

func resolvePort(portByName map[string]int32, svcPort *corev1.ServicePort) int32 {
	switch svcPort.TargetPort.Type {
	case intstr.Int:
		return svcPort.TargetPort.IntVal
	case intstr.String:
		if n, ok := portByName[svcPort.TargetPort.StrVal]; ok {
			return n
		}
		return 0
	default:
		return 0
	}
}
