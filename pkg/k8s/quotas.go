package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListResourceQuotas returns resource quotas in the given namespace.
func (c *ClusterClient) ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
