package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type diagnosePodInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Name      string `json:"name" jsonschema:"Pod name"`
}

type diagnoseNamespaceInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace to diagnose"`
}

type diagnoseClusterInput struct{}

type findUnhealthyPodsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace (empty for all namespaces)"`
}

type checkResourceQuotasInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace (empty for all namespaces)"`
}

func registerDiagnosticTools(server *mcp.Server, client *k8s.ClusterClient) {
	// diagnose_pod
	mcp.AddTool(server, &mcp.Tool{
		Name:        "diagnose_pod",
		Description: "Run a comprehensive diagnosis on a specific pod. Checks status, conditions, events, container states, restart reasons, resource limits, and fetches logs from failing containers. Use this when a pod is unhealthy.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input diagnosePodInput) (*mcp.CallToolResult, any, error) {
		pod, err := client.GetPod(ctx, input.Namespace, input.Name)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting pod %s/%s", input.Namespace, input.Name), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Pod Diagnosis: %s (namespace: %s)", pod.Name, pod.Namespace)))
		sb.WriteString("\n\n")

		// Status summary
		phase := podPhaseReason(pod)
		sb.WriteString(util.FormatKeyValue("STATUS", phase))
		sb.WriteString("\n")
		_, _, restarts := podContainerSummary(pod)
		sb.WriteString(util.FormatKeyValue("RESTARTS", fmt.Sprintf("%d", restarts)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("NODE", pod.Spec.NodeName))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("AGE", util.FormatAge(pod.CreationTimestamp.Time)))
		sb.WriteString("\n")

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		findings := 0

		// Check container statuses
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				reason := cs.State.Waiting.Reason
				switch reason {
				case "CrashLoopBackOff":
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Container '%s' is in CrashLoopBackOff", cs.Name)))
					sb.WriteString("\n")
					if cs.LastTerminationState.Terminated != nil {
						t := cs.LastTerminationState.Terminated
						sb.WriteString(fmt.Sprintf("  - Last termination reason: %s\n", t.Reason))
						sb.WriteString(fmt.Sprintf("  - Exit code: %d\n", t.ExitCode))
						if t.Reason == "OOMKilled" {
							sb.WriteString("  - Container was killed due to out-of-memory\n")
						}
					}
					findings++
				case "ImagePullBackOff", "ErrImagePull":
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Container '%s' cannot pull image: %s", cs.Name, cs.State.Waiting.Message)))
					sb.WriteString("\n")
					findings++
				default:
					sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Container '%s' is waiting: %s", cs.Name, reason)))
					sb.WriteString("\n")
					findings++
				}
			}
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Container '%s' terminated with exit code %d (%s)", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)))
				sb.WriteString("\n")
				findings++
			}
			if cs.RestartCount > util.HighRestartThreshold {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Container '%s' has high restart count: %d", cs.Name, cs.RestartCount)))
				sb.WriteString("\n")
				findings++
			}
		}

		// Check pod conditions
		for _, cond := range pod.Status.Conditions {
			if cond.Status == corev1.ConditionFalse && cond.Type == corev1.PodScheduled {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Pod not scheduled: %s", cond.Message)))
				sb.WriteString("\n")
				findings++
			}
			if cond.Status == corev1.ConditionFalse && cond.Type == corev1.PodReady {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Pod not ready: %s", cond.Message)))
				sb.WriteString("\n")
				findings++
			}
		}

		// Check resource limits
		for _, c := range pod.Spec.Containers {
			if c.Resources.Limits == nil || c.Resources.Limits.Cpu().IsZero() {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Container '%s' has no CPU limit set", c.Name)))
				sb.WriteString("\n")
				findings++
			}
			if c.Resources.Limits == nil || c.Resources.Limits.Memory().IsZero() {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Container '%s' has no memory limit set", c.Name)))
				sb.WriteString("\n")
				findings++
			}
		}

		if findings == 0 {
			sb.WriteString("  No issues found - pod appears healthy.\n")
		}

		// Warning events
		events, err := client.GetEventsForObject(ctx, input.Namespace, input.Name)
		if err == nil {
			warningEvents := 0
			for _, e := range events {
				if e.Type == "Warning" {
					warningEvents++
				}
			}
			if warningEvents > 0 {
				sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d Warning events in recent history", warningEvents))))
				for _, e := range events {
					if e.Type == "Warning" {
						sb.WriteString(fmt.Sprintf("  - %s: %s", e.Reason, e.Message))
						if e.Count > 1 {
							sb.WriteString(fmt.Sprintf(" (x%d)", e.Count))
						}
						sb.WriteString("\n")
					}
				}
			}
		}

		// Fetch logs from crashing containers
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				sb.WriteString(fmt.Sprintf("\nRECENT LOGS (container '%s', previous instance):\n", cs.Name))
				logs, err := client.GetPodLogs(ctx, input.Namespace, input.Name, cs.Name, 50, true, "")
				if err != nil {
					sb.WriteString(fmt.Sprintf("  (could not fetch logs: %v)\n", err))
				} else if logs == "" {
					sb.WriteString("  (no logs available)\n")
				} else {
					sb.WriteString(logs)
					sb.WriteString("\n")
				}
			}
		}

		// Suggested actions
		sb.WriteString("\nSUGGESTED ACTIONS:\n")
		actionNum := 1
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
				for _, c := range pod.Spec.Containers {
					if c.Name == cs.Name && c.Resources.Limits != nil {
						sb.WriteString(fmt.Sprintf("%d. Increase memory limit for container '%s' (currently %s, OOMKilled)\n",
							actionNum, c.Name, c.Resources.Limits.Memory().String()))
						actionNum++
					}
				}
			}
			if cs.State.Waiting != nil {
				switch cs.State.Waiting.Reason {
				case "ImagePullBackOff", "ErrImagePull":
					sb.WriteString(fmt.Sprintf("%d. Check image name and registry credentials for container '%s'\n", actionNum, cs.Name))
					actionNum++
				case "CrashLoopBackOff":
					sb.WriteString(fmt.Sprintf("%d. Check application logs for container '%s' (use get_pod_logs with previous=true)\n", actionNum, cs.Name))
					actionNum++
				}
			}
		}
		if pod.Status.Phase == corev1.PodPending {
			sb.WriteString(fmt.Sprintf("%d. Check cluster capacity and node selectors/tolerations\n", actionNum))
			actionNum++
		}
		if actionNum == 1 {
			sb.WriteString("  No specific actions needed - pod is healthy.\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// diagnose_namespace
	mcp.AddTool(server, &mcp.Tool{
		Name:        "diagnose_namespace",
		Description: "Health check an entire namespace. Finds unhealthy pods, failing deployments, pending PVCs, warning events, and pods with high restart counts. Use this to quickly assess namespace health.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input diagnoseNamespaceInput) (*mcp.CallToolResult, any, error) {
		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Namespace Diagnosis: %s", input.Namespace)))
		sb.WriteString("\n\n")

		findings := 0

		// 1. Check pods
		pods, err := client.ListPods(ctx, input.Namespace, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		unhealthyPods := 0
		highRestartPods := 0
		for i := range pods {
			p := &pods[i]
			if !isPodHealthy(p) {
				unhealthyPods++
			}
			_, _, restarts := podContainerSummary(p)
			if restarts > util.HighRestartThreshold {
				highRestartPods++
			}
		}

		sb.WriteString(util.FormatSubHeader("Pod Summary"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  Total: %d, Unhealthy: %d, High Restarts: %d\n", len(pods), unhealthyPods, highRestartPods))

		if unhealthyPods > 0 {
			sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%d unhealthy pods", unhealthyPods))))
			for i := range pods {
				p := &pods[i]
				if !isPodHealthy(p) {
					_, _, restarts := podContainerSummary(p)
					sb.WriteString(fmt.Sprintf("  - %s: %s (restarts: %d)\n", p.Name, podPhaseReason(p), restarts))
				}
			}
			findings++
		}

		if highRestartPods > 0 {
			sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d pods with >%d restarts", highRestartPods, util.HighRestartThreshold))))
			for i := range pods {
				p := &pods[i]
				_, _, restarts := podContainerSummary(p)
				if restarts > util.HighRestartThreshold {
					sb.WriteString(fmt.Sprintf("  - %s: %d restarts\n", p.Name, restarts))
				}
			}
			findings++
		}

		// 2. Check deployments
		deployments, err := client.ListDeployments(ctx, input.Namespace, metav1.ListOptions{})
		if err == nil {
			failingDeploys := 0
			for _, d := range deployments {
				desired := int32(0)
				if d.Spec.Replicas != nil {
					desired = *d.Spec.Replicas
				}
				if d.Status.AvailableReplicas < desired {
					failingDeploys++
				}
			}
			if failingDeploys > 0 {
				sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d deployments with unavailable replicas", failingDeploys))))
				for _, d := range deployments {
					desired := int32(0)
					if d.Spec.Replicas != nil {
						desired = *d.Spec.Replicas
					}
					if d.Status.AvailableReplicas < desired {
						sb.WriteString(fmt.Sprintf("  - %s: %d/%d available\n", d.Name, d.Status.AvailableReplicas, desired))
					}
				}
				findings++
			}
		}

		// 3. Warning events in last hour
		events, err := client.ListEvents(ctx, input.Namespace, metav1.ListOptions{})
		if err == nil {
			oneHourAgo := time.Now().Add(-1 * time.Hour)
			warningCount := 0
			for _, e := range events {
				eventTime := e.LastTimestamp.Time
				if eventTime.IsZero() {
					eventTime = e.CreationTimestamp.Time
				}
				if e.Type == "Warning" && eventTime.After(oneHourAgo) {
					warningCount++
				}
			}
			if warningCount > 0 {
				sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d warning events in the last hour", warningCount))))
				findings++
			}
		}

		// 4. Pending PVCs
		pvcs, err := client.ListPVCs(ctx, input.Namespace, metav1.ListOptions{})
		if err == nil {
			pendingPVCs := 0
			for _, pvc := range pvcs {
				if pvc.Status.Phase != corev1.ClaimBound {
					pendingPVCs++
				}
			}
			if pendingPVCs > 0 {
				sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d PVCs not bound", pendingPVCs))))
				for _, pvc := range pvcs {
					if pvc.Status.Phase != corev1.ClaimBound {
						sb.WriteString(fmt.Sprintf("  - %s: %s\n", pvc.Name, pvc.Status.Phase))
					}
				}
				findings++
			}
		}

		// Overall assessment
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Namespace appears healthy. No issues found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", findings))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// diagnose_cluster
	mcp.AddTool(server, &mcp.Tool{
		Name:        "diagnose_cluster",
		Description: "Cluster-wide health check. Checks node conditions, pod health across all namespaces, kube-system health, and warning events. Use this for a broad cluster health overview.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input diagnoseClusterInput) (*mcp.CallToolResult, any, error) {
		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Cluster Health Report"))
		sb.WriteString("\n\n")

		findings := 0

		// 1. Node health
		nodes, err := client.ListNodes(ctx, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing nodes", err), nil, nil
		}

		sb.WriteString(util.FormatSubHeader("Node Health"))
		sb.WriteString("\n")
		notReadyNodes := 0
		pressureNodes := 0
		for _, n := range nodes {
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
					sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' is NotReady", n.Name))))
					notReadyNodes++
					findings++
				}
				if (cond.Type == corev1.NodeMemoryPressure || cond.Type == corev1.NodeDiskPressure || cond.Type == corev1.NodePIDPressure) && cond.Status == corev1.ConditionTrue {
					sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' has %s", n.Name, cond.Type))))
					pressureNodes++
					findings++
				}
			}
		}
		if notReadyNodes == 0 && pressureNodes == 0 {
			sb.WriteString(fmt.Sprintf("  All %d nodes healthy.\n", len(nodes)))
		}

		// 2. Pod summary across all namespaces
		pods, err := client.ListPods(ctx, "", metav1.ListOptions{})
		if err == nil {
			phases := make(map[string]int)
			unhealthy := 0
			for i := range pods {
				phases[string(pods[i].Status.Phase)]++
				if !isPodHealthy(&pods[i]) {
					unhealthy++
				}
			}

			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Pod Summary (all namespaces)"))
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("  Total: %d\n", len(pods)))
			for phase, count := range phases {
				sb.WriteString(fmt.Sprintf("  %s: %d\n", phase, count))
			}
			if unhealthy > 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d unhealthy pods cluster-wide", unhealthy))))
				findings++
			}
		}

		// 3. Warning events cluster-wide in last hour
		events, err := client.ListEvents(ctx, "", metav1.ListOptions{})
		if err == nil {
			oneHourAgo := time.Now().Add(-1 * time.Hour)
			warningCount := 0
			for _, e := range events {
				eventTime := e.LastTimestamp.Time
				if eventTime.IsZero() {
					eventTime = e.CreationTimestamp.Time
				}
				if e.Type == "Warning" && eventTime.After(oneHourAgo) {
					warningCount++
				}
			}
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent Events"))
			sb.WriteString("\n")
			if warningCount > 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d warning events in the last hour", warningCount))))
				findings++
			} else {
				sb.WriteString("  No warning events in the last hour.\n")
			}
		}

		// 4. kube-system health
		kubeSystemPods, err := client.ListPods(ctx, "kube-system", metav1.ListOptions{})
		if err == nil {
			kubeUnhealthy := 0
			for i := range kubeSystemPods {
				if !isPodHealthy(&kubeSystemPods[i]) {
					kubeUnhealthy++
				}
			}
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("kube-system Health"))
			sb.WriteString("\n")
			if kubeUnhealthy > 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%d unhealthy pods in kube-system", kubeUnhealthy))))
				for i := range kubeSystemPods {
					p := &kubeSystemPods[i]
					if !isPodHealthy(p) {
						sb.WriteString(fmt.Sprintf("  - %s: %s\n", p.Name, podPhaseReason(p)))
					}
				}
				findings++
			} else {
				sb.WriteString(fmt.Sprintf("  All %d kube-system pods healthy.\n", len(kubeSystemPods)))
			}
		}

		// 5. Resource utilization (if metrics available)
		nodeMetrics, err := client.GetNodeMetrics(ctx)
		if err == nil && len(nodeMetrics) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Resource Utilization"))
			sb.WriteString("\n")
			var totalCPU, totalMem int64
			var capCPU, capMem int64
			for _, n := range nodes {
				capCPU += n.Status.Capacity.Cpu().MilliValue()
				capMem += n.Status.Capacity.Memory().Value()
			}
			for _, m := range nodeMetrics {
				totalCPU += m.Usage.Cpu().MilliValue()
				totalMem += m.Usage.Memory().Value()
			}
			if capCPU > 0 {
				sb.WriteString(fmt.Sprintf("  CPU:    %dm / %dm (%.1f%%)\n", totalCPU, capCPU, float64(totalCPU)/float64(capCPU)*100))
			}
			if capMem > 0 {
				sb.WriteString(fmt.Sprintf("  Memory: %s / %s (%.1f%%)\n", formatBytes(totalMem), formatBytes(capMem), float64(totalMem)/float64(capMem)*100))
			}
		}

		// Overall
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Cluster appears healthy. No issues found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", findings))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// find_unhealthy_pods
	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_unhealthy_pods",
		Description: "Find all pods that are not in a healthy state — CrashLoopBackOff, ImagePullBackOff, Pending, Error, OOMKilled, etc. Use this to quickly identify problem pods cluster-wide or in a namespace.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input findUnhealthyPodsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)

		pods, err := client.ListPods(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "STATUS", "RESTARTS", "AGE", "NODE"}
		rows := make([][]string, 0)
		for i := range pods {
			p := &pods[i]
			if isPodHealthy(p) {
				continue
			}
			_, _, restarts := podContainerSummary(p)
			rows = append(rows, []string{
				p.Name,
				p.Namespace,
				podPhaseReason(p),
				fmt.Sprintf("%d", restarts),
				util.FormatAge(p.CreationTimestamp.Time),
				p.Spec.NodeName,
			})
		}

		var sb strings.Builder
		scope := displayNS(input.Namespace)
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Unhealthy Pods (namespace: %s)", scope)))
		sb.WriteString("\n")

		if len(rows) == 0 {
			sb.WriteString("No unhealthy pods found.\n")
		} else {
			sb.WriteString(util.FormatTable(headers, rows))
			sb.WriteString(fmt.Sprintf("\n%s out of %d total\n", util.FormatCount("unhealthy pods", len(rows)), len(pods)))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// check_resource_quotas
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_resource_quotas",
		Description: "Check resource quota usage across namespaces. Flags namespaces approaching limits (>80%% usage). Use this to find resource constraints.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input checkResourceQuotasInput) (*mcp.CallToolResult, any, error) {
		var namespaces []string
		if input.Namespace != "" {
			namespaces = []string{input.Namespace}
		} else {
			nsList, err := client.ListNamespaces(ctx)
			if err != nil {
				return util.HandleK8sError("listing namespaces", err), nil, nil
			}
			for _, ns := range nsList {
				namespaces = append(namespaces, ns.Name)
			}
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Resource Quota Usage"))
		sb.WriteString("\n\n")

		totalQuotas := 0
		warnings := 0
		for _, ns := range namespaces {
			quotas, err := client.ListResourceQuotas(ctx, ns)
			if err != nil || len(quotas) == 0 {
				continue
			}

			for _, q := range quotas {
				totalQuotas++
				sb.WriteString(fmt.Sprintf("Namespace: %s, Quota: %s\n", ns, q.Name))

				headers := []string{"RESOURCE", "USED", "HARD", "USAGE %"}
				rows := make([][]string, 0)
				for resource, hard := range q.Status.Hard {
					used := q.Status.Used[resource]
					pct := float64(0)
					if hard.Value() > 0 {
						pct = float64(used.Value()) / float64(hard.Value()) * 100
					}
					pctStr := fmt.Sprintf("%.1f%%", pct)
					if pct >= float64(util.ResourceUsageWarningPercent) {
						pctStr += " [WARNING]"
						warnings++
					}
					rows = append(rows, []string{
						string(resource),
						used.String(),
						hard.String(),
						pctStr,
					})
				}
				sb.WriteString(util.FormatTable(headers, rows))
				sb.WriteString("\n")
			}
		}

		if totalQuotas == 0 {
			sb.WriteString("No resource quotas found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Total: %d quotas checked, %d warnings\n", totalQuotas, warnings))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// isPodHealthy returns true if the pod is in a healthy state.
func isPodHealthy(p *corev1.Pod) bool {
	// Running and all containers ready
	if p.Status.Phase == corev1.PodRunning {
		for _, cs := range p.Status.ContainerStatuses {
			if !cs.Ready {
				return false
			}
			if cs.State.Waiting != nil {
				return false
			}
		}
		return true
	}
	// Succeeded (completed jobs) are healthy
	if p.Status.Phase == corev1.PodSucceeded {
		return true
	}
	return false
}
