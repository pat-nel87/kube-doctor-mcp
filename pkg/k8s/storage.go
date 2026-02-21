package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListPVCs returns PersistentVolumeClaims in the given namespace.
func (c *ClusterClient) ListPVCs(ctx context.Context, namespace string, opts metav1.ListOptions) ([]corev1.PersistentVolumeClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListPVs returns all PersistentVolumes.
func (c *ClusterClient) ListPVs(ctx context.Context) ([]corev1.PersistentVolume, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
