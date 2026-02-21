package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type listDeploymentsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter (e.g. app=nginx)"`
}

type getDeploymentDetailInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Name      string `json:"name" jsonschema:"Deployment name"`
}

type listStatefulSetsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter"`
}

type listDaemonSetsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter"`
}

type listJobsInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter"`
}

func registerWorkloadTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_deployments
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_deployments",
		Description: "List deployments showing desired/ready/available replicas and strategy. Use namespace='all' for all namespaces. Useful for checking rollout status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listDeploymentsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		deployments, err := client.ListDeployments(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing deployments", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "READY", "UP-TO-DATE", "AVAILABLE", "AGE", "STRATEGY"}
		rows := make([][]string, 0, len(deployments))
		for _, d := range deployments {
			strategy := "RollingUpdate"
			if d.Spec.Strategy.Type != "" {
				strategy = string(d.Spec.Strategy.Type)
			}
			desired := int32(0)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			rows = append(rows, []string{
				d.Name,
				d.Namespace,
				fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, desired),
				fmt.Sprintf("%d", d.Status.UpdatedReplicas),
				fmt.Sprintf("%d", d.Status.AvailableReplicas),
				util.FormatAge(d.CreationTimestamp.Time),
				strategy,
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Deployments (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("deployments", len(deployments))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_deployment_detail
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_deployment_detail",
		Description: "Get detailed deployment info including rollout status, conditions, replica set history, and pod template. Use this to investigate deployment issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getDeploymentDetailInput) (*mcp.CallToolResult, any, error) {
		deploy, err := client.GetDeployment(ctx, input.Namespace, input.Name)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting deployment %s/%s", input.Namespace, input.Name), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Deployment: %s (namespace: %s)", deploy.Name, deploy.Namespace)))
		sb.WriteString("\n")

		desired := int32(0)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		sb.WriteString(util.FormatKeyValue("Replicas", fmt.Sprintf("%d desired, %d ready, %d available, %d updated",
			desired, deploy.Status.ReadyReplicas, deploy.Status.AvailableReplicas, deploy.Status.UpdatedReplicas)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Strategy", string(deploy.Spec.Strategy.Type)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Age", util.FormatAge(deploy.CreationTimestamp.Time)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Labels", util.FormatLabels(deploy.Labels)))
		sb.WriteString("\n")
		if deploy.Spec.Selector != nil {
			sb.WriteString(util.FormatKeyValue("Selector", util.FormatLabels(deploy.Spec.Selector.MatchLabels)))
			sb.WriteString("\n")
		}

		// Conditions
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Conditions"))
		sb.WriteString("\n")
		for _, cond := range deploy.Status.Conditions {
			sb.WriteString(fmt.Sprintf("  %-20s %-6s %s\n", string(cond.Type), string(cond.Status), cond.Message))
		}

		// Pod template
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Pod Template"))
		sb.WriteString("\n")
		for _, c := range deploy.Spec.Template.Spec.Containers {
			sb.WriteString(fmt.Sprintf("  Container: %s\n", c.Name))
			sb.WriteString(fmt.Sprintf("    Image: %s\n", c.Image))
			if c.Resources.Requests != nil {
				sb.WriteString(fmt.Sprintf("    Requests: cpu=%s, memory=%s\n",
					c.Resources.Requests.Cpu().String(), c.Resources.Requests.Memory().String()))
			}
			if c.Resources.Limits != nil {
				sb.WriteString(fmt.Sprintf("    Limits:   cpu=%s, memory=%s\n",
					c.Resources.Limits.Cpu().String(), c.Resources.Limits.Memory().String()))
			}
		}

		// ReplicaSets
		selectorLabels := util.FormatLabels(deploy.Spec.Selector.MatchLabels)
		rsOpts := metav1.ListOptions{LabelSelector: selectorLabels}
		replicaSets, err := client.ListReplicaSets(ctx, input.Namespace, rsOpts)
		if err == nil && len(replicaSets) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("ReplicaSets"))
			sb.WriteString("\n")
			for _, rs := range replicaSets {
				desired := int32(0)
				if rs.Spec.Replicas != nil {
					desired = *rs.Spec.Replicas
				}
				sb.WriteString(fmt.Sprintf("  %s: %d/%d ready, revision=%s\n",
					rs.Name, rs.Status.ReadyReplicas, desired,
					rs.Annotations["deployment.kubernetes.io/revision"]))
			}
		}

		// Events
		events, err := client.GetEventsForObject(ctx, input.Namespace, input.Name)
		if err == nil && len(events) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent Events"))
			sb.WriteString("\n")
			for _, e := range events {
				sb.WriteString(fmt.Sprintf("  %-8s %-25s %s", e.Type, e.Reason, e.Message))
				if e.Count > 1 {
					sb.WriteString(fmt.Sprintf(" (x%d)", e.Count))
				}
				sb.WriteString("\n")
			}
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_statefulsets
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_statefulsets",
		Description: "List StatefulSets with replica status. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listStatefulSetsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		sets, err := client.ListStatefulSets(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing statefulsets", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "READY", "AGE"}
		rows := make([][]string, 0, len(sets))
		for _, s := range sets {
			desired := int32(0)
			if s.Spec.Replicas != nil {
				desired = *s.Spec.Replicas
			}
			rows = append(rows, []string{
				s.Name,
				s.Namespace,
				fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, desired),
				util.FormatAge(s.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("StatefulSets (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("statefulsets", len(sets))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_daemonsets
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_daemonsets",
		Description: "List DaemonSets showing desired/ready/available on nodes. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listDaemonSetsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		sets, err := client.ListDaemonSets(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing daemonsets", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "DESIRED", "READY", "UP-TO-DATE", "AVAILABLE", "AGE"}
		rows := make([][]string, 0, len(sets))
		for _, d := range sets {
			rows = append(rows, []string{
				d.Name,
				d.Namespace,
				fmt.Sprintf("%d", d.Status.DesiredNumberScheduled),
				fmt.Sprintf("%d", d.Status.NumberReady),
				fmt.Sprintf("%d", d.Status.UpdatedNumberScheduled),
				fmt.Sprintf("%d", d.Status.NumberAvailable),
				util.FormatAge(d.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("DaemonSets (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("daemonsets", len(sets))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_jobs
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List Jobs with completion status, duration, and active/succeeded/failed counts. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listJobsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		jobs, err := client.ListJobs(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing jobs", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "COMPLETIONS", "ACTIVE", "SUCCEEDED", "FAILED", "AGE"}
		rows := make([][]string, 0, len(jobs))
		for _, j := range jobs {
			completions := int32(1)
			if j.Spec.Completions != nil {
				completions = *j.Spec.Completions
			}
			rows = append(rows, []string{
				j.Name,
				j.Namespace,
				fmt.Sprintf("%d/%d", j.Status.Succeeded, completions),
				fmt.Sprintf("%d", j.Status.Active),
				fmt.Sprintf("%d", j.Status.Succeeded),
				fmt.Sprintf("%d", j.Status.Failed),
				util.FormatAge(j.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Jobs (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("jobs", len(jobs))))

		return util.SuccessResult(sb.String()), nil, nil
	})
}
