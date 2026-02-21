package k8s

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListEvents returns events in the given namespace, optionally filtered.
func (c *ClusterClient) ListEvents(ctx context.Context, namespace string, opts metav1.ListOptions) ([]corev1.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().Events(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Sort by last timestamp (most recent first)
	sort.Slice(list.Items, func(i, j int) bool {
		ti := list.Items[i].LastTimestamp.Time
		tj := list.Items[j].LastTimestamp.Time
		if ti.IsZero() {
			ti = list.Items[i].CreationTimestamp.Time
		}
		if tj.IsZero() {
			tj = list.Items[j].CreationTimestamp.Time
		}
		return ti.After(tj)
	})

	// Truncate to MaxEvents
	if len(list.Items) > util.MaxEvents {
		return list.Items[:util.MaxEvents], nil
	}
	return list.Items, nil
}

// GetEventsForObject returns events related to a specific object.
func (c *ClusterClient) GetEventsForObject(ctx context.Context, namespace, objectName string) ([]corev1.Event, error) {
	opts := metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + objectName,
	}
	return c.ListEvents(ctx, namespace, opts)
}
