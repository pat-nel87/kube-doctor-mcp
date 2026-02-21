package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListNodes returns all nodes, optionally filtered by label selector.
func (c *ClusterClient) ListNodes(ctx context.Context, opts metav1.ListOptions) ([]corev1.Node, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().Nodes().List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetNode returns a single node by name.
func (c *ClusterClient) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}
