package k8s

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// ListDeployments returns deployments in the given namespace.
func (c *ClusterClient) ListDeployments(ctx context.Context, namespace string, opts metav1.ListOptions) ([]appsv1.Deployment, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.AppsV1().Deployments(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetDeployment returns a single deployment by name.
func (c *ClusterClient) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ListReplicaSets returns ReplicaSets in the given namespace.
func (c *ClusterClient) ListReplicaSets(ctx context.Context, namespace string, opts metav1.ListOptions) ([]appsv1.ReplicaSet, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListStatefulSets returns StatefulSets in the given namespace.
func (c *ClusterClient) ListStatefulSets(ctx context.Context, namespace string, opts metav1.ListOptions) ([]appsv1.StatefulSet, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.AppsV1().StatefulSets(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListDaemonSets returns DaemonSets in the given namespace.
func (c *ClusterClient) ListDaemonSets(ctx context.Context, namespace string, opts metav1.ListOptions) ([]appsv1.DaemonSet, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.AppsV1().DaemonSets(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListJobs returns Jobs in the given namespace.
func (c *ClusterClient) ListJobs(ctx context.Context, namespace string, opts metav1.ListOptions) ([]batchv1.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.BatchV1().Jobs(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListCronJobs returns CronJobs in the given namespace.
func (c *ClusterClient) ListCronJobs(ctx context.Context, namespace string, opts metav1.ListOptions) ([]batchv1.CronJob, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.Clientset.BatchV1().CronJobs(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
