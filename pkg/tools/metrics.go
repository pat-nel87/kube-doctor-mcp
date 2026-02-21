package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type getNodeMetricsInput struct{}

type getPodMetricsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter"`
}

type topResourceConsumersInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace (empty for all namespaces)"`
	Resource  string `json:"resource" jsonschema:"Resource to sort by: cpu or memory"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Number of top consumers to return (default 10)"`
}

func registerMetricsTools(server *mcp.Server, client *k8s.ClusterClient) {
	// get_node_metrics
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_node_metrics",
		Description: "Get CPU and memory usage for all nodes. Requires metrics-server to be installed in the cluster.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getNodeMetricsInput) (*mcp.CallToolResult, any, error) {
		metrics, err := client.GetNodeMetrics(ctx)
		if err != nil {
			return util.ErrorResult("Error getting node metrics: %v", err), nil, nil
		}

		// Get node capacity for utilization %
		nodes, _ := client.ListNodes(ctx, metav1.ListOptions{})
		capacityMap := make(map[string][2]int64) // [cpu millis, memory bytes]
		for _, n := range nodes {
			cpuCap := n.Status.Capacity.Cpu().MilliValue()
			memCap := n.Status.Capacity.Memory().Value()
			capacityMap[n.Name] = [2]int64{cpuCap, memCap}
		}

		headers := []string{"NODE", "CPU USAGE", "CPU %", "MEMORY USAGE", "MEMORY %"}
		rows := make([][]string, 0, len(metrics))
		for _, m := range metrics {
			cpuUsage := m.Usage.Cpu().MilliValue()
			memUsage := m.Usage.Memory().Value()

			cpuPct := "N/A"
			memPct := "N/A"
			if cap, ok := capacityMap[m.Name]; ok {
				if cap[0] > 0 {
					cpuPct = fmt.Sprintf("%.1f%%", float64(cpuUsage)/float64(cap[0])*100)
				}
				if cap[1] > 0 {
					memPct = fmt.Sprintf("%.1f%%", float64(memUsage)/float64(cap[1])*100)
				}
			}

			rows = append(rows, []string{
				m.Name,
				fmt.Sprintf("%dm", cpuUsage),
				cpuPct,
				formatBytes(memUsage),
				memPct,
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Node Metrics"))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("nodes with metrics", len(metrics))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_pod_metrics
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pod_metrics",
		Description: "Get CPU and memory usage for pods in a namespace. Requires metrics-server. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getPodMetricsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		metrics, err := client.GetPodMetrics(ctx, ns, opts)
		if err != nil {
			return util.ErrorResult("Error getting pod metrics: %v", err), nil, nil
		}

		headers := []string{"POD", "NAMESPACE", "CONTAINER", "CPU", "MEMORY"}
		rows := make([][]string, 0)
		for _, m := range metrics {
			for _, c := range m.Containers {
				rows = append(rows, []string{
					m.Name,
					m.Namespace,
					c.Name,
					fmt.Sprintf("%dm", c.Usage.Cpu().MilliValue()),
					formatBytes(c.Usage.Memory().Value()),
				})
			}
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Pod Metrics (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("pods with metrics", len(metrics))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// top_resource_consumers
	mcp.AddTool(server, &mcp.Tool{
		Name:        "top_resource_consumers",
		Description: "Find the top N pods by CPU or memory usage. Set resource='cpu' or resource='memory'. Requires metrics-server.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input topResourceConsumersInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		limit := input.Limit
		if limit <= 0 {
			limit = util.DefaultTopLimit
		}

		metrics, err := client.GetPodMetrics(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.ErrorResult("Error getting pod metrics: %v", err), nil, nil
		}

		type podUsage struct {
			Name      string
			Namespace string
			CPU       int64
			Memory    int64
		}

		var usages []podUsage
		for _, m := range metrics {
			var totalCPU, totalMem int64
			for _, c := range m.Containers {
				totalCPU += c.Usage.Cpu().MilliValue()
				totalMem += c.Usage.Memory().Value()
			}
			usages = append(usages, podUsage{
				Name:      m.Name,
				Namespace: m.Namespace,
				CPU:       totalCPU,
				Memory:    totalMem,
			})
		}

		// Sort by requested resource
		switch strings.ToLower(input.Resource) {
		case "memory", "mem":
			sort.Slice(usages, func(i, j int) bool { return usages[i].Memory > usages[j].Memory })
		default: // cpu
			sort.Slice(usages, func(i, j int) bool { return usages[i].CPU > usages[j].CPU })
		}

		if len(usages) > limit {
			usages = usages[:limit]
		}

		headers := []string{"#", "POD", "NAMESPACE", "CPU", "MEMORY"}
		rows := make([][]string, 0, len(usages))
		for i, u := range usages {
			rows = append(rows, []string{
				fmt.Sprintf("%d", i+1),
				u.Name,
				u.Namespace,
				fmt.Sprintf("%dm", u.CPU),
				formatBytes(u.Memory),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Top %d Resource Consumers by %s", limit, input.Resource)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// formatBytes formats bytes into human-readable format.
func formatBytes(b int64) string {
	const (
		ki = 1024
		mi = ki * 1024
		gi = mi * 1024
	)
	switch {
	case b >= gi:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(gi))
	case b >= mi:
		return fmt.Sprintf("%.1fMi", float64(b)/float64(mi))
	case b >= ki:
		return fmt.Sprintf("%.1fKi", float64(b)/float64(ki))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
