package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListServices returns services in the given namespace.
func (c *ClusterClient) ListServices(ctx context.Context, namespace string, opts metav1.ListOptions) ([]corev1.Service, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().Services(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListIngresses returns ingresses in the given namespace.
func (c *ClusterClient) ListIngresses(ctx context.Context, namespace string, opts metav1.ListOptions) ([]networkingv1.Ingress, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.NetworkingV1().Ingresses(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetEndpoints returns endpoints for a service.
func (c *ClusterClient) GetEndpoints(ctx context.Context, namespace, name string) (*corev1.Endpoints, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.CoreV1().Endpoints(namespace).Get(ctx, name, metav1.GetOptions{})
}
