// tailscale-cni runs as a DaemonSet and configures the node for pod networking
// over Tailscale: writes CNI config (bridge+portmap), advertises the node's pod
// CIDR via Tailscale, ensures accept-routes is on, and sets up nftables masq.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lstoll/tailscale-cni/internal/cni"
	"github.com/lstoll/tailscale-cni/internal/controller"
	"github.com/lstoll/tailscale-cni/internal/masq"
	"github.com/lstoll/tailscale-cni/internal/routes"
	"github.com/lstoll/tailscale-cni/internal/tailscale"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"net/netip"
)

func main() {
	cniDir := flag.String("cni-dir", defaultEnv("CNI_DIR", "/etc/cni/net.d"), "Host path to write CNI conflist")
	bridgeName := flag.String("bridge", "cni0", "Bridge name for CNI")
	clusterCIDR := flag.String("cluster-cidr", defaultEnv("CLUSTER_CIDR", "10.99.0.0/16"), "Cluster pod CIDR (for routes and CNI config)")
	tailscaleSocket := flag.String("tailscale-socket", "", "Path to Tailscale socket (default: platform default)")
	tailscaleIface := flag.String("tailscale-interface", "tailscale0", "Tailscale interface name for masq")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Current node name")
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

	tsClient := tailscale.NewClient(*tailscaleSocket)
	routeManager := routes.NewManager(*tailscaleIface)

	opts := runReconcileOpts{
		tsClient:       tsClient,
		cniDir:         *cniDir,
		bridgeName:     *bridgeName,
		clusterCIDR:    *clusterCIDR,
		tailscaleIface: *tailscaleIface,
	}

	ctrl, err := controller.New(kubeConfig, *nodeName, func(ctx context.Context, ourPodCIDR string) error {
		return runReconcile(ctx, opts, ourPodCIDR)
	},
		controller.WithResyncPeriod(*resyncPeriod),
		controller.WithOtherRoutesReconciler(func(ctx context.Context, store cache.Store) error {
			return reconcileOtherNodeRoutes(ctx, store, *nodeName, tsClient, routeManager)
		}),
	)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl.Run(ctx)
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type runReconcileOpts struct {
	tsClient       *tailscale.Client
	cniDir         string
	bridgeName     string
	clusterCIDR    string
	tailscaleIface string
}

func runReconcile(ctx context.Context, o runReconcileOpts, ourPodCIDR string) error {
	if ourPodCIDR == "" {
		return nil
	}

	// 1) Write CNI config so kubelet uses bridge + host-local + portmap
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

	// 3) Masq traffic from our pod CIDR that goes out the host (internet); exclude bridge and Tailscale
	if err := masq.Setup(ourPodCIDR, o.bridgeName, o.tailscaleIface); err != nil {
		return fmt.Errorf("nftables masq: %w", err)
	}

	return nil
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
