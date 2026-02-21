package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/mermaid"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// --- Input structs ---

type analyzeResourceUsageInput struct {
	Namespace string `json:"namespace" jsonschema:"required,Kubernetes namespace to analyze resource usage in"`
}

type analyzeNodeCapacityInput struct{}

type analyzeResourceEfficiencyInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace (empty for cluster-wide analysis)"`
}

type analyzeNetworkPoliciesInput struct {
	Namespace string `json:"namespace" jsonschema:"required,Kubernetes namespace to analyze network policies in"`
}

type checkDNSHealthInput struct{}

func registerResourceAnalysisTools(server *mcp.Server, client *k8s.ClusterClient) {
	// -------------------------------------------------------------------------
	// 1. analyze_resource_usage
	// -------------------------------------------------------------------------
	mcp.AddTool(server, &mcp.Tool{
		Name: "analyze_resource_usage",
		Description: "Analyze actual CPU/memory usage vs requests and limits for every pod in a namespace. " +
			"Categories: CRITICAL (>90% of limit), WARNING (>70%), OVERPROVISIONED (<30% of request), " +
			"MISSING LIMITS. Includes namespace totals and a Mermaid xychart of top pods by CPU usage % of limit. " +
			"Requires metrics-server.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeResourceUsageInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace

		// Get pods
		pods, err := client.ListPods(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		// Get metrics
		podMetrics, err := client.GetPodMetrics(ctx, ns, metav1.ListOptions{})
		metricsAvailable := err == nil && len(podMetrics) > 0

		// Build metrics lookup: podName -> (totalCPU millis, totalMem bytes)
		type metricsData struct {
			cpuMillis int64
			memBytes  int64
		}
		metricsMap := make(map[string]metricsData)
		if metricsAvailable {
			for _, pm := range podMetrics {
				var cpu, mem int64
				for _, c := range pm.Containers {
					cpu += c.Usage.Cpu().MilliValue()
					mem += c.Usage.Memory().Value()
				}
				metricsMap[pm.Name] = metricsData{cpuMillis: cpu, memBytes: mem}
			}
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Resource Usage Analysis (namespace: %s)", ns)))
		sb.WriteString("\n\n")

		if !metricsAvailable {
			sb.WriteString(util.FormatFinding("WARNING", "Metrics server not available or returned no data. Usage data will be unavailable."))
			sb.WriteString("\n\n")
		}

		// Per-pod analysis
		type podAnalysis struct {
			name           string
			cpuRequest     int64
			cpuLimit       int64
			memRequest     int64
			memLimit       int64
			cpuUsage       int64
			memUsage       int64
			hasMetrics     bool
			cpuPctOfLimit  float64
			memPctOfLimit  float64
			cpuPctOfReq    float64
			memPctOfReq    float64
			missingLimits  bool
			missingReqs    bool
			category       string // CRITICAL, WARNING, OVERPROVISIONED, MISSING LIMITS, OK
		}

		analyses := make([]podAnalysis, 0, len(pods))
		var nsTotalCPUReq, nsTotalCPULim, nsTotalMemReq, nsTotalMemLim int64
		var nsTotalCPUUsage, nsTotalMemUsage int64
		criticalCount, warningCount, overprovisionedCount, missingLimitsCount := 0, 0, 0, 0

		for _, pod := range pods {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}

			pa := podAnalysis{name: pod.Name}

			// Sum requests/limits across all containers
			for _, c := range pod.Spec.Containers {
				if c.Resources.Requests != nil {
					pa.cpuRequest += c.Resources.Requests.Cpu().MilliValue()
					pa.memRequest += c.Resources.Requests.Memory().Value()
				}
				if c.Resources.Limits != nil {
					pa.cpuLimit += c.Resources.Limits.Cpu().MilliValue()
					pa.memLimit += c.Resources.Limits.Memory().Value()
				}
			}

			// Check for missing limits/requests
			pa.missingLimits = pa.cpuLimit == 0 || pa.memLimit == 0
			pa.missingReqs = pa.cpuRequest == 0 || pa.memRequest == 0

			// Get actual usage
			if m, ok := metricsMap[pod.Name]; ok {
				pa.hasMetrics = true
				pa.cpuUsage = m.cpuMillis
				pa.memUsage = m.memBytes
			}

			// Calculate percentages
			if pa.cpuLimit > 0 && pa.hasMetrics {
				pa.cpuPctOfLimit = float64(pa.cpuUsage) / float64(pa.cpuLimit) * 100
			}
			if pa.memLimit > 0 && pa.hasMetrics {
				pa.memPctOfLimit = float64(pa.memUsage) / float64(pa.memLimit) * 100
			}
			if pa.cpuRequest > 0 && pa.hasMetrics {
				pa.cpuPctOfReq = float64(pa.cpuUsage) / float64(pa.cpuRequest) * 100
			}
			if pa.memRequest > 0 && pa.hasMetrics {
				pa.memPctOfReq = float64(pa.memUsage) / float64(pa.memRequest) * 100
			}

			// Categorize
			if pa.missingLimits && pa.missingReqs {
				pa.category = "MISSING LIMITS"
				missingLimitsCount++
			} else if pa.hasMetrics && (pa.cpuPctOfLimit > 90 || pa.memPctOfLimit > 90) {
				pa.category = "CRITICAL"
				criticalCount++
			} else if pa.hasMetrics && (pa.cpuPctOfLimit > 70 || pa.memPctOfLimit > 70) {
				pa.category = "WARNING"
				warningCount++
			} else if pa.hasMetrics && pa.cpuRequest > 0 && pa.memRequest > 0 && pa.cpuPctOfReq < 30 && pa.memPctOfReq < 30 {
				pa.category = "OVERPROVISIONED"
				overprovisionedCount++
			} else if pa.missingLimits {
				pa.category = "MISSING LIMITS"
				missingLimitsCount++
			} else {
				pa.category = "OK"
			}

			nsTotalCPUReq += pa.cpuRequest
			nsTotalCPULim += pa.cpuLimit
			nsTotalMemReq += pa.memRequest
			nsTotalMemLim += pa.memLimit
			nsTotalCPUUsage += pa.cpuUsage
			nsTotalMemUsage += pa.memUsage

			analyses = append(analyses, pa)
		}

		// Summary
		sb.WriteString(util.FormatSubHeader("Summary"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  Active Pods Analyzed: %d\n", len(analyses)))
		sb.WriteString(fmt.Sprintf("  CRITICAL (>90%% of limit): %d\n", criticalCount))
		sb.WriteString(fmt.Sprintf("  WARNING (>70%% of limit):  %d\n", warningCount))
		sb.WriteString(fmt.Sprintf("  OVERPROVISIONED (<30%% of request): %d\n", overprovisionedCount))
		sb.WriteString(fmt.Sprintf("  MISSING LIMITS/REQUESTS: %d\n", missingLimitsCount))

		// Namespace totals
		sb.WriteString(fmt.Sprintf("\n  Namespace Totals:\n"))
		sb.WriteString(fmt.Sprintf("    CPU Requests: %dm, Limits: %dm, Usage: %dm\n", nsTotalCPUReq, nsTotalCPULim, nsTotalCPUUsage))
		sb.WriteString(fmt.Sprintf("    Memory Requests: %s, Limits: %s, Usage: %s\n",
			formatBytes(nsTotalMemReq), formatBytes(nsTotalMemLim), formatBytes(nsTotalMemUsage)))

		// Pod detail table
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Pod Resource Details"))
		sb.WriteString("\n")

		headers := []string{"POD", "CATEGORY", "CPU USE/REQ/LIM", "CPU%LIM", "MEM USE/REQ/LIM", "MEM%LIM"}
		rows := make([][]string, 0, len(analyses))
		for _, pa := range analyses {
			cpuStr := fmt.Sprintf("%dm/%dm/%dm", pa.cpuUsage, pa.cpuRequest, pa.cpuLimit)
			memStr := fmt.Sprintf("%s/%s/%s", formatBytes(pa.memUsage), formatBytes(pa.memRequest), formatBytes(pa.memLimit))
			cpuPctStr := "N/A"
			memPctStr := "N/A"
			if pa.hasMetrics && pa.cpuLimit > 0 {
				cpuPctStr = fmt.Sprintf("%.1f%%", pa.cpuPctOfLimit)
			}
			if pa.hasMetrics && pa.memLimit > 0 {
				memPctStr = fmt.Sprintf("%.1f%%", pa.memPctOfLimit)
			}
			rows = append(rows, []string{
				truncateName(pa.name, 40),
				pa.category,
				cpuStr,
				cpuPctStr,
				memStr,
				memPctStr,
			})
		}
		sb.WriteString(util.FormatTable(headers, rows))

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		findingsCount := 0

		for _, pa := range analyses {
			switch pa.category {
			case "CRITICAL":
				if pa.cpuPctOfLimit > 90 {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Pod '%s' CPU usage at %.1f%% of limit (%dm/%dm)", pa.name, pa.cpuPctOfLimit, pa.cpuUsage, pa.cpuLimit)))
					sb.WriteString("\n")
					findingsCount++
				}
				if pa.memPctOfLimit > 90 {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Pod '%s' memory usage at %.1f%% of limit (%s/%s) - OOM risk", pa.name, pa.memPctOfLimit, formatBytes(pa.memUsage), formatBytes(pa.memLimit))))
					sb.WriteString("\n")
					findingsCount++
				}
			case "WARNING":
				if pa.cpuPctOfLimit > 70 {
					sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Pod '%s' CPU usage at %.1f%% of limit", pa.name, pa.cpuPctOfLimit)))
					sb.WriteString("\n")
					findingsCount++
				}
				if pa.memPctOfLimit > 70 {
					sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Pod '%s' memory usage at %.1f%% of limit", pa.name, pa.memPctOfLimit)))
					sb.WriteString("\n")
					findingsCount++
				}
			case "OVERPROVISIONED":
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Pod '%s' is overprovisioned: CPU %.1f%% of request, memory %.1f%% of request — consider reducing requests", pa.name, pa.cpuPctOfReq, pa.memPctOfReq)))
				sb.WriteString("\n")
				findingsCount++
			case "MISSING LIMITS":
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Pod '%s' is missing resource limits/requests", pa.name)))
				sb.WriteString("\n")
				findingsCount++
			}
		}

		if findingsCount == 0 {
			sb.WriteString("  No issues found - all pods within healthy thresholds.\n")
		}

		// Mermaid xychart: top pods by CPU usage % of limit
		if metricsAvailable {
			// Sort by CPU % of limit descending, take top 10
			type chartEntry struct {
				name       string
				cpuPctLim  float64
			}
			var chartData []chartEntry
			for _, pa := range analyses {
				if pa.cpuLimit > 0 && pa.hasMetrics {
					chartData = append(chartData, chartEntry{name: truncateName(pa.name, 15), cpuPctLim: pa.cpuPctOfLimit})
				}
			}
			sort.Slice(chartData, func(i, j int) bool { return chartData[i].cpuPctLim > chartData[j].cpuPctLim })

			chartLimit := 10
			if len(chartData) < chartLimit {
				chartLimit = len(chartData)
			}
			chartData = chartData[:chartLimit]

			if len(chartData) > 0 {
				xLabels := make([]string, len(chartData))
				barVals := make([]float64, len(chartData))
				lineVals := make([]float64, len(chartData))
				for i, cd := range chartData {
					xLabels[i] = cd.name
					barVals[i] = cd.cpuPctLim
					lineVals[i] = 80.0 // threshold line at 80%
				}

				chart := mermaid.NewXYChart("Top Pods by CPU Usage % of Limit").
					SetXAxis(xLabels).
					SetYAxis("CPU Usage % of Limit", 0, 120).
					AddBar(barVals).
					AddLine(lineVals)

				sb.WriteString("\nRESOURCE USAGE CHART:\n")
				sb.WriteString(chart.RenderBlock())
				sb.WriteString("\n")
			}
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// -------------------------------------------------------------------------
	// 2. analyze_node_capacity
	// -------------------------------------------------------------------------
	mcp.AddTool(server, &mcp.Tool{
		Name: "analyze_node_capacity",
		Description: "Analyze capacity, allocatable resources, actual usage (from metrics), and pod request sums for every node. " +
			"Calculates allocatable utilization, actual utilization, and scheduling headroom. " +
			"Checks node conditions. Includes a Mermaid xychart of per-node CPU utilization. " +
			"Requires metrics-server for actual usage data.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeNodeCapacityInput) (*mcp.CallToolResult, any, error) {
		nodes, err := client.ListNodes(ctx, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing nodes", err), nil, nil
		}

		// Get node metrics
		nodeMetrics, err := client.GetNodeMetrics(ctx)
		metricsAvailable := err == nil && len(nodeMetrics) > 0

		// Build metrics lookup
		type nodeMetricsData struct {
			cpuMillis int64
			memBytes  int64
		}
		metricsMap := make(map[string]nodeMetricsData)
		if metricsAvailable {
			for _, nm := range nodeMetrics {
				metricsMap[nm.Name] = nodeMetricsData{
					cpuMillis: nm.Usage.Cpu().MilliValue(),
					memBytes:  nm.Usage.Memory().Value(),
				}
			}
		}

		// Get all pods to sum requests per node
		allPods, err := client.ListPods(ctx, "", metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		type nodeRequestSums struct {
			cpuMillis int64
			memBytes  int64
			podCount  int
		}
		requestsByNode := make(map[string]*nodeRequestSums)
		for _, pod := range allPods {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			nodeName := pod.Spec.NodeName
			if nodeName == "" {
				continue
			}
			if requestsByNode[nodeName] == nil {
				requestsByNode[nodeName] = &nodeRequestSums{}
			}
			for _, c := range pod.Spec.Containers {
				if c.Resources.Requests != nil {
					requestsByNode[nodeName].cpuMillis += c.Resources.Requests.Cpu().MilliValue()
					requestsByNode[nodeName].memBytes += c.Resources.Requests.Memory().Value()
				}
			}
			requestsByNode[nodeName].podCount++
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Node Capacity Analysis"))
		sb.WriteString("\n\n")

		if !metricsAvailable {
			sb.WriteString(util.FormatFinding("WARNING", "Metrics server not available. Actual usage data will be unavailable."))
			sb.WriteString("\n\n")
		}

		// Per-node analysis
		type nodeAnalysis struct {
			name              string
			cpuCapacity       int64
			cpuAllocatable    int64
			cpuUsage          int64
			cpuRequests       int64
			memCapacity       int64
			memAllocatable    int64
			memUsage          int64
			memRequests       int64
			podCount          int
			podCapacity       int64
			allocUtilCPU      float64 // actual usage / allocatable
			allocUtilMem      float64
			requestUtilCPU    float64 // requests / allocatable
			requestUtilMem    float64
			headroomCPU       int64 // allocatable - requests
			headroomMem       int64
			hasMetrics        bool
			conditionIssues   []string
		}

		nodeAnalyses := make([]nodeAnalysis, 0, len(nodes))

		for _, node := range nodes {
			na := nodeAnalysis{
				name:           node.Name,
				cpuCapacity:    node.Status.Capacity.Cpu().MilliValue(),
				cpuAllocatable: node.Status.Allocatable.Cpu().MilliValue(),
				memCapacity:    node.Status.Capacity.Memory().Value(),
				memAllocatable: node.Status.Allocatable.Memory().Value(),
			}

			if podCap, ok := node.Status.Allocatable[corev1.ResourcePods]; ok {
				na.podCapacity = podCap.Value()
			}

			// Metrics
			if m, ok := metricsMap[node.Name]; ok {
				na.hasMetrics = true
				na.cpuUsage = m.cpuMillis
				na.memUsage = m.memBytes
			}

			// Pod requests
			if rs, ok := requestsByNode[node.Name]; ok {
				na.cpuRequests = rs.cpuMillis
				na.memRequests = rs.memBytes
				na.podCount = rs.podCount
			}

			// Calculate utilization percentages
			if na.cpuAllocatable > 0 {
				if na.hasMetrics {
					na.allocUtilCPU = float64(na.cpuUsage) / float64(na.cpuAllocatable) * 100
				}
				na.requestUtilCPU = float64(na.cpuRequests) / float64(na.cpuAllocatable) * 100
				na.headroomCPU = na.cpuAllocatable - na.cpuRequests
			}
			if na.memAllocatable > 0 {
				if na.hasMetrics {
					na.allocUtilMem = float64(na.memUsage) / float64(na.memAllocatable) * 100
				}
				na.requestUtilMem = float64(na.memRequests) / float64(na.memAllocatable) * 100
				na.headroomMem = na.memAllocatable - na.memRequests
			}

			// Check node conditions
			for _, cond := range node.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
					na.conditionIssues = append(na.conditionIssues, "NotReady")
				}
				if cond.Type == corev1.NodeMemoryPressure && cond.Status == corev1.ConditionTrue {
					na.conditionIssues = append(na.conditionIssues, "MemoryPressure")
				}
				if cond.Type == corev1.NodeDiskPressure && cond.Status == corev1.ConditionTrue {
					na.conditionIssues = append(na.conditionIssues, "DiskPressure")
				}
				if cond.Type == corev1.NodePIDPressure && cond.Status == corev1.ConditionTrue {
					na.conditionIssues = append(na.conditionIssues, "PIDPressure")
				}
			}

			nodeAnalyses = append(nodeAnalyses, na)
		}

		// Node table
		headers := []string{"NODE", "PODS", "CPU ALLOC", "CPU REQ", "CPU REQ%", "CPU USE", "CPU USE%", "MEM ALLOC", "MEM REQ%", "MEM USE%", "CONDITIONS"}
		tableRows := make([][]string, 0, len(nodeAnalyses))
		for _, na := range nodeAnalyses {
			cpuUsePct := "N/A"
			memUsePct := "N/A"
			if na.hasMetrics {
				cpuUsePct = fmt.Sprintf("%.1f%%", na.allocUtilCPU)
				memUsePct = fmt.Sprintf("%.1f%%", na.allocUtilMem)
			}
			condStr := "OK"
			if len(na.conditionIssues) > 0 {
				condStr = strings.Join(na.conditionIssues, ",")
			}
			tableRows = append(tableRows, []string{
				na.name,
				fmt.Sprintf("%d/%d", na.podCount, na.podCapacity),
				fmt.Sprintf("%dm", na.cpuAllocatable),
				fmt.Sprintf("%dm", na.cpuRequests),
				fmt.Sprintf("%.1f%%", na.requestUtilCPU),
				fmt.Sprintf("%dm", na.cpuUsage),
				cpuUsePct,
				formatBytes(na.memAllocatable),
				fmt.Sprintf("%.1f%%", na.requestUtilMem),
				memUsePct,
				condStr,
			})
		}
		sb.WriteString(util.FormatTable(headers, tableRows))

		// Scheduling headroom
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Scheduling Headroom"))
		sb.WriteString("\n")
		headroomHeaders := []string{"NODE", "CPU HEADROOM", "MEMORY HEADROOM"}
		headroomRows := make([][]string, 0, len(nodeAnalyses))
		for _, na := range nodeAnalyses {
			headroomRows = append(headroomRows, []string{
				na.name,
				fmt.Sprintf("%dm", na.headroomCPU),
				formatBytes(na.headroomMem),
			})
		}
		sb.WriteString(util.FormatTable(headroomHeaders, headroomRows))

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		findingsCount := 0
		for _, na := range nodeAnalyses {
			for _, issue := range na.conditionIssues {
				if issue == "NotReady" {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' is NotReady", na.name)))
					sb.WriteString("\n")
					findingsCount++
				} else {
					sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' has %s", na.name, issue)))
					sb.WriteString("\n")
					findingsCount++
				}
			}
			if na.requestUtilCPU > 90 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' CPU requests at %.1f%% of allocatable — scheduling may fail", na.name, na.requestUtilCPU)))
				sb.WriteString("\n")
				findingsCount++
			} else if na.requestUtilCPU > 80 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' CPU requests at %.1f%% of allocatable", na.name, na.requestUtilCPU)))
				sb.WriteString("\n")
				findingsCount++
			}
			if na.requestUtilMem > 90 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' memory requests at %.1f%% of allocatable — scheduling may fail", na.name, na.requestUtilMem)))
				sb.WriteString("\n")
				findingsCount++
			} else if na.requestUtilMem > 80 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' memory requests at %.1f%% of allocatable", na.name, na.requestUtilMem)))
				sb.WriteString("\n")
				findingsCount++
			}
			if na.hasMetrics && na.allocUtilCPU > 90 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' actual CPU utilization at %.1f%%", na.name, na.allocUtilCPU)))
				sb.WriteString("\n")
				findingsCount++
			}
			if na.hasMetrics && na.allocUtilMem > 90 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' actual memory utilization at %.1f%%", na.name, na.allocUtilMem)))
				sb.WriteString("\n")
				findingsCount++
			}
			if na.headroomCPU < 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' is overcommitted on CPU by %dm", na.name, -na.headroomCPU)))
				sb.WriteString("\n")
				findingsCount++
			}
			if na.headroomMem < 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' is overcommitted on memory by %s", na.name, formatBytes(-na.headroomMem))))
				sb.WriteString("\n")
				findingsCount++
			}
		}
		if findingsCount == 0 {
			sb.WriteString("  All nodes within healthy thresholds.\n")
		}

		// Mermaid xychart: per-node CPU utilization
		if len(nodeAnalyses) > 0 {
			xLabels := make([]string, len(nodeAnalyses))
			barVals := make([]float64, len(nodeAnalyses))
			for i, na := range nodeAnalyses {
				xLabels[i] = truncateName(na.name, 15)
				if na.hasMetrics {
					barVals[i] = na.allocUtilCPU
				} else {
					barVals[i] = na.requestUtilCPU // fallback to request utilization
				}
			}

			yTitle := "CPU Utilization %"
			if !metricsAvailable {
				yTitle = "CPU Request Utilization %"
			}

			chart := mermaid.NewXYChart("Per-Node CPU Utilization").
				SetXAxis(xLabels).
				SetYAxis(yTitle, 0, 120).
				AddBar(barVals)

			sb.WriteString("\nNODE CPU UTILIZATION CHART:\n")
			sb.WriteString(chart.RenderBlock())
			sb.WriteString("\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// -------------------------------------------------------------------------
	// 3. analyze_resource_efficiency
	// -------------------------------------------------------------------------
	mcp.AddTool(server, &mcp.Tool{
		Name: "analyze_resource_efficiency",
		Description: "Analyze resource efficiency cluster-wide or per namespace. Calculates waste (requests - actual usage), " +
			"bin packing efficiency per node, identifies right-sizing opportunities, and flags pods with no requests/limits. " +
			"Requires metrics-server for waste calculations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeResourceEfficiencyInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		scope := displayNS(input.Namespace)

		pods, err := client.ListPods(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		// Get metrics
		podMetrics, err := client.GetPodMetrics(ctx, ns, metav1.ListOptions{})
		metricsAvailable := err == nil && len(podMetrics) > 0

		// Build metrics lookup
		type metricsData struct {
			cpuMillis int64
			memBytes  int64
		}
		metricsMap := make(map[string]metricsData)
		if metricsAvailable {
			for _, pm := range podMetrics {
				var cpu, mem int64
				for _, c := range pm.Containers {
					cpu += c.Usage.Cpu().MilliValue()
					mem += c.Usage.Memory().Value()
				}
				key := pm.Namespace + "/" + pm.Name
				metricsMap[key] = metricsData{cpuMillis: cpu, memBytes: mem}
			}
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Resource Efficiency Report (scope: %s)", scope)))
		sb.WriteString("\n\n")

		if !metricsAvailable {
			sb.WriteString(util.FormatFinding("WARNING", "Metrics server not available. Waste calculations require metrics data."))
			sb.WriteString("\n\n")
		}

		// Aggregate data
		var totalCPUReq, totalCPUUsage, totalMemReq, totalMemUsage int64
		var totalCPULim, totalMemLim int64
		noRequestsPods := make([]string, 0)
		noLimitsPods := make([]string, 0)

		type podEfficiency struct {
			name         string
			namespace    string
			cpuRequest   int64
			cpuUsage     int64
			cpuWaste     int64
			memRequest   int64
			memUsage     int64
			memWaste     int64
			hasMetrics   bool
			hasRequests  bool
			hasLimits    bool
		}

		podEfficiencies := make([]podEfficiency, 0, len(pods))

		for _, pod := range pods {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}

			pe := podEfficiency{
				name:      pod.Name,
				namespace: pod.Namespace,
			}

			var podCPUReq, podCPULim, podMemReq, podMemLim int64
			for _, c := range pod.Spec.Containers {
				if c.Resources.Requests != nil {
					podCPUReq += c.Resources.Requests.Cpu().MilliValue()
					podMemReq += c.Resources.Requests.Memory().Value()
				}
				if c.Resources.Limits != nil {
					podCPULim += c.Resources.Limits.Cpu().MilliValue()
					podMemLim += c.Resources.Limits.Memory().Value()
				}
			}

			pe.cpuRequest = podCPUReq
			pe.memRequest = podMemReq
			pe.hasRequests = podCPUReq > 0 || podMemReq > 0
			pe.hasLimits = podCPULim > 0 || podMemLim > 0

			if !pe.hasRequests {
				noRequestsPods = append(noRequestsPods, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
			}
			if !pe.hasLimits {
				noLimitsPods = append(noLimitsPods, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
			}

			key := pod.Namespace + "/" + pod.Name
			if m, ok := metricsMap[key]; ok {
				pe.hasMetrics = true
				pe.cpuUsage = m.cpuMillis
				pe.memUsage = m.memBytes
				pe.cpuWaste = podCPUReq - m.cpuMillis
				pe.memWaste = podMemReq - m.memBytes
				if pe.cpuWaste < 0 {
					pe.cpuWaste = 0
				}
				if pe.memWaste < 0 {
					pe.memWaste = 0
				}
			}

			totalCPUReq += podCPUReq
			totalCPULim += podCPULim
			totalMemReq += podMemReq
			totalMemLim += podMemLim
			totalCPUUsage += pe.cpuUsage
			totalMemUsage += pe.memUsage

			podEfficiencies = append(podEfficiencies, pe)
		}

		// Waste summary
		sb.WriteString(util.FormatSubHeader("Waste Analysis"))
		sb.WriteString("\n")
		if metricsAvailable {
			cpuWaste := totalCPUReq - totalCPUUsage
			memWaste := totalMemReq - totalMemUsage
			if cpuWaste < 0 {
				cpuWaste = 0
			}
			if memWaste < 0 {
				memWaste = 0
			}

			cpuEfficiency := float64(0)
			if totalCPUReq > 0 {
				cpuEfficiency = float64(totalCPUUsage) / float64(totalCPUReq) * 100
			}
			memEfficiency := float64(0)
			if totalMemReq > 0 {
				memEfficiency = float64(totalMemUsage) / float64(totalMemReq) * 100
			}

			sb.WriteString(fmt.Sprintf("  Total CPU Requested:  %dm\n", totalCPUReq))
			sb.WriteString(fmt.Sprintf("  Total CPU Used:       %dm\n", totalCPUUsage))
			sb.WriteString(fmt.Sprintf("  Total CPU Waste:      %dm (efficiency: %.1f%%)\n", cpuWaste, cpuEfficiency))
			sb.WriteString(fmt.Sprintf("  Total Memory Requested:  %s\n", formatBytes(totalMemReq)))
			sb.WriteString(fmt.Sprintf("  Total Memory Used:       %s\n", formatBytes(totalMemUsage)))
			sb.WriteString(fmt.Sprintf("  Total Memory Waste:      %s (efficiency: %.1f%%)\n", formatBytes(memWaste), memEfficiency))
		} else {
			sb.WriteString("  Waste calculations unavailable without metrics-server.\n")
			sb.WriteString(fmt.Sprintf("  Total CPU Requested: %dm, Limits: %dm\n", totalCPUReq, totalCPULim))
			sb.WriteString(fmt.Sprintf("  Total Memory Requested: %s, Limits: %s\n", formatBytes(totalMemReq), formatBytes(totalMemLim)))
		}

		// Bin packing efficiency per node
		nodes, err := client.ListNodes(ctx, metav1.ListOptions{})
		if err == nil && len(nodes) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Bin Packing Efficiency (per Node)"))
			sb.WriteString("\n")

			// Collect per-node request sums from pods
			type nodeUsage struct {
				cpuRequests    int64
				memRequests    int64
				cpuAllocatable int64
				memAllocatable int64
				podCount       int
			}
			nodeMap := make(map[string]*nodeUsage)
			for _, n := range nodes {
				nodeMap[n.Name] = &nodeUsage{
					cpuAllocatable: n.Status.Allocatable.Cpu().MilliValue(),
					memAllocatable: n.Status.Allocatable.Memory().Value(),
				}
			}
			for _, pod := range pods {
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
					continue
				}
				if pod.Spec.NodeName == "" {
					continue
				}
				nu, ok := nodeMap[pod.Spec.NodeName]
				if !ok {
					continue
				}
				for _, c := range pod.Spec.Containers {
					if c.Resources.Requests != nil {
						nu.cpuRequests += c.Resources.Requests.Cpu().MilliValue()
						nu.memRequests += c.Resources.Requests.Memory().Value()
					}
				}
				nu.podCount++
			}

			binHeaders := []string{"NODE", "PODS", "CPU PACKING", "MEMORY PACKING"}
			binRows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				nu := nodeMap[n.Name]
				cpuPacking := float64(0)
				memPacking := float64(0)
				if nu.cpuAllocatable > 0 {
					cpuPacking = float64(nu.cpuRequests) / float64(nu.cpuAllocatable) * 100
				}
				if nu.memAllocatable > 0 {
					memPacking = float64(nu.memRequests) / float64(nu.memAllocatable) * 100
				}
				binRows = append(binRows, []string{
					n.Name,
					fmt.Sprintf("%d", nu.podCount),
					fmt.Sprintf("%.1f%%", cpuPacking),
					fmt.Sprintf("%.1f%%", memPacking),
				})
			}
			sb.WriteString(util.FormatTable(binHeaders, binRows))
		}

		// Right-sizing opportunities: pods where usage < 30% of requests
		if metricsAvailable {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Right-Sizing Opportunities"))
			sb.WriteString("\n")

			type rightSizeCandidate struct {
				name       string
				namespace  string
				cpuReq     int64
				cpuUse     int64
				cpuPct     float64
				memReq     int64
				memUse     int64
				memPct     float64
			}
			var candidates []rightSizeCandidate
			for _, pe := range podEfficiencies {
				if !pe.hasMetrics || !pe.hasRequests {
					continue
				}
				cpuPct := float64(0)
				if pe.cpuRequest > 0 {
					cpuPct = float64(pe.cpuUsage) / float64(pe.cpuRequest) * 100
				}
				memPct := float64(0)
				if pe.memRequest > 0 {
					memPct = float64(pe.memUsage) / float64(pe.memRequest) * 100
				}
				if (pe.cpuRequest > 0 && cpuPct < 30) || (pe.memRequest > 0 && memPct < 30) {
					candidates = append(candidates, rightSizeCandidate{
						name: pe.name, namespace: pe.namespace,
						cpuReq: pe.cpuRequest, cpuUse: pe.cpuUsage, cpuPct: cpuPct,
						memReq: pe.memRequest, memUse: pe.memUsage, memPct: memPct,
					})
				}
			}

			if len(candidates) == 0 {
				sb.WriteString("  No pods with usage below 30% of requests found.\n")
			} else {
				rsHeaders := []string{"POD", "NAMESPACE", "CPU USE/REQ", "CPU%", "MEM USE/REQ", "MEM%"}
				rsRows := make([][]string, 0, len(candidates))
				for _, c := range candidates {
					rsRows = append(rsRows, []string{
						truncateName(c.name, 35),
						c.namespace,
						fmt.Sprintf("%dm/%dm", c.cpuUse, c.cpuReq),
						fmt.Sprintf("%.1f%%", c.cpuPct),
						fmt.Sprintf("%s/%s", formatBytes(c.memUse), formatBytes(c.memReq)),
						fmt.Sprintf("%.1f%%", c.memPct),
					})
				}
				sb.WriteString(util.FormatTable(rsHeaders, rsRows))
			}
		}

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		findingsCount := 0

		if len(noRequestsPods) > 0 {
			sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d pods have no resource requests set", len(noRequestsPods))))
			sb.WriteString("\n")
			for _, p := range noRequestsPods {
				sb.WriteString(fmt.Sprintf("  - %s\n", p))
			}
			findingsCount++
		}
		if len(noLimitsPods) > 0 {
			sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d pods have no resource limits set", len(noLimitsPods))))
			sb.WriteString("\n")
			for _, p := range noLimitsPods {
				sb.WriteString(fmt.Sprintf("  - %s\n", p))
			}
			findingsCount++
		}

		if metricsAvailable && totalCPUReq > 0 {
			cpuEff := float64(totalCPUUsage) / float64(totalCPUReq) * 100
			if cpuEff < 30 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Overall CPU efficiency is only %.1f%% — significant overprovisioning", cpuEff)))
				sb.WriteString("\n")
				findingsCount++
			} else if cpuEff < 50 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("CPU efficiency at %.1f%% — consider right-sizing workloads", cpuEff)))
				sb.WriteString("\n")
				findingsCount++
			}
		}
		if metricsAvailable && totalMemReq > 0 {
			memEff := float64(totalMemUsage) / float64(totalMemReq) * 100
			if memEff < 30 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Overall memory efficiency is only %.1f%% — significant overprovisioning", memEff)))
				sb.WriteString("\n")
				findingsCount++
			} else if memEff < 50 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Memory efficiency at %.1f%% — consider right-sizing workloads", memEff)))
				sb.WriteString("\n")
				findingsCount++
			}
		}

		if findingsCount == 0 {
			sb.WriteString("  Resource efficiency appears healthy.\n")
		}

		// Recommendations
		sb.WriteString("\nRECOMMENDATIONS:\n")
		recNum := 1
		if len(noRequestsPods) > 0 {
			sb.WriteString(fmt.Sprintf("%d. Set resource requests on all %d pods without them to improve scheduling reliability.\n", recNum, len(noRequestsPods)))
			recNum++
		}
		if len(noLimitsPods) > 0 {
			sb.WriteString(fmt.Sprintf("%d. Set resource limits on all %d pods without them to prevent resource contention.\n", recNum, len(noLimitsPods)))
			recNum++
		}
		if metricsAvailable && totalCPUReq > 0 {
			cpuEff := float64(totalCPUUsage) / float64(totalCPUReq) * 100
			if cpuEff < 50 {
				sb.WriteString(fmt.Sprintf("%d. Reduce CPU requests for overprovisioned pods to reclaim %dm of wasted CPU.\n", recNum, totalCPUReq-totalCPUUsage))
				recNum++
			}
		}
		if metricsAvailable && totalMemReq > 0 {
			memEff := float64(totalMemUsage) / float64(totalMemReq) * 100
			if memEff < 50 {
				sb.WriteString(fmt.Sprintf("%d. Reduce memory requests for overprovisioned pods to reclaim %s of wasted memory.\n", recNum, formatBytes(totalMemReq-totalMemUsage)))
				recNum++
			}
		}
		if recNum == 1 {
			sb.WriteString("  No specific recommendations — resource configuration looks good.\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// -------------------------------------------------------------------------
	// 4. analyze_network_policies
	// -------------------------------------------------------------------------
	mcp.AddTool(server, &mcp.Tool{
		Name: "analyze_network_policies",
		Description: "Analyze network policies in a namespace. Parses selectors, ingress/egress rules, builds an allow/deny matrix, " +
			"flags pods with no matching policy, and generates a Mermaid flowchart showing allowed flows (solid arrows) " +
			"and denied flows (dotted red arrows).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeNetworkPoliciesInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace

		policies, err := client.ListNetworkPolicies(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing network policies", err), nil, nil
		}

		pods, err := client.ListPods(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Network Policy Analysis (namespace: %s)", ns)))
		sb.WriteString("\n\n")

		if len(policies) == 0 {
			sb.WriteString(util.FormatFinding("WARNING", "No network policies found in this namespace — all traffic is allowed by default"))
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("  %d pods are running without any network policy protection.\n", len(pods)))

			// Simple diagram for no-policy case
			sb.WriteString("\nNETWORK FLOW DIAGRAM:\n")
			fc := mermaid.NewFlowchart(mermaid.DirectionLR)
			fc.AddNode("ANY_SRC", "Any Source", mermaid.ShapeStadium)
			fc.AddNode("NS", fmt.Sprintf("All Pods in %s", ns), mermaid.ShapeRect)
			fc.AddNode("ANY_DST", "Any Destination", mermaid.ShapeStadium)
			fc.AddEdge("ANY_SRC", "NS", "allowed", mermaid.EdgeSolid)
			fc.AddEdge("NS", "ANY_DST", "allowed", mermaid.EdgeSolid)
			sb.WriteString(fc.RenderBlock())
			sb.WriteString("\n")

			return util.SuccessResult(sb.String()), nil, nil
		}

		// Policy summary table
		sb.WriteString(util.FormatSubHeader("Policy Summary"))
		sb.WriteString("\n")
		pHeaders := []string{"POLICY", "POD SELECTOR", "INGRESS RULES", "EGRESS RULES", "TYPES"}
		pRows := make([][]string, 0, len(policies))
		for _, np := range policies {
			policyTypes := make([]string, 0, len(np.Spec.PolicyTypes))
			for _, pt := range np.Spec.PolicyTypes {
				policyTypes = append(policyTypes, string(pt))
			}
			if len(policyTypes) == 0 {
				policyTypes = []string{"Ingress"}
			}
			pRows = append(pRows, []string{
				np.Name,
				formatLabelSelector(&np.Spec.PodSelector),
				fmt.Sprintf("%d", len(np.Spec.Ingress)),
				fmt.Sprintf("%d", len(np.Spec.Egress)),
				strings.Join(policyTypes, ","),
			})
		}
		sb.WriteString(util.FormatTable(pHeaders, pRows))

		// Determine which pods are covered by at least one policy
		coveredPods := make(map[string][]string)   // podName -> list of policy names
		uncoveredPods := make([]string, 0)

		for i := range pods {
			pod := &pods[i]
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			matched := false
			for _, np := range policies {
				selector, sErr := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
				if sErr != nil {
					continue
				}
				if selector.Matches(labels.Set(pod.Labels)) {
					coveredPods[pod.Name] = append(coveredPods[pod.Name], np.Name)
					matched = true
				}
			}
			if !matched {
				uncoveredPods = append(uncoveredPods, pod.Name)
			}
		}

		// Coverage report
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Pod Coverage"))
		sb.WriteString("\n")
		activePods := 0
		for _, pod := range pods {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				activePods++
			}
		}
		sb.WriteString(fmt.Sprintf("  Active Pods: %d\n", activePods))
		sb.WriteString(fmt.Sprintf("  Covered by Policy: %d\n", len(coveredPods)))
		sb.WriteString(fmt.Sprintf("  No Policy Match: %d\n", len(uncoveredPods)))

		// Allow/Deny matrix per policy
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Allow/Deny Matrix"))
		sb.WriteString("\n")
		for _, np := range policies {
			sb.WriteString(fmt.Sprintf("\n  Policy: %s\n", np.Name))
			sb.WriteString(fmt.Sprintf("    Selects: %s\n", formatLabelSelector(&np.Spec.PodSelector)))

			hasIngress := false
			hasEgress := false
			for _, pt := range np.Spec.PolicyTypes {
				if pt == networkingv1.PolicyTypeIngress {
					hasIngress = true
				}
				if pt == networkingv1.PolicyTypeEgress {
					hasEgress = true
				}
			}
			if len(np.Spec.PolicyTypes) == 0 {
				hasIngress = true
			}

			if hasIngress {
				if len(np.Spec.Ingress) == 0 {
					sb.WriteString("    Ingress: DENY ALL\n")
				} else {
					for i, rule := range np.Spec.Ingress {
						parts := describeIngressRule(rule)
						sb.WriteString(fmt.Sprintf("    Ingress Rule %d: ALLOW %s\n", i+1, strings.Join(parts, "; ")))
					}
				}
			}
			if hasEgress {
				if len(np.Spec.Egress) == 0 {
					sb.WriteString("    Egress: DENY ALL\n")
				} else {
					for i, rule := range np.Spec.Egress {
						parts := describeEgressRule(rule)
						sb.WriteString(fmt.Sprintf("    Egress Rule %d: ALLOW %s\n", i+1, strings.Join(parts, "; ")))
					}
				}
			}
		}

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		findingsCount := 0

		if len(uncoveredPods) > 0 {
			sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d pods have no matching network policy — all traffic allowed by default", len(uncoveredPods))))
			sb.WriteString("\n")
			for _, p := range uncoveredPods {
				sb.WriteString(fmt.Sprintf("  - %s\n", p))
			}
			findingsCount++
		}

		for _, np := range policies {
			hasIngress := false
			hasEgress := false
			for _, pt := range np.Spec.PolicyTypes {
				if pt == networkingv1.PolicyTypeIngress {
					hasIngress = true
				}
				if pt == networkingv1.PolicyTypeEgress {
					hasEgress = true
				}
			}
			if len(np.Spec.PolicyTypes) == 0 {
				hasIngress = true
			}

			if hasIngress && len(np.Spec.Ingress) == 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Policy '%s' has Ingress type but no ingress rules — all inbound traffic DENIED to matched pods", np.Name)))
				sb.WriteString("\n")
				findingsCount++
			}
			if hasEgress && len(np.Spec.Egress) == 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Policy '%s' has Egress type but no egress rules — all outbound traffic DENIED from matched pods (including DNS)", np.Name)))
				sb.WriteString("\n")
				findingsCount++
			}

			// Check for overly broad policies (select all pods)
			if len(np.Spec.PodSelector.MatchLabels) == 0 && len(np.Spec.PodSelector.MatchExpressions) == 0 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Policy '%s' selects ALL pods in namespace", np.Name)))
				sb.WriteString("\n")
				findingsCount++
			}
		}

		if findingsCount == 0 {
			sb.WriteString("  Network policies appear well-configured.\n")
		}

		// Mermaid flowchart
		sb.WriteString("\nNETWORK FLOW DIAGRAM:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionLR)

		// Add subgraph for allowed traffic
		fc.AddSubgraph("allowed_traffic", "Allowed Traffic", func(sg *mermaid.Subgraph) {
			edgeIdx := 0
			for _, np := range policies {
				policyID := mermaid.SafeID("policy_" + np.Name)

				// Determine selected pods
				selector, sErr := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
				if sErr != nil {
					continue
				}
				targetLabel := formatLabelSelector(&np.Spec.PodSelector)
				targetID := mermaid.SafeID("target_" + np.Name)
				sg.AddNode(targetID, fmt.Sprintf("Pods: %s", targetLabel), mermaid.ShapeRect)

				// Ingress allowed sources
				for _, rule := range np.Spec.Ingress {
					sources := describeIngressSources(rule)
					for _, src := range sources {
						srcID := mermaid.SafeID(fmt.Sprintf("src_%s_%d", policyID, edgeIdx))
						sg.AddNode(srcID, src, mermaid.ShapeStadium)
						portLabel := ""
						if len(rule.Ports) > 0 {
							portParts := make([]string, 0, len(rule.Ports))
							for _, p := range rule.Ports {
								if p.Port != nil {
									portParts = append(portParts, p.Port.String())
								}
							}
							if len(portParts) > 0 {
								portLabel = strings.Join(portParts, ",")
							}
						}
						sg.AddEdge(srcID, targetID, portLabel, mermaid.EdgeSolid)
						edgeIdx++
					}
				}

				// Egress allowed destinations
				for _, rule := range np.Spec.Egress {
					dests := describeEgressDests(rule)
					for _, dst := range dests {
						dstID := mermaid.SafeID(fmt.Sprintf("dst_%s_%d", policyID, edgeIdx))
						sg.AddNode(dstID, dst, mermaid.ShapeStadium)
						portLabel := ""
						if len(rule.Ports) > 0 {
							portParts := make([]string, 0, len(rule.Ports))
							for _, p := range rule.Ports {
								if p.Port != nil {
									portParts = append(portParts, p.Port.String())
								}
							}
							if len(portParts) > 0 {
								portLabel = strings.Join(portParts, ",")
							}
						}
						sg.AddEdge(targetID, dstID, portLabel, mermaid.EdgeSolid)
						edgeIdx++
					}
				}

				_ = selector // used above for matching
			}
		})

		// Add subgraph for denied flows
		hasDeniedFlows := false
		for _, np := range policies {
			hasIngress := false
			hasEgress := false
			for _, pt := range np.Spec.PolicyTypes {
				if pt == networkingv1.PolicyTypeIngress {
					hasIngress = true
				}
				if pt == networkingv1.PolicyTypeEgress {
					hasEgress = true
				}
			}
			if len(np.Spec.PolicyTypes) == 0 {
				hasIngress = true
			}
			if (hasIngress && len(np.Spec.Ingress) == 0) || (hasEgress && len(np.Spec.Egress) == 0) {
				hasDeniedFlows = true
				break
			}
		}

		if hasDeniedFlows || len(uncoveredPods) > 0 {
			fc.AddSubgraph("denied_traffic", "Denied by Policy", func(sg *mermaid.Subgraph) {
				denyIdx := 0
				for _, np := range policies {
					hasIngress := false
					hasEgress := false
					for _, pt := range np.Spec.PolicyTypes {
						if pt == networkingv1.PolicyTypeIngress {
							hasIngress = true
						}
						if pt == networkingv1.PolicyTypeEgress {
							hasEgress = true
						}
					}
					if len(np.Spec.PolicyTypes) == 0 {
						hasIngress = true
					}

					targetLabel := formatLabelSelector(&np.Spec.PodSelector)
					denyTargetID := mermaid.SafeID(fmt.Sprintf("deny_target_%s", np.Name))
					sg.AddNode(denyTargetID, fmt.Sprintf("Pods: %s", targetLabel), mermaid.ShapeRect)

					if hasIngress && len(np.Spec.Ingress) == 0 {
						blockSrcID := mermaid.SafeID(fmt.Sprintf("deny_src_%d", denyIdx))
						sg.AddNode(blockSrcID, "All Inbound", mermaid.ShapeStadium)
						sg.AddEdge(blockSrcID, denyTargetID, "denied", mermaid.EdgeDotted)
						denyIdx++
					}
					if hasEgress && len(np.Spec.Egress) == 0 {
						blockDstID := mermaid.SafeID(fmt.Sprintf("deny_dst_%d", denyIdx))
						sg.AddNode(blockDstID, "All Outbound", mermaid.ShapeStadium)
						sg.AddEdge(denyTargetID, blockDstID, "denied", mermaid.EdgeDotted)
						denyIdx++
					}
				}
			})
		}

		// Style denied nodes as critical
		sb.WriteString(fc.RenderBlock())
		sb.WriteString("\n")

		return util.SuccessResult(sb.String()), nil, nil
	})

	// -------------------------------------------------------------------------
	// 5. check_dns_health
	// -------------------------------------------------------------------------
	mcp.AddTool(server, &mcp.Tool{
		Name: "check_dns_health",
		Description: "Check CoreDNS health in the cluster. Finds CoreDNS pods in kube-system, checks phase, " +
			"restart counts, readiness conditions. Retrieves CoreDNS logs and scans for SERVFAIL, NXDOMAIN, " +
			"and ERROR patterns. Returns a health report with findings.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input checkDNSHealthInput) (*mcp.CallToolResult, any, error) {
		var sb strings.Builder
		sb.WriteString(util.FormatHeader("DNS Health Check"))
		sb.WriteString("\n\n")

		// Find CoreDNS pods
		coreDNSPods, err := client.ListPods(ctx, "kube-system", metav1.ListOptions{
			LabelSelector: "k8s-app=kube-dns",
		})
		if err != nil {
			return util.HandleK8sError("listing CoreDNS pods", err), nil, nil
		}

		// If label selector didn't find them, try alternative labels
		if len(coreDNSPods) == 0 {
			coreDNSPods, err = client.ListPods(ctx, "kube-system", metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=coredns",
			})
			if err != nil {
				return util.HandleK8sError("listing CoreDNS pods", err), nil, nil
			}
		}

		// Last resort: find by name prefix
		if len(coreDNSPods) == 0 {
			allKubeSystemPods, listErr := client.ListPods(ctx, "kube-system", metav1.ListOptions{})
			if listErr == nil {
				for _, pod := range allKubeSystemPods {
					if strings.HasPrefix(pod.Name, "coredns-") {
						coreDNSPods = append(coreDNSPods, pod)
					}
				}
			}
		}

		if len(coreDNSPods) == 0 {
			sb.WriteString(util.FormatFinding("CRITICAL", "No CoreDNS pods found in kube-system namespace"))
			sb.WriteString("\n")
			sb.WriteString("  DNS resolution will not work in the cluster.\n")
			sb.WriteString("  Check if CoreDNS is deployed with: list_pods namespace=kube-system\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		// CoreDNS service check
		coreDNSSvc, svcErr := client.GetService(ctx, "kube-system", "kube-dns")
		if svcErr != nil {
			sb.WriteString(util.FormatFinding("WARNING", "CoreDNS service 'kube-dns' not found in kube-system"))
			sb.WriteString("\n\n")
		} else {
			sb.WriteString(util.FormatKeyValue("Service", fmt.Sprintf("%s (ClusterIP: %s)", coreDNSSvc.Name, coreDNSSvc.Spec.ClusterIP)))
			sb.WriteString("\n")
		}

		// Pod health table
		sb.WriteString(util.FormatSubHeader("CoreDNS Pod Status"))
		sb.WriteString("\n")

		podHeaders := []string{"POD", "STATUS", "READY", "RESTARTS", "AGE", "NODE"}
		podRows := make([][]string, 0, len(coreDNSPods))
		for i := range coreDNSPods {
			p := &coreDNSPods[i]
			ready, total, restarts := podContainerSummary(p)
			podRows = append(podRows, []string{
				p.Name,
				podPhaseReason(p),
				fmt.Sprintf("%d/%d", ready, total),
				fmt.Sprintf("%d", restarts),
				util.FormatAge(p.CreationTimestamp.Time),
				p.Spec.NodeName,
			})
		}
		sb.WriteString(util.FormatTable(podHeaders, podRows))

		// Detailed pod analysis
		sb.WriteString("\nFINDINGS:\n")
		findingsCount := 0

		healthyPods := 0
		for i := range coreDNSPods {
			p := &coreDNSPods[i]

			// Check phase
			if p.Status.Phase != corev1.PodRunning {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("CoreDNS pod '%s' is %s (not Running)", p.Name, string(p.Status.Phase))))
				sb.WriteString("\n")
				findingsCount++
			} else {
				healthyPods++
			}

			// Check restarts
			_, _, restarts := podContainerSummary(p)
			if restarts > util.HighRestartThreshold {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("CoreDNS pod '%s' has %d restarts", p.Name, restarts)))
				sb.WriteString("\n")
				findingsCount++
			} else if restarts > 0 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("CoreDNS pod '%s' has %d restart(s)", p.Name, restarts)))
				sb.WriteString("\n")
				findingsCount++
			}

			// Check readiness
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("CoreDNS pod '%s' is not ready: %s", p.Name, cond.Message)))
					sb.WriteString("\n")
					findingsCount++
				}
			}

			// Check container states for CrashLoop etc.
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("CoreDNS container '%s' in pod '%s' is in CrashLoopBackOff", cs.Name, p.Name)))
					sb.WriteString("\n")
					findingsCount++
				}
				if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("CoreDNS container '%s' in pod '%s' was OOMKilled — increase memory limit", cs.Name, p.Name)))
					sb.WriteString("\n")
					findingsCount++
				}
			}
		}

		// Replica count check
		if len(coreDNSPods) < 2 {
			sb.WriteString(util.FormatFinding("WARNING", "Only 1 CoreDNS pod running — no DNS redundancy. Consider scaling to at least 2 replicas."))
			sb.WriteString("\n")
			findingsCount++
		}

		// Log analysis
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Log Analysis"))
		sb.WriteString("\n")

		type errorPattern struct {
			pattern string
			count   int
		}
		errorPatterns := []errorPattern{
			{pattern: "SERVFAIL"},
			{pattern: "NXDOMAIN"},
			{pattern: "REFUSED"},
			{pattern: "i/o timeout"},
			{pattern: "connection refused"},
			{pattern: "no such host"},
			{pattern: "plugin/errors"},
			{pattern: "ERROR"},
		}

		totalErrors := 0
		for i := range coreDNSPods {
			p := &coreDNSPods[i]

			// Get container name (usually "coredns")
			containerName := ""
			for _, c := range p.Spec.Containers {
				if strings.Contains(c.Name, "coredns") || strings.Contains(c.Name, "dns") {
					containerName = c.Name
					break
				}
			}
			if containerName == "" && len(p.Spec.Containers) > 0 {
				containerName = p.Spec.Containers[0].Name
			}

			logs, logErr := client.GetPodLogs(ctx, "kube-system", p.Name, containerName, 500, false, "1h")
			if logErr != nil {
				sb.WriteString(fmt.Sprintf("  Pod '%s': could not fetch logs: %v\n", p.Name, logErr))
				continue
			}

			if logs == "" {
				sb.WriteString(fmt.Sprintf("  Pod '%s': no logs available\n", p.Name))
				continue
			}

			sb.WriteString(fmt.Sprintf("\n  Pod '%s' log scan (last 1h):\n", p.Name))
			logLines := strings.Split(logs, "\n")
			podErrors := 0

			for idx := range errorPatterns {
				count := 0
				for _, line := range logLines {
					if strings.Contains(line, errorPatterns[idx].pattern) {
						count++
					}
				}
				errorPatterns[idx].count += count
				if count > 0 {
					sb.WriteString(fmt.Sprintf("    %s: %d occurrences\n", errorPatterns[idx].pattern, count))
					podErrors += count
				}
			}

			totalErrors += podErrors
			if podErrors == 0 {
				sb.WriteString("    No error patterns found in logs.\n")
			}
		}

		// Log findings
		for _, ep := range errorPatterns {
			if ep.count > 50 && ep.pattern == "SERVFAIL" {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("High SERVFAIL rate: %d occurrences — DNS resolution is failing", ep.count)))
				sb.WriteString("\n")
				findingsCount++
			} else if ep.count > 10 && ep.pattern == "SERVFAIL" {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Elevated SERVFAIL count: %d — some DNS queries are failing", ep.count)))
				sb.WriteString("\n")
				findingsCount++
			}
			if ep.count > 100 && ep.pattern == "NXDOMAIN" {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("High NXDOMAIN count: %d — check if services have correct DNS names", ep.count)))
				sb.WriteString("\n")
				findingsCount++
			}
			if ep.count > 0 && (ep.pattern == "i/o timeout" || ep.pattern == "connection refused") {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Upstream DNS connectivity issues: %d '%s' errors", ep.count, ep.pattern)))
				sb.WriteString("\n")
				findingsCount++
			}
		}

		// Overall assessment
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall DNS Assessment"))
		sb.WriteString("\n")

		if findingsCount == 0 {
			sb.WriteString(fmt.Sprintf("  CoreDNS is healthy: %d/%d pods running, no error patterns detected.\n", healthyPods, len(coreDNSPods)))
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. %d/%d CoreDNS pods running.\n", findingsCount, healthyPods, len(coreDNSPods)))
			if totalErrors > 0 {
				sb.WriteString(fmt.Sprintf("  Total error patterns in logs: %d\n", totalErrors))
			}
		}

		// Suggested actions
		sb.WriteString("\nSUGGESTED ACTIONS:\n")
		actionNum := 1
		if healthyPods == 0 {
			sb.WriteString(fmt.Sprintf("%d. URGENT: No healthy CoreDNS pods. Check CoreDNS deployment and events in kube-system.\n", actionNum))
			actionNum++
		}
		if len(coreDNSPods) < 2 {
			sb.WriteString(fmt.Sprintf("%d. Scale CoreDNS to at least 2 replicas for high availability.\n", actionNum))
			actionNum++
		}
		for _, ep := range errorPatterns {
			if ep.count > 10 && ep.pattern == "SERVFAIL" {
				sb.WriteString(fmt.Sprintf("%d. Investigate SERVFAIL errors — check upstream DNS configuration in CoreDNS ConfigMap.\n", actionNum))
				actionNum++
				break
			}
		}
		for _, ep := range errorPatterns {
			if ep.count > 0 && (ep.pattern == "i/o timeout" || ep.pattern == "connection refused") {
				sb.WriteString(fmt.Sprintf("%d. Check upstream DNS server connectivity and network policies affecting kube-system.\n", actionNum))
				actionNum++
				break
			}
		}
		if actionNum == 1 {
			sb.WriteString("  No specific actions needed — DNS appears healthy.\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// truncateName shortens a name to maxLen characters, appending ".." if truncated.
func truncateName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	if maxLen < 3 {
		return name[:maxLen]
	}
	return name[:maxLen-2] + ".."
}
