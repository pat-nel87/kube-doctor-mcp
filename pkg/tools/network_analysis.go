package tools

import (
	"context"
	"fmt"
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

type mapServiceTopologyInput struct {
	Namespace string `json:"namespace" jsonschema:"required,Kubernetes namespace to map (use a specific namespace, not 'all')"`
}

type traceIngressToBackendInput struct {
	Hostname string `json:"hostname" jsonschema:"required,Hostname to trace (e.g. api.example.com)"`
	Path     string `json:"path" jsonschema:"required,URL path to trace (e.g. /api/v1)"`
}

type listEndpointHealthInput struct {
	Namespace string `json:"namespace" jsonschema:"required,Kubernetes namespace to check endpoint health"`
}

type analyzeServiceConnectivityInput struct {
	Namespace   string `json:"namespace" jsonschema:"required,Kubernetes namespace"`
	ServiceName string `json:"service_name" jsonschema:"required,Service name to analyze"`
}

type analyzeAllIngressesInput struct {
	Namespace string `json:"namespace" jsonschema:"required,Kubernetes namespace to audit ingresses"`
}

type checkAGICHealthInput struct{}

func registerNetworkAnalysisTools(server *mcp.Server, client *k8s.ClusterClient) {

	// =========================================================================
	// 1. map_service_topology
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "map_service_topology",
		Description: "Map the full network topology for a namespace: services, their backing pods, ingresses exposing them, and inferred inter-service dependencies from pod environment variables. Produces structured text plus a Mermaid flowchart showing Internet -> Ingresses -> Services -> Pods with dependency edges.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input mapServiceTopologyInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace
		if ns == "" || ns == "all" || ns == "*" {
			return util.ErrorResult("map_service_topology requires a specific namespace, not 'all'"), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Service Topology Map (namespace: %s)", ns)))
		sb.WriteString("\n\n")

		// --- Gather services ---
		services, err := client.ListServices(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing services", err), nil, nil
		}

		sb.WriteString(util.FormatSubHeader("Services"))
		sb.WriteString("\n")
		if len(services) == 0 {
			sb.WriteString("  No services found in this namespace.\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		// Build a service -> pods mapping
		type svcInfo struct {
			Service  corev1.Service
			Pods     []corev1.Pod
			Health   *k8s.EndpointHealth
			HealthOK bool
		}
		svcMap := make(map[string]*svcInfo, len(services))

		svcHeaders := []string{"SERVICE", "TYPE", "CLUSTER-IP", "PORTS", "PODS", "READY-EP", "AGE"}
		svcRows := make([][]string, 0, len(services))

		for i := range services {
			svc := &services[i]
			info := &svcInfo{Service: *svc}

			// Resolve backing pods (best-effort)
			pods, podErr := client.GetPodsForService(ctx, svc)
			if podErr == nil {
				info.Pods = pods
			}

			// Resolve endpoint health (best-effort)
			health, healthErr := client.GetServiceEndpointHealth(ctx, ns, svc.Name)
			if healthErr == nil {
				info.Health = health
				info.HealthOK = true
			}

			svcMap[svc.Name] = info

			ports := formatServicePorts(svc)
			podCount := len(info.Pods)
			readyEP := "?"
			if info.HealthOK {
				readyEP = fmt.Sprintf("%d/%d", info.Health.ReadyCount, info.Health.TotalEndpoints)
			}

			svcRows = append(svcRows, []string{
				svc.Name,
				string(svc.Spec.Type),
				svc.Spec.ClusterIP,
				ports,
				fmt.Sprintf("%d", podCount),
				readyEP,
				util.FormatAge(svc.CreationTimestamp.Time),
			})
		}
		sb.WriteString(util.FormatTable(svcHeaders, svcRows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("services", len(services))))

		// --- Gather ingresses ---
		ingresses, err := client.ListIngresses(ctx, ns, metav1.ListOptions{})
		if err != nil {
			// Non-fatal: just note the error
			sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatFinding("WARNING", fmt.Sprintf("Could not list ingresses: %v", err))))
		}

		if len(ingresses) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Ingresses"))
			sb.WriteString("\n")
			ingHeaders := []string{"INGRESS", "HOSTS", "PATHS", "BACKEND-SERVICES", "TLS"}
			ingRows := make([][]string, 0, len(ingresses))
			for _, ing := range ingresses {
				hosts, paths, backends := extractIngressDetails(&ing)
				tlsStr := "No"
				if len(ing.Spec.TLS) > 0 {
					tlsStr = "Yes"
				}
				ingRows = append(ingRows, []string{
					ing.Name,
					strings.Join(hosts, ", "),
					strings.Join(paths, ", "),
					strings.Join(backends, ", "),
					tlsStr,
				})
			}
			sb.WriteString(util.FormatTable(ingHeaders, ingRows))
			sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("ingresses", len(ingresses))))
		}

		// --- Infer dependencies ---
		deps, depErr := client.InferServiceDependencies(ctx, ns)
		if depErr == nil && len(deps) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Inferred Service Dependencies"))
			sb.WriteString("\n")
			depHeaders := []string{"FROM", "TO", "CONFIDENCE", "SOURCE"}
			depRows := make([][]string, 0, len(deps))
			for _, d := range deps {
				depRows = append(depRows, []string{d.FromService, d.ToService, d.Confidence, d.Source})
			}
			sb.WriteString(util.FormatTable(depHeaders, depRows))
		}

		// --- Findings ---
		sb.WriteString("\n")
		sb.WriteString("FINDINGS:\n")
		findings := 0
		for _, info := range svcMap {
			if info.HealthOK && info.Health.TotalEndpoints == 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Service '%s' has 0 endpoints — no pods match its selector", info.Service.Name)))
				sb.WriteString("\n")
				findings++
			} else if info.HealthOK && info.Health.NotReadyCount > 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Service '%s' has %d not-ready endpoints out of %d total", info.Service.Name, info.Health.NotReadyCount, info.Health.TotalEndpoints)))
				sb.WriteString("\n")
				findings++
			}
			if info.Service.Spec.Selector == nil || len(info.Service.Spec.Selector) == 0 {
				if info.Service.Spec.Type != corev1.ServiceTypeExternalName {
					sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Service '%s' has no selector (headless/external)", info.Service.Name)))
					sb.WriteString("\n")
					findings++
				}
			}
		}
		if findings == 0 {
			sb.WriteString("  No issues found.\n")
		}

		// --- Mermaid Flowchart ---
		sb.WriteString("\nTOPOLOGY DIAGRAM:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionTB)

		// Internet node
		hasIngress := len(ingresses) > 0
		if hasIngress {
			fc.AddNode("INTERNET", "Internet", mermaid.ShapeCircle)
			fc.AddStyle("INTERNET", mermaid.SeverityInfo)
		}

		// Ingress subgraph
		if hasIngress {
			fc.AddSubgraph("ingresses", "Ingresses", func(sg *mermaid.Subgraph) {
				for _, ing := range ingresses {
					nodeID := mermaid.SafeID("ing_" + ing.Name)
					hosts, _, _ := extractIngressDetails(&ing)
					label := ing.Name + mermaid.BR() + strings.Join(hosts, ", ")
					sg.AddNode(nodeID, label, mermaid.ShapeTrapAlt)
				}
			})
			// Internet -> each ingress
			for _, ing := range ingresses {
				nodeID := mermaid.SafeID("ing_" + ing.Name)
				fc.AddEdge("INTERNET", nodeID, "", mermaid.EdgeSolid)
			}
		}

		// Services subgraph
		fc.AddSubgraph("svc_sub", fmt.Sprintf("Services (%s)", ns), func(sg *mermaid.Subgraph) {
			for _, info := range svcMap {
				svcNodeID := mermaid.SafeID("svc_" + info.Service.Name)
				label := info.Service.Name + mermaid.BR() + string(info.Service.Spec.Type)
				sg.AddNode(svcNodeID, label, mermaid.ShapeRound)
			}
		})

		// Style services based on health
		for _, info := range svcMap {
			svcNodeID := mermaid.SafeID("svc_" + info.Service.Name)
			if info.HealthOK && info.Health.TotalEndpoints == 0 {
				fc.AddStyle(svcNodeID, mermaid.SeverityCritical)
			} else if info.HealthOK && info.Health.NotReadyCount > 0 {
				fc.AddStyle(svcNodeID, mermaid.SeverityWarning)
			} else {
				fc.AddStyle(svcNodeID, mermaid.SeverityHealthy)
			}
		}

		// Ingress -> Service edges
		for _, ing := range ingresses {
			ingNodeID := mermaid.SafeID("ing_" + ing.Name)
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, path := range rule.HTTP.Paths {
					if path.Backend.Service != nil {
						svcNodeID := mermaid.SafeID("svc_" + path.Backend.Service.Name)
						edgeLabel := rule.Host + path.Path
						fc.AddEdge(ingNodeID, svcNodeID, edgeLabel, mermaid.EdgeSolid)
					}
				}
			}
		}

		// Pods subgraph
		fc.AddSubgraph("pods_sub", "Pods", func(sg *mermaid.Subgraph) {
			for _, info := range svcMap {
				for _, pod := range info.Pods {
					podNodeID := mermaid.SafeID("pod_" + pod.Name)
					status := podPhaseReason(&pod)
					label := pod.Name + mermaid.BR() + status
					sg.AddNode(podNodeID, label, mermaid.ShapeRect)
				}
			}
		})

		// Style pods based on health
		for _, info := range svcMap {
			for i := range info.Pods {
				podNodeID := mermaid.SafeID("pod_" + info.Pods[i].Name)
				if isPodHealthy(&info.Pods[i]) {
					fc.AddStyle(podNodeID, mermaid.SeverityHealthy)
				} else {
					fc.AddStyle(podNodeID, mermaid.SeverityCritical)
				}
			}
		}

		// Service -> Pod edges
		for _, info := range svcMap {
			svcNodeID := mermaid.SafeID("svc_" + info.Service.Name)
			for _, pod := range info.Pods {
				podNodeID := mermaid.SafeID("pod_" + pod.Name)
				fc.AddEdge(svcNodeID, podNodeID, "", mermaid.EdgeSolid)
			}
		}

		// Dependency edges (dotted)
		if depErr == nil {
			for _, dep := range deps {
				fromID := mermaid.SafeID("svc_" + dep.FromService)
				toID := mermaid.SafeID("svc_" + dep.ToService)
				fc.AddEdge(fromID, toID, dep.Confidence, mermaid.EdgeDotted)
			}
		}

		sb.WriteString(fc.RenderBlock())
		sb.WriteString("\n")

		return util.SuccessResult(sb.String()), nil, nil
	})

	// =========================================================================
	// 2. trace_ingress_to_backend
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "trace_ingress_to_backend",
		Description: "Trace the full request path from a hostname+path through Ingress -> Service -> Endpoints -> Pods. Checks AGIC annotations, backend service health, pod status, and available metrics. Produces a layered trace report plus a Mermaid sequence diagram of the request flow. Use this to debug 502/503/504 errors or routing issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input traceIngressToBackendInput) (*mcp.CallToolResult, any, error) {
		hostname := input.Hostname
		path := input.Path
		if hostname == "" {
			return util.ErrorResult("hostname is required"), nil, nil
		}
		if path == "" {
			path = "/"
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Ingress-to-Backend Trace: %s%s", hostname, path)))
		sb.WriteString("\n\n")

		findings := 0

		// --- [1] INGRESS layer ---
		sb.WriteString(util.FormatSubHeader("[1] INGRESS"))
		sb.WriteString("\n")

		ing, matchedRule, matchedPath, err := client.FindIngressForHostPath(ctx, "", hostname, path)
		if err != nil {
			sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("No ingress found for %s%s: %v", hostname, path, err)))
			sb.WriteString("\n")
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			sb.WriteString("1. Verify the hostname and path are correct\n")
			sb.WriteString("2. Check that an Ingress resource exists with this host/path\n")
			sb.WriteString("3. Use list_ingresses to see available ingresses\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		sb.WriteString(util.FormatKeyValue("Ingress", fmt.Sprintf("%s/%s", ing.Namespace, ing.Name)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Ingress Class", ingressClassName(ing)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Matched Host", matchedRule.Host))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Matched Path", matchedPath.Path))
		sb.WriteString("\n")
		if matchedPath.PathType != nil {
			sb.WriteString(util.FormatKeyValue("Path Type", string(*matchedPath.PathType)))
			sb.WriteString("\n")
		}

		// TLS
		hasTLS := false
		for _, tls := range ing.Spec.TLS {
			for _, h := range tls.Hosts {
				if h == hostname {
					hasTLS = true
					sb.WriteString(util.FormatKeyValue("TLS Secret", tls.SecretName))
					sb.WriteString("\n")
					break
				}
			}
			if hasTLS {
				break
			}
		}
		if !hasTLS {
			sb.WriteString(util.FormatFinding("INFO", "No TLS configured for this host"))
			sb.WriteString("\n")
		}

		// AGIC annotations
		agicAnnotations := k8s.ParseAGICAnnotations(ing)
		if len(agicAnnotations) > 0 {
			sb.WriteString("\n  AGIC Annotations:\n")
			for _, a := range agicAnnotations {
				sb.WriteString(fmt.Sprintf("    %s: %s\n", a.Key, a.Value))
			}
		}

		// --- [2] SERVICE layer ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("[2] SERVICE"))
		sb.WriteString("\n")

		if matchedPath.Backend.Service == nil {
			sb.WriteString(util.FormatFinding("CRITICAL", "Ingress path has no service backend configured"))
			sb.WriteString("\n")
			findings++
			return util.SuccessResult(sb.String()), nil, nil
		}

		backendSvcName := matchedPath.Backend.Service.Name
		backendPort := ""
		if matchedPath.Backend.Service.Port.Number != 0 {
			backendPort = fmt.Sprintf("%d", matchedPath.Backend.Service.Port.Number)
		} else if matchedPath.Backend.Service.Port.Name != "" {
			backendPort = matchedPath.Backend.Service.Port.Name
		}

		sb.WriteString(util.FormatKeyValue("Backend Service", backendSvcName))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Backend Port", backendPort))
		sb.WriteString("\n")

		svc, svcErr := client.GetService(ctx, ing.Namespace, backendSvcName)
		if svcErr != nil {
			sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Backend service '%s' not found: %v", backendSvcName, svcErr)))
			sb.WriteString("\n")
			findings++
		} else {
			sb.WriteString(util.FormatKeyValue("Service Type", string(svc.Spec.Type)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Cluster IP", svc.Spec.ClusterIP))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Selector", util.FormatLabels(svc.Spec.Selector)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Ports", formatServicePorts(svc)))
			sb.WriteString("\n")

			// Validate port mapping
			portValid := false
			for _, p := range svc.Spec.Ports {
				if backendPort == fmt.Sprintf("%d", p.Port) || backendPort == p.Name {
					portValid = true
					break
				}
			}
			if !portValid && backendPort != "" {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Backend port '%s' does not match any service port", backendPort)))
				sb.WriteString("\n")
				findings++
			}
		}

		// --- [3] ENDPOINTS / PODS layer ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("[3] ENDPOINTS / PODS"))
		sb.WriteString("\n")

		health, healthErr := client.GetServiceEndpointHealth(ctx, ing.Namespace, backendSvcName)
		if healthErr != nil {
			sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Could not get endpoint health: %v", healthErr)))
			sb.WriteString("\n")
			findings++
		} else {
			sb.WriteString(util.FormatKeyValue("Total Endpoints", fmt.Sprintf("%d", health.TotalEndpoints)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Ready", fmt.Sprintf("%d", health.ReadyCount)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Not Ready", fmt.Sprintf("%d", health.NotReadyCount)))
			sb.WriteString("\n")

			if health.TotalEndpoints == 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", "Service has 0 endpoints — requests will fail with 502/503"))
				sb.WriteString("\n")
				findings++
			} else if health.NotReadyCount > 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d endpoints are not ready — partial availability", health.NotReadyCount)))
				sb.WriteString("\n")
				findings++
			}

			// Detail ready endpoints
			if len(health.ReadyAddresses) > 0 {
				sb.WriteString("\n  Ready Endpoints:\n")
				for _, addr := range health.ReadyAddresses {
					line := fmt.Sprintf("    %s", addr.IP)
					if addr.PodName != "" {
						line += fmt.Sprintf(" (pod: %s)", addr.PodName)
					}
					if addr.NodeName != "" {
						line += fmt.Sprintf(" [node: %s]", addr.NodeName)
					}
					sb.WriteString(line + "\n")
				}
			}

			// Detail not-ready endpoints
			if len(health.NotReadyPods) > 0 {
				sb.WriteString("\n  Not-Ready Endpoints:\n")
				for _, addr := range health.NotReadyPods {
					line := fmt.Sprintf("    %s", addr.IP)
					if addr.PodName != "" {
						line += fmt.Sprintf(" (pod: %s)", addr.PodName)
					}
					sb.WriteString(line + "\n")
				}
			}
		}

		// Pod health detail
		if svcErr == nil {
			pods, podsErr := client.GetPodsForService(ctx, svc)
			if podsErr == nil && len(pods) > 0 {
				sb.WriteString("\n  Pod Health:\n")
				podHeaders := []string{"POD", "STATUS", "READY", "RESTARTS", "AGE"}
				podRows := make([][]string, 0, len(pods))
				for i := range pods {
					p := &pods[i]
					ready, total, restarts := podContainerSummary(p)
					podRows = append(podRows, []string{
						p.Name,
						podPhaseReason(p),
						fmt.Sprintf("%d/%d", ready, total),
						fmt.Sprintf("%d", restarts),
						util.FormatAge(p.CreationTimestamp.Time),
					})
					if !isPodHealthy(p) {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("  Backend pod '%s' is unhealthy: %s", p.Name, podPhaseReason(p))))
						sb.WriteString("\n")
						findings++
					}
					if restarts > util.HighRestartThreshold {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("  Backend pod '%s' has high restart count: %d", p.Name, restarts)))
						sb.WriteString("\n")
						findings++
					}
				}
				sb.WriteString(util.FormatTable(podHeaders, podRows))
			}

			// Attempt to get pod metrics (best-effort)
			if svc.Spec.Selector != nil && len(svc.Spec.Selector) > 0 {
				sel := labels.SelectorFromSet(svc.Spec.Selector)
				metricsOpts := metav1.ListOptions{LabelSelector: sel.String()}
				podMetrics, metricsErr := client.GetPodMetrics(ctx, ing.Namespace, metricsOpts)
				if metricsErr == nil && len(podMetrics) > 0 {
					sb.WriteString("\n  Pod Metrics:\n")
					for _, pm := range podMetrics {
						var totalCPU, totalMem int64
						for _, c := range pm.Containers {
							totalCPU += c.Usage.Cpu().MilliValue()
							totalMem += c.Usage.Memory().Value() / (1024 * 1024)
						}
						sb.WriteString(fmt.Sprintf("    %s: CPU=%dm, Memory=%dMi\n", pm.Name, totalCPU, totalMem))
					}
				}
			}
		}

		// --- Overall assessment ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Trace looks healthy. Requests to " + hostname + path + " should be routed correctly.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found along the request path. Review findings above.\n", findings))
		}

		// --- Mermaid Sequence Diagram ---
		sb.WriteString("\nREQUEST FLOW DIAGRAM:\n")
		seq := mermaid.NewSequence()
		seq.AddParticipant("CLIENT", "Client")
		if hasIngress(ing) {
			seq.AddParticipant("INGRESS", fmt.Sprintf("Ingress: %s", ing.Name))
		}
		seq.AddParticipant("SVC", fmt.Sprintf("Service: %s", backendSvcName))
		seq.AddParticipant("EP", "Endpoints")

		// Request flow
		seq.AddMessage("CLIENT", "INGRESS", fmt.Sprintf("%s%s", hostname, path), mermaid.MsgSolid)

		if len(agicAnnotations) > 0 {
			seq.AddNote("INGRESS", fmt.Sprintf("AGIC: %d annotations", len(agicAnnotations)), mermaid.NoteRight)
		}

		if hasTLS {
			seq.AddNote("INGRESS", "TLS termination", mermaid.NoteRight)
		}

		svcPortLabel := backendPort
		if svcPortLabel == "" {
			svcPortLabel = "default"
		}
		seq.AddMessage("INGRESS", "SVC", fmt.Sprintf("route to port %s", svcPortLabel), mermaid.MsgSolid)

		if healthErr == nil {
			if health.TotalEndpoints == 0 {
				seq.AddMessage("SVC", "EP", "NO ENDPOINTS", mermaid.MsgDotted)
				seq.AddNote("EP", "502/503 - no backends", mermaid.NoteRight)
			} else {
				seq.AddMessage("SVC", "EP", fmt.Sprintf("%d ready endpoint(s)", health.ReadyCount), mermaid.MsgSolid)
				if health.NotReadyCount > 0 {
					seq.AddNote("EP", fmt.Sprintf("%d not-ready", health.NotReadyCount), mermaid.NoteRight)
				}
			}
		} else {
			seq.AddMessage("SVC", "EP", "endpoints unknown", mermaid.MsgDotted)
		}

		sb.WriteString(seq.RenderBlock())
		sb.WriteString("\n")

		return util.SuccessResult(sb.String()), nil, nil
	})

	// =========================================================================
	// 3. list_endpoint_health
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_endpoint_health",
		Description: "Check endpoint health for every service in a namespace. Flags services with 0 ready endpoints as DEAD and services with partial readiness as DEGRADED. Use this to quickly find services that can't serve traffic.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listEndpointHealthInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace
		if ns == "" {
			return util.ErrorResult("namespace is required"), nil, nil
		}

		services, err := client.ListServices(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing services", err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Endpoint Health Report (namespace: %s)", ns)))
		sb.WriteString("\n\n")

		if len(services) == 0 {
			sb.WriteString("No services found in this namespace.\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		headers := []string{"SERVICE", "TYPE", "TOTAL-EP", "READY", "NOT-READY", "STATUS"}
		rows := make([][]string, 0, len(services))

		deadServices := 0
		degradedServices := 0
		healthyServices := 0
		findings := 0

		for _, svc := range services {
			health, healthErr := client.GetServiceEndpointHealth(ctx, ns, svc.Name)

			if healthErr != nil {
				rows = append(rows, []string{
					svc.Name,
					string(svc.Spec.Type),
					"?", "?", "?",
					"ERROR",
				})
				continue
			}

			status := "HEALTHY"
			if svc.Spec.Type == corev1.ServiceTypeExternalName {
				status = "EXTERNAL"
			} else if health.TotalEndpoints == 0 {
				if svc.Spec.Selector == nil || len(svc.Spec.Selector) == 0 {
					status = "NO-SELECTOR"
				} else {
					status = "DEAD"
					deadServices++
				}
			} else if health.NotReadyCount > 0 {
				status = "DEGRADED"
				degradedServices++
			} else {
				healthyServices++
			}

			rows = append(rows, []string{
				svc.Name,
				string(svc.Spec.Type),
				fmt.Sprintf("%d", health.TotalEndpoints),
				fmt.Sprintf("%d", health.ReadyCount),
				fmt.Sprintf("%d", health.NotReadyCount),
				status,
			})
		}

		sb.WriteString(util.FormatTable(headers, rows))

		sb.WriteString(fmt.Sprintf("\nSummary: %d healthy, %d degraded, %d dead out of %d services\n",
			healthyServices, degradedServices, deadServices, len(services)))

		// Findings
		sb.WriteString("\nFINDINGS:\n")
		for _, row := range rows {
			switch row[5] {
			case "DEAD":
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Service '%s' has 0 ready endpoints — all traffic will fail", row[0])))
				sb.WriteString("\n")
				findings++
			case "DEGRADED":
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Service '%s' has not-ready endpoints (%s/%s ready) — partial availability", row[0], row[3], row[2])))
				sb.WriteString("\n")
				findings++
			case "ERROR":
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Could not check endpoint health for service '%s'", row[0])))
				sb.WriteString("\n")
				findings++
			}
		}
		if findings == 0 {
			sb.WriteString("  All services have healthy endpoints.\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// =========================================================================
	// 4. analyze_service_connectivity
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "analyze_service_connectivity",
		Description: "Run a comprehensive connectivity analysis for a specific service. Checks: service exists, selector matches pods, endpoints are ready, port mappings are valid, NetworkPolicies that affect it, and Ingress exposure. Produces a full connectivity report with a Mermaid flowchart. Use this to debug why a service is unreachable.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeServiceConnectivityInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace
		svcName := input.ServiceName
		if ns == "" || svcName == "" {
			return util.ErrorResult("namespace and service_name are required"), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Service Connectivity Analysis: %s (namespace: %s)", svcName, ns)))
		sb.WriteString("\n\n")

		findings := 0

		// --- Check 1: Service exists ---
		sb.WriteString(util.FormatSubHeader("Check 1: Service Exists"))
		sb.WriteString("\n")
		svc, err := client.GetService(ctx, ns, svcName)
		if err != nil {
			sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Service '%s/%s' not found: %v", ns, svcName, err)))
			sb.WriteString("\n")
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			sb.WriteString("1. Verify the service name and namespace\n")
			sb.WriteString("2. Use list_services to see available services\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		sb.WriteString(util.FormatKeyValue("Type", string(svc.Spec.Type)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Cluster IP", svc.Spec.ClusterIP))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Selector", util.FormatLabels(svc.Spec.Selector)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatKeyValue("Ports", formatServicePorts(svc)))
		sb.WriteString("\n")
		sb.WriteString(util.FormatFinding("INFO", "Service exists"))
		sb.WriteString("\n")

		// --- Check 2: Selector matches pods ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Check 2: Selector Matches Pods"))
		sb.WriteString("\n")

		var matchedPods []corev1.Pod
		if svc.Spec.Selector == nil || len(svc.Spec.Selector) == 0 {
			sb.WriteString(util.FormatFinding("INFO", "Service has no selector (headless/ExternalName)"))
			sb.WriteString("\n")
		} else {
			pods, podErr := client.GetPodsForService(ctx, svc)
			if podErr != nil {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Could not list pods matching selector: %v", podErr)))
				sb.WriteString("\n")
				findings++
			} else {
				matchedPods = pods
				if len(pods) == 0 {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("No pods match selector %s — service cannot route traffic", util.FormatLabels(svc.Spec.Selector))))
					sb.WriteString("\n")
					findings++
				} else {
					healthyCount := 0
					for i := range pods {
						if isPodHealthy(&pods[i]) {
							healthyCount++
						}
					}
					sb.WriteString(fmt.Sprintf("  %d pods match selector (%d healthy, %d unhealthy)\n", len(pods), healthyCount, len(pods)-healthyCount))
					if healthyCount == 0 {
						sb.WriteString(util.FormatFinding("CRITICAL", "All matching pods are unhealthy"))
						sb.WriteString("\n")
						findings++
					} else if healthyCount < len(pods) {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d of %d matching pods are unhealthy", len(pods)-healthyCount, len(pods))))
						sb.WriteString("\n")
						findings++
					} else {
						sb.WriteString(util.FormatFinding("INFO", "All matching pods are healthy"))
						sb.WriteString("\n")
					}
				}
			}
		}

		// --- Check 3: Endpoints are ready ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Check 3: Endpoints Ready"))
		sb.WriteString("\n")

		health, healthErr := client.GetServiceEndpointHealth(ctx, ns, svcName)
		if healthErr != nil {
			sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Could not check endpoints: %v", healthErr)))
			sb.WriteString("\n")
			findings++
		} else {
			sb.WriteString(util.FormatKeyValue("Total Endpoints", fmt.Sprintf("%d", health.TotalEndpoints)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Ready", fmt.Sprintf("%d", health.ReadyCount)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Not Ready", fmt.Sprintf("%d", health.NotReadyCount)))
			sb.WriteString("\n")

			if health.TotalEndpoints == 0 && svc.Spec.Selector != nil && len(svc.Spec.Selector) > 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", "No endpoints — service has no backends to route to"))
				sb.WriteString("\n")
				findings++
			} else if health.ReadyCount == 0 && health.TotalEndpoints > 0 {
				sb.WriteString(util.FormatFinding("CRITICAL", "All endpoints are not-ready — service is effectively down"))
				sb.WriteString("\n")
				findings++
			} else if health.NotReadyCount > 0 {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("%d of %d endpoints not ready", health.NotReadyCount, health.TotalEndpoints)))
				sb.WriteString("\n")
				findings++
			} else if health.ReadyCount > 0 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("All %d endpoints are ready", health.ReadyCount)))
				sb.WriteString("\n")
			}
		}

		// --- Check 4: Port mappings valid ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Check 4: Port Mapping Validation"))
		sb.WriteString("\n")

		if len(matchedPods) > 0 {
			containerPorts := make(map[int32]bool)
			for _, pod := range matchedPods {
				for _, c := range pod.Spec.Containers {
					for _, p := range c.Ports {
						containerPorts[p.ContainerPort] = true
					}
				}
			}

			for _, sp := range svc.Spec.Ports {
				targetPort := sp.TargetPort.IntVal
				if targetPort == 0 {
					// Named port or same as service port
					targetPort = sp.Port
				}
				if len(containerPorts) > 0 && !containerPorts[targetPort] {
					sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("Service port %d/%s targets port %d, but no container declares this port", sp.Port, sp.Protocol, targetPort)))
					sb.WriteString("\n")
					findings++
				} else {
					sb.WriteString(fmt.Sprintf("  Port %d/%s -> target %d: OK\n", sp.Port, sp.Protocol, targetPort))
				}
			}
		} else {
			sb.WriteString("  Skipped — no matched pods to validate against.\n")
		}

		// --- Check 5: NetworkPolicies affecting this service ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Check 5: Network Policies"))
		sb.WriteString("\n")

		policies, npErr := client.ListNetworkPolicies(ctx, ns, metav1.ListOptions{})
		if npErr != nil {
			sb.WriteString(fmt.Sprintf("  Could not check network policies: %v\n", npErr))
		} else if len(policies) == 0 {
			sb.WriteString(util.FormatFinding("INFO", "No network policies in namespace — all traffic allowed by default"))
			sb.WriteString("\n")
		} else {
			matchingPolicies := 0
			for _, np := range policies {
				if svc.Spec.Selector == nil {
					continue
				}
				npSelector, selErr := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
				if selErr != nil {
					continue
				}
				// Check if the NP selector matches the service's pods' labels
				for _, pod := range matchedPods {
					if npSelector.Matches(labels.Set(pod.Labels)) {
						matchingPolicies++
						sb.WriteString(fmt.Sprintf("  Policy '%s' affects service pods\n", np.Name))
						for _, pt := range np.Spec.PolicyTypes {
							if pt == networkingv1.PolicyTypeIngress && len(np.Spec.Ingress) == 0 {
								sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("    Policy '%s' denies all ingress — may block traffic to this service", np.Name)))
								sb.WriteString("\n")
								findings++
							}
						}
						break // One match per policy is enough
					}
				}
			}
			if matchingPolicies == 0 {
				sb.WriteString(util.FormatFinding("INFO", "No network policies target this service's pods"))
				sb.WriteString("\n")
			}
		}

		// --- Check 6: Ingress exposure ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Check 6: Ingress Exposure"))
		sb.WriteString("\n")

		ingresses, ingErr := client.ListIngresses(ctx, ns, metav1.ListOptions{})
		if ingErr != nil {
			sb.WriteString(fmt.Sprintf("  Could not check ingresses: %v\n", ingErr))
		} else {
			exposingIngresses := 0
			for _, ing := range ingresses {
				for _, rule := range ing.Spec.Rules {
					if rule.HTTP == nil {
						continue
					}
					for _, p := range rule.HTTP.Paths {
						if p.Backend.Service != nil && p.Backend.Service.Name == svcName {
							sb.WriteString(fmt.Sprintf("  Ingress '%s' exposes this service via %s%s\n", ing.Name, rule.Host, p.Path))
							exposingIngresses++
						}
					}
				}
			}
			if exposingIngresses == 0 {
				sb.WriteString(util.FormatFinding("INFO", "Service is not exposed via any Ingress (internal only)"))
				sb.WriteString("\n")
			}
		}

		// --- Overall assessment ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Service connectivity looks healthy. No issues found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", findings))
		}

		// --- Mermaid Flowchart ---
		sb.WriteString("\nCONNECTIVITY DIAGRAM:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionTB)

		// Ingress nodes (if any)
		if ingErr == nil {
			for _, ing := range ingresses {
				for _, rule := range ing.Spec.Rules {
					if rule.HTTP == nil {
						continue
					}
					for _, p := range rule.HTTP.Paths {
						if p.Backend.Service != nil && p.Backend.Service.Name == svcName {
							ingID := mermaid.SafeID("ing_" + ing.Name)
							fc.AddNode(ingID, fmt.Sprintf("Ingress: %s%s%s", ing.Name, mermaid.BR(), rule.Host), mermaid.ShapeTrapAlt)
							fc.AddStyle(ingID, mermaid.SeverityInfo)
							svcID := mermaid.SafeID("svc_" + svcName)
							fc.AddEdge(ingID, svcID, p.Path, mermaid.EdgeSolid)
						}
					}
				}
			}
		}

		// Service node
		svcID := mermaid.SafeID("svc_" + svcName)
		svcLabel := fmt.Sprintf("Service: %s%s%s", svcName, mermaid.BR(), formatServicePorts(svc))
		fc.AddNode(svcID, svcLabel, mermaid.ShapeRound)

		// Style service node
		if healthErr == nil {
			if health.TotalEndpoints == 0 && svc.Spec.Selector != nil && len(svc.Spec.Selector) > 0 {
				fc.AddStyle(svcID, mermaid.SeverityCritical)
			} else if health.NotReadyCount > 0 {
				fc.AddStyle(svcID, mermaid.SeverityWarning)
			} else {
				fc.AddStyle(svcID, mermaid.SeverityHealthy)
			}
		}

		// Pod nodes
		for i := range matchedPods {
			pod := &matchedPods[i]
			podID := mermaid.SafeID("pod_" + pod.Name)
			podLabel := fmt.Sprintf("%s%s%s", pod.Name, mermaid.BR(), podPhaseReason(pod))
			fc.AddNode(podID, podLabel, mermaid.ShapeRect)
			fc.AddEdge(svcID, podID, "", mermaid.EdgeSolid)

			if isPodHealthy(pod) {
				fc.AddStyle(podID, mermaid.SeverityHealthy)
			} else {
				fc.AddStyle(podID, mermaid.SeverityCritical)
			}
		}

		// NetworkPolicy nodes
		if npErr == nil {
			for _, np := range policies {
				if svc.Spec.Selector == nil {
					continue
				}
				npSelector, selErr := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
				if selErr != nil {
					continue
				}
				for _, pod := range matchedPods {
					if npSelector.Matches(labels.Set(pod.Labels)) {
						npID := mermaid.SafeID("np_" + np.Name)
						fc.AddNode(npID, fmt.Sprintf("NetPol: %s", np.Name), mermaid.ShapeDiamond)
						fc.AddStyle(npID, mermaid.SeverityWarning)
						podID := mermaid.SafeID("pod_" + pod.Name)
						fc.AddEdge(npID, podID, "restricts", mermaid.EdgeDotted)
						break
					}
				}
			}
		}

		sb.WriteString(fc.RenderBlock())
		sb.WriteString("\n")

		return util.SuccessResult(sb.String()), nil, nil
	})

	// =========================================================================
	// 5. analyze_all_ingresses
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "analyze_all_ingresses",
		Description: "Audit every Ingress in a namespace: AGIC annotations, backend service existence and endpoint health, TLS configuration, and conflicting host/path rules across ingresses. Use this for a pre-deployment or post-incident ingress review.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeAllIngressesInput) (*mcp.CallToolResult, any, error) {
		ns := input.Namespace
		if ns == "" {
			return util.ErrorResult("namespace is required"), nil, nil
		}

		ingresses, err := client.ListIngresses(ctx, ns, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing ingresses", err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Ingress Audit (namespace: %s)", ns)))
		sb.WriteString("\n\n")

		if len(ingresses) == 0 {
			sb.WriteString("No ingresses found in this namespace.\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		totalFindings := 0

		// Track host+path combinations for conflict detection
		type hostPathEntry struct {
			IngressName string
			Path        string
			PathType    string
		}
		hostPaths := make(map[string][]hostPathEntry) // key = "host"

		// --- Per-ingress audit ---
		for _, ing := range ingresses {
			sb.WriteString(util.FormatSubHeader(fmt.Sprintf("Ingress: %s", ing.Name)))
			sb.WriteString("\n")

			sb.WriteString(util.FormatKeyValue("Class", ingressClassName(&ing)))
			sb.WriteString("\n")
			sb.WriteString(util.FormatKeyValue("Age", util.FormatAge(ing.CreationTimestamp.Time)))
			sb.WriteString("\n")

			// AGIC annotations
			agicAnnotations := k8s.ParseAGICAnnotations(&ing)
			if len(agicAnnotations) > 0 {
				sb.WriteString(fmt.Sprintf("  AGIC Annotations (%d):\n", len(agicAnnotations)))
				for _, a := range agicAnnotations {
					sb.WriteString(fmt.Sprintf("    %s: %s\n", a.Key, a.Value))
				}
			}

			// Check each rule
			for _, rule := range ing.Spec.Rules {
				host := rule.Host
				if host == "" {
					host = "*"
				}
				sb.WriteString(fmt.Sprintf("\n  Host: %s\n", host))

				if rule.HTTP == nil {
					sb.WriteString(util.FormatFinding("WARNING", "    Rule has no HTTP paths defined"))
					sb.WriteString("\n")
					totalFindings++
					continue
				}

				for _, path := range rule.HTTP.Paths {
					pathType := "Prefix"
					if path.PathType != nil {
						pathType = string(*path.PathType)
					}

					// Record for conflict detection
					hostPaths[host] = append(hostPaths[host], hostPathEntry{
						IngressName: ing.Name,
						Path:        path.Path,
						PathType:    pathType,
					})

					sb.WriteString(fmt.Sprintf("    Path: %s (type: %s)\n", path.Path, pathType))

					if path.Backend.Service == nil {
						sb.WriteString(util.FormatFinding("CRITICAL", "      No service backend configured"))
						sb.WriteString("\n")
						totalFindings++
						continue
					}

					backendName := path.Backend.Service.Name
					backendPort := ""
					if path.Backend.Service.Port.Number != 0 {
						backendPort = fmt.Sprintf("%d", path.Backend.Service.Port.Number)
					} else if path.Backend.Service.Port.Name != "" {
						backendPort = path.Backend.Service.Port.Name
					}
					sb.WriteString(fmt.Sprintf("      Backend: %s:%s\n", backendName, backendPort))

					// Verify backend service exists
					backendSvc, svcErr := client.GetService(ctx, ns, backendName)
					if svcErr != nil {
						sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("      Backend service '%s' NOT FOUND", backendName)))
						sb.WriteString("\n")
						totalFindings++
						continue
					}

					// Validate port
					portFound := false
					for _, sp := range backendSvc.Spec.Ports {
						if backendPort == fmt.Sprintf("%d", sp.Port) || backendPort == sp.Name {
							portFound = true
							break
						}
					}
					if !portFound && backendPort != "" {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("      Port '%s' not defined on service '%s'", backendPort, backendName)))
						sb.WriteString("\n")
						totalFindings++
					}

					// Check endpoint health
					epHealth, epErr := client.GetServiceEndpointHealth(ctx, ns, backendName)
					if epErr != nil {
						sb.WriteString(fmt.Sprintf("      Endpoints: could not check (%v)\n", epErr))
					} else if epHealth.TotalEndpoints == 0 {
						sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("      Service '%s' has 0 endpoints — this path will return 502/503", backendName)))
						sb.WriteString("\n")
						totalFindings++
					} else if epHealth.NotReadyCount > 0 {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("      Service '%s' has %d/%d ready endpoints", backendName, epHealth.ReadyCount, epHealth.TotalEndpoints)))
						sb.WriteString("\n")
						totalFindings++
					} else {
						sb.WriteString(fmt.Sprintf("      Endpoints: %d ready\n", epHealth.ReadyCount))
					}
				}
			}

			// TLS check
			if len(ing.Spec.TLS) > 0 {
				sb.WriteString("\n  TLS Configuration:\n")
				for _, tls := range ing.Spec.TLS {
					sb.WriteString(fmt.Sprintf("    Hosts: %s\n", strings.Join(tls.Hosts, ", ")))
					if tls.SecretName == "" {
						sb.WriteString(util.FormatFinding("WARNING", "    No TLS secret specified — may use default certificate"))
						sb.WriteString("\n")
						totalFindings++
					} else {
						sb.WriteString(fmt.Sprintf("    Secret: %s\n", tls.SecretName))
					}
				}
			} else {
				sb.WriteString(util.FormatFinding("INFO", "\n  No TLS configured — traffic is unencrypted"))
				sb.WriteString("\n")
			}

			sb.WriteString("\n")
		}

		// --- Conflict detection ---
		sb.WriteString(util.FormatSubHeader("Host/Path Conflict Analysis"))
		sb.WriteString("\n")
		conflicts := 0
		for host, entries := range hostPaths {
			if len(entries) <= 1 {
				continue
			}
			// Check for overlapping paths across different ingresses
			for i := 0; i < len(entries); i++ {
				for j := i + 1; j < len(entries); j++ {
					if entries[i].IngressName == entries[j].IngressName {
						continue // Same ingress, not a conflict
					}
					if pathsOverlap(entries[i].Path, entries[j].Path) {
						sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf(
							"Potential conflict: host '%s' path '%s' (ingress: %s) overlaps with path '%s' (ingress: %s)",
							host, entries[i].Path, entries[i].IngressName, entries[j].Path, entries[j].IngressName,
						)))
						sb.WriteString("\n")
						conflicts++
						totalFindings++
					}
				}
			}
		}
		if conflicts == 0 {
			sb.WriteString("  No host/path conflicts detected.\n")
		}

		// --- Overall ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Audit Summary"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  Ingresses audited: %d\n", len(ingresses)))
		if totalFindings == 0 {
			sb.WriteString("  No issues found. All ingresses look healthy.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", totalFindings))
		}

		return util.SuccessResult(sb.String()), nil, nil
	})

	// =========================================================================
	// 6. check_agic_health
	// =========================================================================
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_agic_health",
		Description: "Check the health of Azure Application Gateway Ingress Controller (AGIC). Finds the AGIC pod (label app=ingress-azure), checks its status, restarts, recent logs for errors, and AGIC ConfigMap. Use this when ingress routing through Azure Application Gateway is failing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input checkAGICHealthInput) (*mcp.CallToolResult, any, error) {
		var sb strings.Builder
		sb.WriteString(util.FormatHeader("AGIC Health Check"))
		sb.WriteString("\n\n")

		findings := 0

		// --- Find AGIC pod ---
		sb.WriteString(util.FormatSubHeader("AGIC Pod Status"))
		sb.WriteString("\n")

		// Search across all namespaces for AGIC pods
		agicPods, err := client.ListPods(ctx, "", metav1.ListOptions{
			LabelSelector: "app=ingress-azure",
		})
		if err != nil {
			return util.HandleK8sError("searching for AGIC pods", err), nil, nil
		}

		if len(agicPods) == 0 {
			sb.WriteString(util.FormatFinding("CRITICAL", "No AGIC pods found with label 'app=ingress-azure'"))
			sb.WriteString("\n")
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			sb.WriteString("1. Verify AGIC is installed in the cluster\n")
			sb.WriteString("2. Check if AGIC uses a different label selector\n")
			sb.WriteString("3. Try: list_pods with label_selector='app.kubernetes.io/name=ingress-azure'\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		podHeaders := []string{"POD", "NAMESPACE", "STATUS", "READY", "RESTARTS", "AGE", "NODE"}
		podRows := make([][]string, 0, len(agicPods))
		for i := range agicPods {
			p := &agicPods[i]
			ready, total, restarts := podContainerSummary(p)
			podRows = append(podRows, []string{
				p.Name,
				p.Namespace,
				podPhaseReason(p),
				fmt.Sprintf("%d/%d", ready, total),
				fmt.Sprintf("%d", restarts),
				util.FormatAge(p.CreationTimestamp.Time),
				p.Spec.NodeName,
			})
		}
		sb.WriteString(util.FormatTable(podHeaders, podRows))
		sb.WriteString("\n")

		// Analyze each AGIC pod
		for i := range agicPods {
			p := &agicPods[i]
			sb.WriteString(fmt.Sprintf("\n  Pod: %s/%s\n", p.Namespace, p.Name))

			// Check health
			if !isPodHealthy(p) {
				sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("AGIC pod '%s' is not healthy: %s", p.Name, podPhaseReason(p))))
				sb.WriteString("\n")
				findings++
			} else {
				sb.WriteString(util.FormatFinding("INFO", "Pod is running and healthy"))
				sb.WriteString("\n")
			}

			// Check restarts
			_, _, restarts := podContainerSummary(p)
			if restarts > util.HighRestartThreshold {
				sb.WriteString(util.FormatFinding("WARNING", fmt.Sprintf("AGIC pod has high restart count: %d", restarts)))
				sb.WriteString("\n")
				findings++
			} else if restarts > 0 {
				sb.WriteString(util.FormatFinding("INFO", fmt.Sprintf("Pod has restarted %d time(s)", restarts)))
				sb.WriteString("\n")
			}

			// Check container statuses for OOMKilled or CrashLoopBackOff
			for _, cs := range p.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated != nil {
					t := cs.LastTerminationState.Terminated
					if t.Reason == "OOMKilled" {
						sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Container '%s' was OOMKilled — consider increasing memory limits", cs.Name)))
						sb.WriteString("\n")
						findings++
					}
				}
				if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
					sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("Container '%s' is in CrashLoopBackOff", cs.Name)))
					sb.WriteString("\n")
					findings++
				}
			}

			// Get recent events
			events, evErr := client.GetEventsForObject(ctx, p.Namespace, p.Name)
			if evErr == nil {
				warningEvents := 0
				for _, e := range events {
					if e.Type == "Warning" {
						warningEvents++
					}
				}
				if warningEvents > 0 {
					sb.WriteString(fmt.Sprintf("  %d warning event(s):\n", warningEvents))
					for _, e := range events {
						if e.Type == "Warning" {
							msg := fmt.Sprintf("    %s: %s", e.Reason, e.Message)
							if e.Count > 1 {
								msg += fmt.Sprintf(" (x%d)", e.Count)
							}
							sb.WriteString(msg + "\n")
						}
					}
					findings++
				}
			}

			// Get recent logs and scan for errors (best-effort)
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent AGIC Logs (errors/warnings)"))
			sb.WriteString("\n")

			containerName := ""
			if len(p.Spec.Containers) > 0 {
				containerName = p.Spec.Containers[0].Name
			}

			logs, logErr := client.GetPodLogs(ctx, p.Namespace, p.Name, containerName, 200, false, "5m")
			if logErr != nil {
				sb.WriteString(fmt.Sprintf("  Could not fetch logs: %v\n", logErr))
			} else if logs == "" {
				sb.WriteString("  No recent logs available.\n")
			} else {
				// Scan logs for error/warning patterns
				errorLines := extractLogErrors(logs)
				if len(errorLines) > 0 {
					sb.WriteString(fmt.Sprintf("  Found %d error/warning lines in recent logs:\n", len(errorLines)))
					maxLines := 20
					if len(errorLines) < maxLines {
						maxLines = len(errorLines)
					}
					for _, line := range errorLines[:maxLines] {
						sb.WriteString(fmt.Sprintf("    %s\n", line))
					}
					if len(errorLines) > 20 {
						sb.WriteString(fmt.Sprintf("    ... and %d more error lines\n", len(errorLines)-20))
					}
					findings++
				} else {
					sb.WriteString("  No errors or warnings in recent logs.\n")
				}
			}
		}

		// --- Check AGIC ConfigMap ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("AGIC ConfigMap"))
		sb.WriteString("\n")

		// AGIC ConfigMap is typically in the same namespace as the AGIC pod
		agicNS := agicPods[0].Namespace
		configMapNames := []string{"ingress-azure", "agic-config"}
		configMapFound := false
		for _, cmName := range configMapNames {
			cm, cmErr := client.Clientset.CoreV1().ConfigMaps(agicNS).Get(ctx, cmName, metav1.GetOptions{})
			if cmErr != nil {
				continue
			}
			configMapFound = true
			sb.WriteString(fmt.Sprintf("  ConfigMap: %s/%s\n", agicNS, cmName))
			for key, value := range cm.Data {
				// Truncate long values
				displayVal := value
				if len(displayVal) > 100 {
					displayVal = displayVal[:100] + "..."
				}
				sb.WriteString(fmt.Sprintf("    %s: %s\n", key, displayVal))
			}
			break
		}
		if !configMapFound {
			sb.WriteString(util.FormatFinding("INFO", "No AGIC ConfigMap found (may be using Helm values or pod-identity)"))
			sb.WriteString("\n")
		}

		// --- Overall ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  AGIC appears healthy. No issues found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", findings))
		}

		sb.WriteString("\nSUGGESTED ACTIONS:\n")
		actionNum := 1
		for i := range agicPods {
			p := &agicPods[i]
			if !isPodHealthy(p) {
				sb.WriteString(fmt.Sprintf("%d. Investigate unhealthy AGIC pod '%s' (use diagnose_pod)\n", actionNum, p.Name))
				actionNum++
			}
			_, _, restarts := podContainerSummary(p)
			if restarts > util.HighRestartThreshold {
				sb.WriteString(fmt.Sprintf("%d. Check AGIC pod '%s' logs for crash cause (use get_pod_logs with previous=true)\n", actionNum, p.Name))
				actionNum++
			}
		}
		if actionNum == 1 {
			sb.WriteString("  No specific actions needed — AGIC is healthy.\n")
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// --- Helper functions ---
// NOTE: formatServicePorts is defined in composite_diagnostics.go and shared across tools.

// extractIngressDetails returns hosts, paths, and backend service names from an Ingress.
func extractIngressDetails(ing *networkingv1.Ingress) (hosts []string, paths []string, backends []string) {
	backendSet := make(map[string]bool)
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
		if rule.HTTP != nil {
			for _, p := range rule.HTTP.Paths {
				paths = append(paths, p.Path)
				if p.Backend.Service != nil && !backendSet[p.Backend.Service.Name] {
					backends = append(backends, p.Backend.Service.Name)
					backendSet[p.Backend.Service.Name] = true
				}
			}
		}
	}
	return
}

// ingressClassName returns the ingress class name from the Ingress spec or annotation.
func ingressClassName(ing *networkingv1.Ingress) string {
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName
	}
	if v, ok := ing.Annotations["kubernetes.io/ingress.class"]; ok {
		return v
	}
	return "<none>"
}

// hasIngress returns true if the ingress object is non-nil (used for the trace tool).
func hasIngress(ing *networkingv1.Ingress) bool {
	return ing != nil
}

// pathsOverlap checks whether two paths might conflict.
func pathsOverlap(a, b string) bool {
	// Same path is a definite overlap
	if a == b {
		return true
	}
	// One is a prefix of the other
	if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
		return true
	}
	return false
}

// extractLogErrors scans log output for lines containing error/warning keywords.
func extractLogErrors(logs string) []string {
	lines := strings.Split(logs, "\n")
	var errors []string
	keywords := []string{"error", "Error", "ERROR", "warn", "Warn", "WARN", "fatal", "Fatal", "FATAL", "panic", "Panic", "PANIC", "failed", "Failed", "FAILED"}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, kw := range keywords {
			if strings.Contains(line, kw) {
				errors = append(errors, trimmed)
				break
			}
		}
	}
	return errors
}
