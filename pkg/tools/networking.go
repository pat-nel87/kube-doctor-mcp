package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type listServicesInput struct {
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"Label selector filter"`
}

type listIngressesInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
}

type getEndpointsInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Name      string `json:"name" jsonschema:"Service name"`
}

func registerNetworkingTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_services
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_services",
		Description: "List services with type, cluster IP, external IP, and ports. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listServicesInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions(input.LabelSelector, "")

		services, err := client.ListServices(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing services", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "PORTS", "AGE"}
		rows := make([][]string, 0, len(services))
		for _, svc := range services {
			ports := make([]string, 0, len(svc.Spec.Ports))
			for _, p := range svc.Spec.Ports {
				portStr := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
				if p.NodePort > 0 {
					portStr = fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol)
				}
				ports = append(ports, portStr)
			}

			externalIP := "<none>"
			if len(svc.Spec.ExternalIPs) > 0 {
				externalIP = strings.Join(svc.Spec.ExternalIPs, ",")
			} else if svc.Spec.Type == "LoadBalancer" && len(svc.Status.LoadBalancer.Ingress) > 0 {
				ips := make([]string, 0)
				for _, ing := range svc.Status.LoadBalancer.Ingress {
					if ing.IP != "" {
						ips = append(ips, ing.IP)
					} else if ing.Hostname != "" {
						ips = append(ips, ing.Hostname)
					}
				}
				if len(ips) > 0 {
					externalIP = strings.Join(ips, ",")
				}
			}

			rows = append(rows, []string{
				svc.Name,
				svc.Namespace,
				string(svc.Spec.Type),
				svc.Spec.ClusterIP,
				externalIP,
				strings.Join(ports, ","),
				util.FormatAge(svc.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Services (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("services", len(services))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_ingresses
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_ingresses",
		Description: "List ingresses with hosts, paths, backends, and TLS configuration. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listIngressesInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions("", "")

		ingresses, err := client.ListIngresses(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing ingresses", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "HOSTS", "PATHS", "TLS", "AGE"}
		rows := make([][]string, 0, len(ingresses))
		for _, ing := range ingresses {
			var hosts, paths []string
			for _, rule := range ing.Spec.Rules {
				hosts = append(hosts, rule.Host)
				if rule.HTTP != nil {
					for _, path := range rule.HTTP.Paths {
						paths = append(paths, path.Path)
					}
				}
			}

			tlsStr := "No"
			if len(ing.Spec.TLS) > 0 {
				tlsStr = "Yes"
			}

			rows = append(rows, []string{
				ing.Name,
				ing.Namespace,
				util.JoinNonEmpty(",", hosts...),
				util.JoinNonEmpty(",", paths...),
				tlsStr,
				util.FormatAge(ing.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Ingresses (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("ingresses", len(ingresses))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// get_endpoints
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_endpoints",
		Description: "Get endpoints for a service showing which pods back it and their ready status. Useful for debugging services with no endpoints or connectivity issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getEndpointsInput) (*mcp.CallToolResult, any, error) {
		endpoints, err := client.GetEndpoints(ctx, input.Namespace, input.Name)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting endpoints %s/%s", input.Namespace, input.Name), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Endpoints: %s (namespace: %s)", endpoints.Name, endpoints.Namespace)))
		sb.WriteString("\n")

		totalAddresses := 0
		for _, subset := range endpoints.Subsets {
			// Ready addresses
			if len(subset.Addresses) > 0 {
				sb.WriteString("\nReady Addresses:\n")
				for _, addr := range subset.Addresses {
					ref := ""
					if addr.TargetRef != nil {
						ref = fmt.Sprintf(" (%s/%s)", addr.TargetRef.Kind, addr.TargetRef.Name)
					}
					for _, port := range subset.Ports {
						sb.WriteString(fmt.Sprintf("  %s:%d%s\n", addr.IP, port.Port, ref))
					}
					totalAddresses++
				}
			}

			// Not-ready addresses
			if len(subset.NotReadyAddresses) > 0 {
				sb.WriteString("\nNot Ready Addresses:\n")
				for _, addr := range subset.NotReadyAddresses {
					ref := ""
					if addr.TargetRef != nil {
						ref = fmt.Sprintf(" (%s/%s)", addr.TargetRef.Kind, addr.TargetRef.Name)
					}
					for _, port := range subset.Ports {
						sb.WriteString(fmt.Sprintf("  %s:%d%s\n", addr.IP, port.Port, ref))
					}
				}
			}
		}

		if totalAddresses == 0 && len(endpoints.Subsets) == 0 {
			sb.WriteString("\n(no endpoints - check service selector matches pod labels)\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}
