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

// --- list_contexts ---

type listContextsInput struct{}

// --- list_namespaces ---

type listNamespacesInput struct{}

// --- cluster_info ---

type clusterInfoInput struct{}

func registerClusterTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_contexts
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_contexts",
		Description: "List all available Kubernetes contexts from kubeconfig and identify the current context. Use this to see which clusters are configured.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listContextsInput) (*mcp.CallToolResult, any, error) {
		contexts, current, err := k8s.ListAvailableContexts()
		if err != nil {
			return util.HandleK8sError("listing contexts", err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Kubernetes Contexts"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("Current context: %s\n\n", current))

		for _, c := range contexts {
			marker := "  "
			if c == current {
				marker = "* "
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, c))
		}
		sb.WriteString(fmt.Sprintf("\nTotal: %d contexts\n", len(contexts)))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_namespaces
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List all namespaces in the cluster with their status and age. Use this to discover what namespaces exist before inspecting resources.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listNamespacesInput) (*mcp.CallToolResult, any, error) {
		namespaces, err := client.ListNamespaces(ctx)
		if err != nil {
			return util.HandleK8sError("listing namespaces", err), nil, nil
		}

		headers := []string{"NAME", "STATUS", "AGE", "LABELS"}
		rows := make([][]string, 0, len(namespaces))
		for _, ns := range namespaces {
			rows = append(rows, []string{
				ns.Name,
				string(ns.Status.Phase),
				util.FormatAge(ns.CreationTimestamp.Time),
				util.FormatLabels(ns.Labels),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Namespaces"))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\nTotal: %d namespaces\n", len(namespaces)))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// cluster_info
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cluster_info",
		Description: "Get cluster version, node count, namespace count, and overall resource summary. Use this for a quick cluster overview.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input clusterInfoInput) (*mcp.CallToolResult, any, error) {
		timeoutCtx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
		defer cancel()

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Cluster Information"))
		sb.WriteString("\n")

		// Server version
		version, err := client.Clientset.Discovery().ServerVersion()
		if err != nil {
			sb.WriteString(fmt.Sprintf("Server Version: error (%v)\n", err))
		} else {
			sb.WriteString(fmt.Sprintf("Server Version: %s\n", version.GitVersion))
		}

		// Node count
		nodes, err := client.Clientset.CoreV1().Nodes().List(timeoutCtx, metav1.ListOptions{})
		if err != nil {
			sb.WriteString(fmt.Sprintf("Nodes: error (%v)\n", err))
		} else {
			readyCount := 0
			for _, n := range nodes.Items {
				for _, c := range n.Status.Conditions {
					if c.Type == "Ready" && c.Status == "True" {
						readyCount++
					}
				}
			}
			sb.WriteString(fmt.Sprintf("Nodes: %d total, %d ready\n", len(nodes.Items), readyCount))
		}

		// Namespace count
		namespaces, err := client.ListNamespaces(ctx)
		if err != nil {
			sb.WriteString(fmt.Sprintf("Namespaces: error (%v)\n", err))
		} else {
			sb.WriteString(fmt.Sprintf("Namespaces: %d\n", len(namespaces)))
		}

		// Pod count (all namespaces)
		pods, err := client.Clientset.CoreV1().Pods("").List(timeoutCtx, metav1.ListOptions{})
		if err != nil {
			sb.WriteString(fmt.Sprintf("Pods: error (%v)\n", err))
		} else {
			running := 0
			for _, p := range pods.Items {
				if p.Status.Phase == "Running" {
					running++
				}
			}
			sb.WriteString(fmt.Sprintf("Pods: %d total, %d running\n", len(pods.Items), running))
		}

		// Service count
		services, err := client.Clientset.CoreV1().Services("").List(timeoutCtx, metav1.ListOptions{})
		if err != nil {
			sb.WriteString(fmt.Sprintf("Services: error (%v)\n", err))
		} else {
			sb.WriteString(fmt.Sprintf("Services: %d\n", len(services.Items)))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}
