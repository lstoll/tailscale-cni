// tailscale-cni runs as a DaemonSet and configures the node for pod networking
// over Tailscale: writes CNI config (bridge+portmap), advertises the node's pod
// CIDR via Tailscale, ensures accept-routes is on and subnet SNAT is off, and sets up
// nftables NAT and pod ingress isolation.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lstoll/tailscale-cni/internal/cni"
	"github.com/lstoll/tailscale-cni/internal/controller"
	"github.com/lstoll/tailscale-cni/internal/firewall"
	"github.com/lstoll/tailscale-cni/internal/routes"
	"github.com/lstoll/tailscale-cni/internal/tailscale"
	"tailscale.com/types/preftype"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"net/netip"
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
	netfilterMode := flag.String("tailscale-netfilter-mode", defaultEnv("TAILSCALE_NETFILTER_MODE", ""), "If set (on, nodivert, off), set tailscaled's netfilter mode. nodivert keeps tailscaled's chains out of FORWARD/INPUT/POSTROUTING so they can't bypass network policy. Empty leaves it unchanged")
	ingressIsolation := flag.Bool("pod-ingress-isolation", defaultEnv("POD_INGRESS_ISOLATION", "true") == "true", "Drop traffic into pods unless it's from another pod, another cluster node, a reply, a Service/hostPort, or -pod-ingress-allow")
	ingressAllow := flag.String("pod-ingress-allow", defaultEnv("POD_INGRESS_ALLOW", ""), "Comma-separated IPv4 CIDRs allowed to reach pods directly when -pod-ingress-isolation is on (e.g. an admin's tailnet IP)")
	resyncPeriod := flag.Duration("resync-period", 30*time.Minute, "How often to full resync node cache (informer resync)")
	flag.Parse()

	if *nodeName == "" {
		log.Fatal("node-name or NODE_NAME is required")
	}

	// K8s client (in-cluster or kubeconfig)
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)

	}

	var nfMode *preftype.NetfilterMode
	if *netfilterMode != "" {
		m, err := preftype.ParseNetfilterMode(*netfilterMode)
		if err != nil {
			log.Fatalf("tailscale-netfilter-mode: %v", err)
		}
		nfMode = &m
	}

	clusterPrefix, err := netip.ParsePrefix(*clusterCIDR)
	if err != nil {
		log.Fatalf("cluster-cidr: %v", err)
	}
	allowed, err := parsePrefixes(*ingressAllow)
	if err != nil {
		log.Fatalf("pod-ingress-allow: %v", err)
	}
	fw, err := firewall.NewManager(firewall.Config{
		ClusterCIDR:      clusterPrefix,
		Bridge:           *bridgeName,
		TailscaleIface:   *tailscaleIface,
		IngressIsolation: *ingressIsolation,
		AllowedSources:   allowed,
	})
	if err != nil {
		log.Fatal(err)
	}

	tsClient := tailscale.NewClient(*tailscaleSocket)
	routeManager := routes.NewManager(*tailscaleIface)

	opts := runReconcileOpts{
		tsClient:        tsClient,
		cniDir:          *cniDir,
		cniBinDir:       *cniBinDir,
		cniPluginSource: *cniPluginSource,
		bridgeName:      *bridgeName,
		clusterCIDR:     *clusterCIDR,
		tailscaleIface:  *tailscaleIface,
		netfilterMode:   nfMode,
		fw:              fw,
	}

	ctrl, err := controller.New(kubeConfig, *nodeName, func(ctx context.Context, ourPodCIDR string) error {
		return runReconcile(ctx, opts, ourPodCIDR)
	},
		controller.WithResyncPeriod(*resyncPeriod),
		controller.WithOtherRoutesReconciler(func(ctx context.Context, store cache.Store) error {
			return reconcileOtherNodeRoutes(ctx, store, *nodeName, tsClient, routeManager, fw, clusterPrefix)
		}),
	)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tailscale route approval for a new node can land after its Node event,
	// so poll for cluster node IP changes too. Cheap: only applies on change.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := refreshClusterNodeIPs(ctx, tsClient, fw, clusterPrefix); err != nil {
					log.Printf("firewall: refresh cluster node IPs: %v", err)
				}
			}
		}
	}()

	ctrl.Run(ctx)
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			f += "/32"
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// refreshClusterNodeIPs updates the firewall's set of other cluster nodes'
// Tailscale IPs from tailscaled's view of who routes pod CIDRs.
func refreshClusterNodeIPs(ctx context.Context, tsClient *tailscale.Client, fw *firewall.Manager, clusterCIDR netip.Prefix) error {
	st, err := tsClient.Status(ctx)
	if err != nil {
		return err
	}
	return fw.SetClusterNodeIPs(tailscale.ClusterPeerIPv4s(st, clusterCIDR))
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type runReconcileOpts struct {
	tsClient        *tailscale.Client
	cniDir          string
	cniBinDir       string
	cniPluginSource string
	bridgeName      string
	clusterCIDR     string
	tailscaleIface  string
	netfilterMode   *preftype.NetfilterMode // nil = leave unchanged
	fw              *firewall.Manager
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
	// Keep original source IPs on traffic forwarded into our pod CIDR.
	if changed, err := o.tsClient.EnsureNoSNAT(ctx); err != nil {
		return fmt.Errorf("disable subnet route SNAT: %w", err)
	} else if changed {
		log.Print("disabled tailscaled subnet route SNAT (NoSNAT=true) so pods see real source IPs")
	}
	if o.netfilterMode != nil {
		prev, changed, err := o.tsClient.EnsureNetfilterMode(ctx, *o.netfilterMode)
		if err != nil {
			return fmt.Errorf("set tailscale netfilter mode: %w", err)
		}
		if changed {
			log.Printf("changed tailscaled netfilter mode from %s to %s", prev, *o.netfilterMode)
		}
	}

	// 3) NAT non-cluster egress as the node, and isolate pod ingress.
	if err := o.fw.SetPodCIDR(prefix); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}

	return nil
}

// reconcileOtherNodeRoutes builds desired routes: other nodes' pod CIDR -> our Tailscale IP.
// Using our own IP as gateway forces traffic out tailscale0; Tailscale then routes it
// to the peer that advertises that subnet.
func reconcileOtherNodeRoutes(ctx context.Context, store cache.Store, selfNodeName string, tsClient *tailscale.Client, routeManager *routes.Manager, fw *firewall.Manager, clusterCIDR netip.Prefix) error {
	list := store.List()
	st, _ := tsClient.Status(ctx)
	if st != nil {
		if err := fw.SetClusterNodeIPs(tailscale.ClusterPeerIPv4s(st, clusterCIDR)); err != nil {
			log.Printf("firewall: update cluster node IPs: %v", err)
		}
	}
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
