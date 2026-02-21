package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListPods returns pods in the given namespace (empty = all namespaces).
func (c *ClusterClient) ListPods(ctx context.Context, namespace string, opts metav1.ListOptions) ([]corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Truncate to MaxPods
	if len(list.Items) > util.MaxPods {
		return list.Items[:util.MaxPods], nil
	}
	return list.Items, nil
}

// GetPod returns a single pod by name.
func (c *ClusterClient) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}
