// Package controller runs a node watch and reconciles only when the current
// node's pod CIDR (or relevant state) actually changes, using a cache to avoid
// redundant work.
package controller

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Reconciler is called when the desired state has changed and the controller
// should apply configuration (CNI, Tailscale, masq).
type Reconciler func(ctx context.Context, ourPodCIDR string) error

// OtherRoutesReconciler is called when any node is added/updated/deleted so the
// caller can update system routes to other nodes' pod CIDRs (e.g. via Tailscale).
// It receives the node informer store to list all nodes.
type OtherRoutesReconciler func(ctx context.Context, store cache.Store) error

// Controller watches nodes and triggers reconciliation when our node's pod
// CIDR changes. It caches the last applied pod CIDR so we only act on real changes.
// If OtherRoutesReconciler is set, it is also run on any node add/update/delete.
type Controller struct {
	clientset   kubernetes.Interface
	nodeName    string
	resyncPeriod time.Duration
	store       cache.Store // set in Run() so reconcile can list nodes

	reconcile            Reconciler
	otherRoutesReconcile OtherRoutesReconciler

	mu              sync.Mutex
	lastAppliedCIDR string // last pod CIDR we successfully reconciled for
}

// Option configures the controller.
type Option func(*Controller)

// WithResyncPeriod sets the informer's resync period. When non-zero, the cache
// is periodically re-listed so we recover from missed events.
func WithResyncPeriod(d time.Duration) Option {
	return func(c *Controller) { c.resyncPeriod = d }
}

// WithOtherRoutesReconciler sets the callback run on any node add/update/delete
// so routes to other nodes' pod CIDRs can be updated.
func WithOtherRoutesReconciler(fn OtherRoutesReconciler) Option {
	return func(c *Controller) { c.otherRoutesReconcile = fn }
}

// New returns a controller that watches nodes and calls reconcile when our
// node's pod CIDR differs from the cached last-applied value.
func New(config *rest.Config, nodeName string, reconcile Reconciler, opts ...Option) (*Controller, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	c := &Controller{
		clientset: clientset,
		nodeName:  nodeName,
		reconcile: reconcile,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Run starts the node informer and blocks until ctx is done. It triggers
// reconciliation on node add/update when our node's pod CIDR is set or changed.
func (c *Controller) Run(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(c.clientset, c.resyncPeriod)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	c.store = nodeInformer.GetStore()
	_, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueueNode(obj)
			c.runOtherRoutesReconcile(ctx, c.store)
		},
		UpdateFunc: func(_, newObj interface{}) {
			c.enqueueNodeUpdate(nil, newObj)
			c.runOtherRoutesReconcile(ctx, c.store)
		},
		DeleteFunc: func(obj interface{}) {
			c.enqueueNode(obj)
			c.runOtherRoutesReconcile(ctx, c.store)
		},
	})
	if err != nil {
		log.Printf("controller: failed to add event handler: %v", err)
		return
	}

	factory.Start(ctx.Done())

	log.Print("controller: waiting for node cache sync")
	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced) {
		log.Print("controller: cache sync failed")
		return
	}
	log.Print("controller: node cache synced")

	// Run an immediate reconcile from cache (in case we missed events before sync)
	obj, exists, _ := c.store.GetByKey(c.nodeName)
	if exists {
		if n, ok := obj.(*corev1.Node); ok && n.Spec.PodCIDR != "" {
			log.Printf("controller: this node %q has pod CIDR %s", c.nodeName, n.Spec.PodCIDR)
		}
	}
	c.maybeReconcile(ctx)
	c.runOtherRoutesReconcile(ctx, c.store)

	<-ctx.Done()
	log.Print("controller: stopping")
}

func (c *Controller) enqueueNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	if node.Name == c.nodeName {
		c.maybeReconcileFromNode(context.Background(), node)
	}
}

func (c *Controller) enqueueNodeUpdate(_, newObj interface{}) {
	node, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}
	if node.Name == c.nodeName {
		c.maybeReconcileFromNode(context.Background(), node)
	}
}

func (c *Controller) maybeReconcile(ctx context.Context) {
	obj, exists, err := c.store.GetByKey(c.nodeName)
	if err != nil {
		log.Printf("controller: get node from cache: %v", err)
		return
	}
	if !exists {
		return
	}
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	c.maybeReconcileFromNode(ctx, node)
}

func (c *Controller) maybeReconcileFromNode(ctx context.Context, node *corev1.Node) {
	podCIDR := node.Spec.PodCIDR

	c.mu.Lock()
	last := c.lastAppliedCIDR
	c.mu.Unlock()

	if podCIDR == "" {
		if last != "" {
			log.Printf("controller: node %q lost pod CIDR (was %s), skipping reconcile", c.nodeName, last)
		} else {
			log.Printf("controller: node %q has no spec.podCIDR yet; cannot write CNI config", c.nodeName)
		}
		return
	}

	if podCIDR == last {
		return
	}

	log.Printf("controller: pod CIDR changed %q -> %q, reconciling", last, podCIDR)
	if err := c.reconcile(ctx, podCIDR); err != nil {
		log.Printf("controller: reconcile failed: %v", err)
		return
	}

	c.mu.Lock()
	c.lastAppliedCIDR = podCIDR
	c.mu.Unlock()
	log.Printf("controller: reconciled pod CIDR %q", podCIDR)
}

func (c *Controller) runOtherRoutesReconcile(ctx context.Context, store cache.Store) {
	if c.otherRoutesReconcile == nil {
		return
	}
	if err := c.otherRoutesReconcile(ctx, store); err != nil {
		log.Printf("controller: other-routes reconcile failed: %v", err)
	}
}
