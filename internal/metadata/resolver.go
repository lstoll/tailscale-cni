package metadata

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// PodResolver resolves a pod IP to namespace and name (for the current node's pods).
type PodResolver interface {
	PodForIP(ip string) (namespace, name string, ok bool)
}

// PodStoreResolver implements PodResolver by listing pods from a cache.Store (e.g. from pod informer).
type PodStoreResolver struct {
	store cache.Store
}

// NewPodStoreResolver returns a resolver that uses the given store. Store may be nil (all lookups return false).
func NewPodStoreResolver(store cache.Store) *PodStoreResolver {
	return &PodStoreResolver{store: store}
}

// SetStore updates the store (e.g. once the informer has synced).
func (r *PodStoreResolver) SetStore(store cache.Store) {
	r.store = store
}

// PodForIP returns the namespace and name of the pod with the given status.podIP, if any.
func (r *PodStoreResolver) PodForIP(ip string) (namespace, name string, ok bool) {
	if r.store == nil {
		return "", "", false
	}
	for _, obj := range r.store.List() {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if pod.Status.PodIP == ip {
			return pod.Namespace, pod.Name, true
		}
	}
	return "", "", false
}
