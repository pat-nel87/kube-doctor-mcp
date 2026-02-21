package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// GetNodeMetrics returns resource usage metrics for all nodes.
func (c *ClusterClient) GetNodeMetrics(ctx context.Context) ([]metricsv1beta1.NodeMetrics, error) {
	if c.MetricsClient == nil {
		return nil, fmt.Errorf("metrics-server not available (MetricsClient is nil)")
	}

	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.MetricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("metrics-server error (is metrics-server installed?): %w", err)
	}
	return list.Items, nil
}

// GetPodMetrics returns resource usage metrics for pods in a namespace.
func (c *ClusterClient) GetPodMetrics(ctx context.Context, namespace string, opts metav1.ListOptions) ([]metricsv1beta1.PodMetrics, error) {
	if c.MetricsClient == nil {
		return nil, fmt.Errorf("metrics-server not available (MetricsClient is nil)")
	}

	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	list, err := c.MetricsClient.MetricsV1beta1().PodMetricses(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("metrics-server error (is metrics-server installed?): %w", err)
	}
	return list.Items, nil
}
