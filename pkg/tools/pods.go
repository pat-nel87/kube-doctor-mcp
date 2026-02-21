package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// --- list_pods ---

type listPodsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter (e.g. app=nginx)"`
	FieldSelector string `json:"field_selector,omitempty" jsonschema:"Field selector filter (e.g. status.phase=Running)"`
}

// --- get_pod_detail ---

type getPodDetailInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Name      string `json:"name" jsonschema:"Pod name"`
}

// --- get_pod_logs ---

type getPodLogsInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Name      string `json:"name" jsonschema:"Pod name"`
	Container string `json:"container,omitempty" jsonschema:"Container name (required for multi-container pods)"`
	TailLines int64  `json:"tail_lines,omitempty" jsonschema:"Number of lines from end (default 100)"`
	Previous  bool   `json:"previous,omitempty" jsonschema:"Get logs from previously terminated container (useful for crash loops)"`
	Since     string `json:"since,omitempty" jsonschema:"Only logs newer than this duration (e.g. 1h, 30m, 5s)"`
}

func registerPodTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_pods
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_pods",
		Description: "List pods in a namespace with status, restarts, age, and node placement. Use namespace='all' for all namespaces. Use label_selector to filter (e.g. app=nginx).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listPodsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, input.FieldSelector)

		pods, err := client.ListPods(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "STATUS", "READY", "RESTARTS", "AGE", "NODE"}
		rows := make([][]string, 0, len(pods))
		for i := range pods {
			ready, total, restarts := podContainerSummary(&pods[i])
			rows = append(rows, []string{
				pods[i].Name,
				pods[i].Namespace,
				podPhaseReason(&pods[i]),
				fmt.Sprintf("%d/%d", ready, total),
				fmt.Sprintf("%d", restarts),
				util.FormatAge(pods[i].CreationTimestamp.Time),
				pods[i].Spec.NodeName,
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Pods (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("pods", len(pods))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_pod_detail
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pod_detail",
		Description: "Get detailed information about a specific pod including container statuses, conditions, events, volumes, and resource requests/limits. Use this to investigate a specific pod.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getPodDetailInput) (*mcp.CallToolResult, any, error) {
		pod, err := client.GetPod(ctx, input.Namespace, input.Name)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting pod %s/%s", input.Namespace, input.Name), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Pod: %s (namespace: %s)", pod.Name, pod.Namespace)))
		sb.WriteString("\n")

		// Basic info
		sb.WriteString(util.FormatKeyValue("Status", string(pod.Status.Phase)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Node", pod.Spec.NodeName))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("IP", pod.Status.PodIP))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Age", util.FormatAge(pod.CreationTimestamp.Time)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Labels", util.FormatLabels(pod.Labels)))
		sb.WriteString("\n")
		if pod.Spec.ServiceAccountName != "" {
			sb.WriteString(util.FormatKeyValue("Service Account", pod.Spec.ServiceAccountName))
			sb.WriteString("\n")
		}

		// Containers
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Containers"))
		sb.WriteString("\n")
		for _, c := range pod.Spec.Containers {
			sb.WriteString(fmt.Sprintf("\n  Container: %s\n", c.Name))
			sb.WriteString(fmt.Sprintf("    Image: %s\n", c.Image))

			if c.Resources.Requests != nil {
				cpu := c.Resources.Requests.Cpu()
				mem := c.Resources.Requests.Memory()
				sb.WriteString(fmt.Sprintf("    Requests: cpu=%s, memory=%s\n", cpu.String(), mem.String()))
			}
			if c.Resources.Limits != nil {
				cpu := c.Resources.Limits.Cpu()
				mem := c.Resources.Limits.Memory()
				sb.WriteString(fmt.Sprintf("    Limits:   cpu=%s, memory=%s\n", cpu.String(), mem.String()))
			}

			if len(c.Ports) > 0 {
				ports := make([]string, 0, len(c.Ports))
				for _, p := range c.Ports {
					ports = append(ports, fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
				}
				sb.WriteString(fmt.Sprintf("    Ports: %s\n", strings.Join(ports, ", ")))
			}
		}

		// Container statuses
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Container Statuses"))
		sb.WriteString("\n")
		for _, cs := range pod.Status.ContainerStatuses {
			sb.WriteString(fmt.Sprintf("\n  %s: ready=%v, restarts=%d\n", cs.Name, cs.Ready, cs.RestartCount))
			if cs.State.Running != nil {
				sb.WriteString(fmt.Sprintf("    State: Running (since %s)\n", util.FormatAge(cs.State.Running.StartedAt.Time)))
			}
			if cs.State.Waiting != nil {
				sb.WriteString(fmt.Sprintf("    State: Waiting (%s: %s)\n", cs.State.Waiting.Reason, cs.State.Waiting.Message))
			}
			if cs.State.Terminated != nil {
				sb.WriteString(fmt.Sprintf("    State: Terminated (%s, exit code %d)\n", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
			}
			if cs.LastTerminationState.Terminated != nil {
				t := cs.LastTerminationState.Terminated
				sb.WriteString(fmt.Sprintf("    Last Termination: %s (exit code %d, %s)\n", t.Reason, t.ExitCode, util.FormatAge(t.FinishedAt.Time)))
			}
		}

		// Conditions
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Conditions"))
		sb.WriteString("\n")
		for _, cond := range pod.Status.Conditions {
			sb.WriteString(fmt.Sprintf("  %-20s %s", string(cond.Type), string(cond.Status)))
			if cond.Reason != "" {
				sb.WriteString(fmt.Sprintf(" (%s)", cond.Reason))
			}
			sb.WriteString("\n")
		}

		// Events
		events, err := client.GetEventsForObject(ctx, input.Namespace, input.Name)
		if err == nil && len(events) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent Events"))
			sb.WriteString("\n")
			for _, e := range events {
				sb.WriteString(fmt.Sprintf("  %-8s %-20s %s", e.Type, e.Reason, e.Message))
				if e.Count > 1 {
					sb.WriteString(fmt.Sprintf(" (x%d)", e.Count))
				}
				sb.WriteString("\n")
			}
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_pod_logs
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pod_logs",
		Description: "Get logs from a pod container. Supports tail lines, previous container logs (for crash loops), and time-based filtering. Use previous=true to get logs from a crashed container.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getPodLogsInput) (*mcp.CallToolResult, any, error) {
		logs, err := client.GetPodLogs(ctx, input.Namespace, input.Name, input.Container, input.TailLines, input.Previous, input.Since)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting logs for %s/%s", input.Namespace, input.Name), err), nil, nil
		}

		if logs == "" {
			logs = "(no logs available)"
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Logs: %s/%s", input.Namespace, input.Name)))
		if input.Container != "" {
			sb.WriteString(fmt.Sprintf(" (container: %s)", input.Container))
		}
		if input.Previous {
			sb.WriteString(" [previous]")
		}
		sb.WriteString("\n\n")
		sb.WriteString(logs)

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// podPhaseReason returns the most informative status string for a pod.
func podPhaseReason(p *corev1.Pod) string {
	// Check container statuses for more specific reasons
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	// Check init container statuses
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return "Init:" + cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return "Init:" + cs.State.Terminated.Reason
		}
	}
	if p.Status.Reason != "" {
		return p.Status.Reason
	}
	return string(p.Status.Phase)
}

// podContainerSummary returns (ready, total, restarts) for a pod.
func podContainerSummary(p *corev1.Pod) (int, int, int32) {
	total := len(p.Spec.Containers)
	ready := 0
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	return ready, total, restarts
}

func displayNS(ns string) string {
	if ns == "" || ns == "all" || ns == "*" {
		return "all"
	}
	return ns
}
