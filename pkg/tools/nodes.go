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

type listNodesInput struct {
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter (e.g. node-role.kubernetes.io/control-plane)"`
}

type getNodeDetailInput struct {
	Name string `json:"name" jsonschema:"Node name"`
}

func registerNodeTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_nodes
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_nodes",
		Description: "List all nodes with status, roles, version, and CPU/memory capacity. Use label_selector to filter by role or other labels.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listNodesInput) (*mcp.CallToolResult, any, error) {
		opts := util.ListOptions(input.LabelSelector, "")

		nodes, err := client.ListNodes(ctx, opts)
		if err != nil {
			return util.HandleK8sError("listing nodes", err), nil, nil
		}

		headers := []string{"NAME", "STATUS", "ROLES", "VERSION", "CPU", "MEMORY", "AGE"}
		rows := make([][]string, 0, len(nodes))
		for _, n := range nodes {
			rows = append(rows, []string{
				n.Name,
				nodeStatus(&n),
				nodeRoles(&n),
				n.Status.NodeInfo.KubeletVersion,
				n.Status.Capacity.Cpu().String(),
				n.Status.Capacity.Memory().String(),
				util.FormatAge(n.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Nodes"))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("nodes", len(nodes))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_node_detail
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_node_detail",
		Description: "Get detailed node info including conditions (MemoryPressure, DiskPressure, PIDPressure), taints, allocatable resources, and system info. Use this to investigate node issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getNodeDetailInput) (*mcp.CallToolResult, any, error) {
		node, err := client.GetNode(ctx, input.Name)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting node %s", input.Name), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Node: %s", node.Name)))
		sb.WriteString("\n")

		sb.WriteString(util.FormatKeyValue("Status", nodeStatus(node)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Roles", nodeRoles(node)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Age", util.FormatAge(node.CreationTimestamp.Time)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("OS", fmt.Sprintf("%s %s", node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.OSImage)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Kernel", node.Status.NodeInfo.KernelVersion))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Container Runtime", node.Status.NodeInfo.ContainerRuntimeVersion))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Kubelet", node.Status.NodeInfo.KubeletVersion))
		sb.WriteString("\n")

		// Capacity & Allocatable
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Resources"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  %-15s %-15s %-15s\n", "RESOURCE", "CAPACITY", "ALLOCATABLE"))
		sb.WriteString(fmt.Sprintf("  %-15s %-15s %-15s\n", "cpu",
			node.Status.Capacity.Cpu().String(),
			node.Status.Allocatable.Cpu().String()))
		sb.WriteString(fmt.Sprintf("  %-15s %-15s %-15s\n", "memory",
			node.Status.Capacity.Memory().String(),
			node.Status.Allocatable.Memory().String()))
		sb.WriteString(fmt.Sprintf("  %-15s %-15s %-15s\n", "pods",
			node.Status.Capacity.Pods().String(),
			node.Status.Allocatable.Pods().String()))
		ephemeral := node.Status.Capacity.StorageEphemeral()
		if ephemeral != nil && !ephemeral.IsZero() {
			sb.WriteString(fmt.Sprintf("  %-15s %-15s %-15s\n", "ephemeral",
				ephemeral.String(),
				node.Status.Allocatable.StorageEphemeral().String()))
		}

		// Conditions
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Conditions"))
		sb.WriteString("\n")
		for _, cond := range node.Status.Conditions {
			sb.WriteString(fmt.Sprintf("  %-20s %-6s %s\n", string(cond.Type), string(cond.Status), cond.Message))
		}

		// Taints
		if len(node.Spec.Taints) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Taints"))
			sb.WriteString("\n")
			for _, t := range node.Spec.Taints {
				sb.WriteString(fmt.Sprintf("  %s=%s:%s\n", t.Key, t.Value, t.Effect))
			}
		}

		// Addresses
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Addresses"))
		sb.WriteString("\n")
		for _, addr := range node.Status.Addresses {
			sb.WriteString(fmt.Sprintf("  %-15s %s\n", string(addr.Type), addr.Address))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// nodeStatus returns the overall status of a node.
func nodeStatus(n *corev1.Node) string {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

// nodeRoles extracts roles from node labels.
func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k := range n.Labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(k, "node-role.kubernetes.io/")
			if role != "" {
				roles = append(roles, role)
			}
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	return strings.Join(roles, ",")
}
