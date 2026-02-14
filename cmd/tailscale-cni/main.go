// tailscale-cni runs as a DaemonSet and configures the node for pod networking
// over Tailscale: writes CNI config (bridge+portmap), advertises the node's pod
// CIDR via Tailscale, ensures accept-routes is on, and sets up nftables masq.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lstoll/tailscale-cni/internal/cni"
	"github.com/lstoll/tailscale-cni/internal/controller"
	"github.com/lstoll/tailscale-cni/internal/masq"
	"github.com/lstoll/tailscale-cni/internal/metadata"
	"github.com/lstoll/tailscale-cni/internal/routes"
	"github.com/lstoll/tailscale-cni/internal/serve"
	"github.com/lstoll/tailscale-cni/internal/tailscale"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"net/netip"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

func main() {
	cniDir := flag.String("cni-dir", defaultEnv("CNI_DIR", "/etc/cni/net.d"), "Host path to write CNI conflist")
	cniBinDir := flag.String("cni-bin-dir", defaultEnv("CNI_BIN_DIR", ""), "If set, copy bridge/host-local/portmap from -cni-plugin-source into this dir (host plugin path)")
	cniPluginSource := flag.String("cni-plugin-source", defaultEnv("CNI_PLUGIN_SOURCE", "/cni"), "Path to built-in CNI plugins in the container (source for copy)")
	bridgeName := flag.String("bridge", "cni0", "Bridge name for CNI")
	clusterCIDR := flag.String("cluster-cidr", defaultEnv("CLUSTER_CIDR", "10.99.0.0/16"), "Cluster pod CIDR (for routes and CNI config)")
	tailscaleSocket := flag.String("tailscale-socket", "", "Path to Tailscale socket (default: platform default)")
	tailscaleIface := flag.String("tailscale-interface", "tailscale0", "Tailscale interface name for masq")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Current node name")
	resyncPeriod := flag.Duration("resync-period", 30*time.Minute, "How often to full resync node cache (informer resync)")
	metadataPort := flag.Int("metadata-port", 4160, "Port for metadata service on 127.0.0.1 (0 to disable)")
	flag.Parse()

	if *nodeName == "" {
		log.Fatal("node-name or NODE_NAME is required")
	}

	// K8s client (in-cluster or kubeconfig)
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("kube clientset: %v", err)
	}

	tsClient := tailscale.NewClient(*tailscaleSocket)
	routeManager := routes.NewManager(*tailscaleIface)

	opts := runReconcileOpts{
		tsClient:            tsClient,
		cniDir:               *cniDir,
		cniBinDir:            *cniBinDir,
		cniPluginSource:     *cniPluginSource,
		bridgeName:           *bridgeName,
		clusterCIDR:          *clusterCIDR,
		tailscaleIface:       *tailscaleIface,
		metadataListenPort:   *metadataPort,
	}

	serveState := &serveReconcileState{}
	podResolver := metadata.NewPodStoreResolver(nil)
	certAuthorizer := metadata.NewCertAuthorizer()
	ctrl, err := controller.New(kubeConfig, *nodeName, func(ctx context.Context, ourPodCIDR string) error {
		return runReconcile(ctx, opts, ourPodCIDR)
	},
		controller.WithResyncPeriod(*resyncPeriod),
		controller.WithOtherRoutesReconciler(func(ctx context.Context, store cache.Store) error {
			return reconcileOtherNodeRoutes(ctx, store, *nodeName, tsClient, routeManager)
		}),
		controller.WithServeReconciler(func(ctx context.Context, nodeStore, serviceStore, endpointSliceStore cache.Store) error {
			return reconcileServe(ctx, *nodeName, tsClient, clientset, serveState, certAuthorizer, nodeStore, serviceStore, endpointSliceStore)
		}),
		controller.WithPodStoreReceiver(func(store cache.Store) {
			podResolver.SetStore(store)
		}),
	)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metadataPort > 0 {
		tokenStore := metadata.NewTokenStore()
		metaSrv := metadata.NewServer(tsClient, tokenStore, podResolver, certAuthorizer, net.JoinHostPort("127.0.0.1", strconv.Itoa(*metadataPort)))
		go func() {
			if err := metaSrv.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("metadata server: %v", err)
			}
		}()
	}

	ctrl.Run(ctx)
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type runReconcileOpts struct {
	tsClient            *tailscale.Client
	cniDir               string
	cniBinDir            string
	cniPluginSource      string
	bridgeName           string
	clusterCIDR          string
	tailscaleIface       string
	metadataListenPort   int
}

func runReconcile(ctx context.Context, o runReconcileOpts, ourPodCIDR string) error {
	if ourPodCIDR == "" {
		return nil
	}

	// 1) Optionally copy built-in CNI plugins to host, then write CNI config
	if o.cniBinDir != "" {
		if err := cni.CopyPlugins(o.cniPluginSource, o.cniBinDir); err != nil {
			return fmt.Errorf("copy CNI plugins: %w", err)
		}
	}
	if err := cni.WriteConflist(o.cniDir, "tailscale-cni", o.bridgeName, ourPodCIDR, o.clusterCIDR); err != nil {
		return fmt.Errorf("write CNI config: %w", err)
	}

	// 2) Advertise our pod CIDR via Tailscale and ensure we accept routes
	prefix, err := netip.ParsePrefix(ourPodCIDR)
	if err != nil {
		return fmt.Errorf("parse pod CIDR: %w", err)
	}
	log.Printf("advertising route %s via Tailscale (approve in admin console if using ACLs)", ourPodCIDR)
	if err := o.tsClient.AdvertiseRoute(ctx, prefix); err != nil {
		return fmt.Errorf("advertise route %s via Tailscale: %w (is tailscaled running on this node?)", ourPodCIDR, err)
	}
	if err := o.tsClient.EnsureAcceptRoutes(ctx, true); err != nil {
		return fmt.Errorf("enable accept-routes: %w", err)
	}

	// 3) Masq traffic from our pod CIDR that goes out the host (internet); exclude bridge and Tailscale.
	//    If metadata port is set, also add prerouting DNAT for metadata service IP.
	metadataPort := 0
	if o.metadataListenPort > 0 {
		metadataPort = o.metadataListenPort
	}
	if err := masq.Setup(ourPodCIDR, o.bridgeName, o.tailscaleIface, metadataPort); err != nil {
		return fmt.Errorf("nftables masq: %w", err)
	}

	return nil
}

// serveReconcileState tracks which Tailscale service names we manage so we can
// remove them from the serve config when they no longer have local endpoints.
type serveReconcileState struct {
	mu           sync.Mutex
	managedNames map[tailcfg.ServiceName]struct{}
}

// reconcileServe updates Tailscale serve config for LoadBalancer Services with our
// loadBalancerClass that have at least one local endpoint, and patches Service status.
// If certAuthorizer is non-nil, it also updates the cert authorizer so only pods
// serving each service may request that service's TLS cert via the metadata API.
func reconcileServe(
	ctx context.Context,
	nodeName string,
	tsClient *tailscale.Client,
	clientset kubernetes.Interface,
	state *serveReconcileState,
	certAuthorizer metadata.CertAuthorizer,
	nodeStore, serviceStore, endpointSliceStore cache.Store,
) error {
	obj, exists, _ := nodeStore.GetByKey(nodeName)
	if !exists {
		return nil
	}
	node, _ := obj.(*corev1.Node)
	if node == nil {
		return nil
	}
	podCIDR := node.Spec.PodCIDR

	services := listServices(serviceStore)
	slices := listEndpointSlices(endpointSliceStore)
	desired, managed := serve.BuildDesiredServices(nodeName, podCIDR, services, slices)

	current, err := tsClient.GetServeConfig(ctx)
	if err != nil {
		return fmt.Errorf("get serve config: %w", err)
	}
	if current == nil {
		current = &ipn.ServeConfig{}
	}
	if current.Services == nil {
		current.Services = make(map[tailcfg.ServiceName]*ipn.ServiceConfig)
	}

	state.mu.Lock()
	lastManaged := state.managedNames
	state.managedNames = make(map[tailcfg.ServiceName]struct{})
	for _, n := range managed {
		state.managedNames[n] = struct{}{}
	}
	state.mu.Unlock()

	// De-register: remove from serve config any service we previously advertised
	// but no longer have local endpoints for (so we stop announcing it to Tailscale).
	for name := range lastManaged {
		if _, keep := state.managedNames[name]; !keep {
			delete(current.Services, name)
		}
	}
	for name, cfg := range desired {
		current.Services[name] = cfg
	}

	if err := tsClient.SetServeConfig(ctx, current); err != nil {
		return fmt.Errorf("set serve config: %w", err)
	}

	// Tell the control plane we advertise these services (so "Advertising the service" shows in admin).
	prefs, err := tsClient.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}
	advertiseList := make([]string, 0, len(prefs.AdvertiseServices)+len(managed))
	for _, s := range prefs.AdvertiseServices {
		if _, wasOurs := lastManaged[tailcfg.ServiceName(s)]; !wasOurs {
			advertiseList = append(advertiseList, s)
		}
	}
	for _, n := range managed {
		advertiseList = append(advertiseList, string(n))
	}
	if err := tsClient.SetAdvertiseServices(ctx, advertiseList); err != nil {
		return fmt.Errorf("set advertise services: %w", err)
	}
	if len(managed) > 0 {
		log.Printf("serve: advertising %d Tailscale Service(s) to control plane", len(managed))
	}

	managedSet := make(map[tailcfg.ServiceName]struct{})
	for _, n := range managed {
		managedSet[n] = struct{}{}
	}
	st, err := tsClient.Status(ctx)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	magicDNS := magicDNSSuffix(st)
	if magicDNS != "" {
		for _, svc := range services {
			if !serve.IsOurLoadBalancerService(svc) {
				continue
			}
			svcName := serve.TailscaleServiceName(svc)
			if _, ok := managedSet[svcName]; !ok {
				continue
			}
			hostname := string(svcName.WithoutPrefix()) + "." + magicDNS
			if err := patchServiceLoadBalancerHostname(ctx, clientset, svc.Namespace, svc.Name, hostname); err != nil {
				log.Printf("serve: patch service %s/%s status: %v", svc.Namespace, svc.Name, err)
			}
		}
	}
	if certAuthorizer != nil {
		domainToPodIPs := make(map[string][]string)
		if magicDNS != "" {
			svcNameToPodIPs := serve.LocalPodIPsByServiceName(nodeName, podCIDR, services, slices)
			for svcName, ips := range svcNameToPodIPs {
				domain := string(svcName.WithoutPrefix()) + "." + magicDNS
				domainToPodIPs[domain] = ips
			}
		}
		certAuthorizer.SetAllowedDomains(domainToPodIPs)
	}
	return nil
}

func listServices(store cache.Store) []*corev1.Service {
	var out []*corev1.Service
	for _, obj := range store.List() {
		if svc, ok := obj.(*corev1.Service); ok {
			out = append(out, svc)
		}
	}
	return out
}

func listEndpointSlices(store cache.Store) []*discoveryv1.EndpointSlice {
	var out []*discoveryv1.EndpointSlice
	for _, obj := range store.List() {
		if es, ok := obj.(*discoveryv1.EndpointSlice); ok {
			out = append(out, es)
		}
	}
	return out
}

func magicDNSSuffix(st *ipnstate.Status) string {
	if st == nil {
		return ""
	}
	if st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSSuffix != "" {
		return st.CurrentTailnet.MagicDNSSuffix
	}
	return st.MagicDNSSuffix
}

func patchServiceLoadBalancerHostname(ctx context.Context, clientset kubernetes.Interface, ns, name, hostname string) error {
	svc, err := clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if svc.Status.LoadBalancer.Ingress != nil {
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.Hostname == hostname {
				return nil
			}
		}
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: hostname}}
	_, err = clientset.CoreV1().Services(ns).UpdateStatus(ctx, svc, metav1.UpdateOptions{})
	return err
}

// reconcileOtherNodeRoutes builds desired routes: other nodes' pod CIDR -> our Tailscale IP.
// Using our own IP as gateway forces traffic out tailscale0; Tailscale then routes it
// to the peer that advertises that subnet.
func reconcileOtherNodeRoutes(ctx context.Context, store cache.Store, selfNodeName string, tsClient *tailscale.Client, routeManager *routes.Manager) error {
	list := store.List()
	st, _ := tsClient.Status(ctx)
	selfIP, ok := tailscale.SelfTailscaleIPv4(st)
	if !ok {
		return fmt.Errorf("no Tailscale IPv4 for this node (tailscale status has no TailscaleIPs)")
	}
	selfVia := selfIP.String()
	desired := make(map[string]string)
	for _, obj := range list {
		node, ok := obj.(*corev1.Node)
		if !ok || node.Name == selfNodeName {
			continue
		}
		cidr := node.Spec.PodCIDR
		if cidr == "" {
			continue
		}
		desired[cidr] = selfVia
	}
	return routeManager.EnsureRoutes(desired)
}
