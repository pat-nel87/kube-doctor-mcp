package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/mermaid"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type diagnoseRequestPathInput struct {
	Hostname  string `json:"hostname" jsonschema:"required,Hostname to trace (e.g. api.example.com)"`
	Path      string `json:"path,omitempty" jsonschema:"URL path to trace (e.g. /payments/v1/charge). Default: /"`
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace to search for Ingress (empty = all)"`
}

type diagnoseServiceInput struct {
	Namespace   string `json:"namespace" jsonschema:"required,Kubernetes namespace"`
	ServiceName string `json:"service_name" jsonschema:"required,Service name to diagnose"`
}

type clusterHealthOverviewInput struct{}

type analyzeServiceLogsInput struct {
	Namespace      string `json:"namespace" jsonschema:"required,Kubernetes namespace"`
	DeploymentName string `json:"deployment_name" jsonschema:"required,Deployment name"`
	Pattern        string `json:"pattern,omitempty" jsonschema:"Search pattern (regex). Default: error|exception|fatal|panic|timeout|refused"`
	TailLines      int64  `json:"tail_lines,omitempty" jsonschema:"Lines per pod (default 200)"`
}

func registerCompositeDiagnosticTools(server *mcp.Server, client *k8s.ClusterClient) {
	// diagnose_request_path — THE FLAGSHIP TOOL
	mcp.AddTool(server, &mcp.Tool{
		Name: "diagnose_request_path",
		Description: "Trace and diagnose the full request path from a hostname through Ingress → Service → Endpoints → Pods. " +
			"Checks health at every layer, validates AGIC/Ingress annotations, analyzes resource usage, " +
			"and generates Mermaid topology + sequence diagrams. THE PRIMARY tool for debugging why a URL is not working.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input diagnoseRequestPathInput) (*mcp.CallToolResult, any, error) {
		path := input.Path
		if path == "" {
			path = "/"
		}
		ns := input.Namespace
		if ns == "" {
			ns = "" // search all namespaces
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Request Path: https://%s%s", input.Hostname, path)))
		sb.WriteString("\n\n")
		findings := 0
		actions := []string{}

		// --- [1] FIND INGRESS ---
		ing, rule, matchedPath, err := client.FindIngressForHostPath(ctx, ns, input.Hostname, path)
		if err != nil {
			sb.WriteString(util.FormatFinding("CRITICAL", fmt.Sprintf("No Ingress found for %s%s", input.Hostname, path)))
			sb.WriteString("\n")
			sb.WriteString("  Searched all namespaces for matching Ingress host+path rules.\n")
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			sb.WriteString("1. Create an Ingress resource with host: " + input.Hostname + " and path: " + path + "\n")
			sb.WriteString("2. Use list_ingresses to see existing Ingress resources\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		sb.WriteString("[1] INGRESS\n")
		sb.WriteString(fmt.Sprintf("    Name: %s/%s\n", ing.Namespace, ing.Name))
		sb.WriteString(fmt.Sprintf("    Host: %s\n", rule.Host))
		sb.WriteString(fmt.Sprintf("    Path: %s\n", matchedPath.Path))
		if matchedPath.PathType != nil {
			sb.WriteString(fmt.Sprintf("    Path Type: %s\n", *matchedPath.PathType))
		}

		// IngressClass
		if ing.Spec.IngressClassName != nil {
			sb.WriteString(fmt.Sprintf("    Ingress Class: %s\n", *ing.Spec.IngressClassName))
		}

		// AGIC annotations
		agicAnnotations := k8s.ParseAGICAnnotations(ing)
		if len(agicAnnotations) > 0 {
			sb.WriteString("    AGIC Annotations:\n")
			for _, a := range agicAnnotations {
				sb.WriteString(fmt.Sprintf("      %s: %s\n", a.Key, a.Value))
			}
		}

		// TLS check
		hasTLS := false
		for _, tls := range ing.Spec.TLS {
			for _, h := range tls.Hosts {
				if h == input.Hostname {
					hasTLS = true
					sb.WriteString(fmt.Sprintf("    TLS: %s\n", tls.SecretName))
				}
			}
		}
		if !hasTLS {
			sb.WriteString(util.FormatFinding("WARNING", "No TLS configured for this host"))
			sb.WriteString("\n")
			findings++
			actions = append(actions, "Configure TLS for "+input.Hostname)
		}

		// Check Ingress events
		ingEvents, _ := client.GetEventsForObject(ctx, ing.Namespace, ing.Name)
		warningIngEvents := 0
		for _, e := range ingEvents {
			if e.Type == "Warning" {
				warningIngEvents++
			}
		}
		if warningIngEvents > 0 {
			sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d warning events on Ingress", warningIngEvents))))
			for _, e := range ingEvents {
				if e.Type == "Warning" {
					sb.WriteString(fmt.Sprintf("      - %s: %s\n", e.Reason, e.Message))
				}
			}
			findings++
		}
		sb.WriteString("\n")

		// --- [2] SERVICE ---
		backendSvcName := ""
		backendSvcPort := ""
		if matchedPath.Backend.Service != nil {
			backendSvcName = matchedPath.Backend.Service.Name
			if matchedPath.Backend.Service.Port.Name != "" {
				backendSvcPort = matchedPath.Backend.Service.Port.Name
			} else {
				backendSvcPort = fmt.Sprintf("%d", matchedPath.Backend.Service.Port.Number)
			}
		}

		if backendSvcName == "" {
			sb.WriteString(util.FormatFinding("CRITICAL", "No backend service configured in Ingress path"))
			sb.WriteString("\n")
			return util.SuccessResult(sb.String()), nil, nil
		}

		svc, err := client.GetService(ctx, ing.Namespace, backendSvcName)
		if err != nil {
			sb.WriteString("[2] SERVICE\n")
			sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("Backend service '%s' not found in namespace '%s'", backendSvcName, ing.Namespace))))
			findings++
			actions = append(actions, fmt.Sprintf("Create service '%s' in namespace '%s'", backendSvcName, ing.Namespace))
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			for i, a := range actions {
				sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, a))
			}
			return util.SuccessResult(sb.String()), nil, nil
		}

		sb.WriteString("[2] SERVICE\n")
		sb.WriteString(fmt.Sprintf("    Name: %s\n", svc.Name))
		sb.WriteString(fmt.Sprintf("    Type: %s\n", svc.Spec.Type))
		sb.WriteString(fmt.Sprintf("    ClusterIP: %s\n", svc.Spec.ClusterIP))
		sb.WriteString(fmt.Sprintf("    Port: %s → targetPort: %s\n", backendSvcPort, formatServicePorts(svc)))
		sb.WriteString(fmt.Sprintf("    Selector: %s\n", util.FormatLabels(svc.Spec.Selector)))

		// Check service events
		svcEvents, _ := client.GetEventsForObject(ctx, ing.Namespace, backendSvcName)
		warningSvcEvents := 0
		for _, e := range svcEvents {
			if e.Type == "Warning" {
				warningSvcEvents++
			}
		}
		if warningSvcEvents > 0 {
			sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d warning events on Service", warningSvcEvents))))
			findings++
		}
		sb.WriteString("\n")

		// --- [3] ENDPOINTS + PODS ---
		sb.WriteString("[3] ENDPOINTS\n")
		epHealth, err := client.GetServiceEndpointHealth(ctx, ing.Namespace, backendSvcName)
		if err != nil {
			sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", "Could not get endpoints: "+err.Error())))
			findings++
		} else {
			sb.WriteString(fmt.Sprintf("    Ready: %d/%d\n", epHealth.ReadyCount, epHealth.TotalEndpoints))

			if epHealth.TotalEndpoints == 0 {
				sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", "Service has 0 endpoints — no pods match the selector")))
				findings++
				actions = append(actions, fmt.Sprintf("Check that pods with labels %s exist in namespace %s", util.FormatLabels(svc.Spec.Selector), ing.Namespace))
			} else if epHealth.NotReadyCount > 0 {
				sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d endpoint(s) not ready", epHealth.NotReadyCount))))
				for _, nr := range epHealth.NotReadyPods {
					sb.WriteString(fmt.Sprintf("      - %s (%s)\n", nr.PodName, nr.IP))
				}
				findings++
			}
		}

		// Get backing pods with details
		pods, err := client.GetPodsForService(ctx, svc)
		if err == nil && len(pods) > 0 {
			sb.WriteString("\n    PODS:\n")
			headers := []string{"POD", "NODE", "READY", "RESTARTS", "STATUS", "AGE"}
			rows := make([][]string, 0, len(pods))
			for i := range pods {
				p := &pods[i]
				ready, total, restarts := podContainerSummary(p)
				rows = append(rows, []string{
					p.Name, p.Spec.NodeName,
					fmt.Sprintf("%d/%d", ready, total),
					fmt.Sprintf("%d", restarts),
					podPhaseReason(p),
					util.FormatAge(p.CreationTimestamp.Time),
				})
			}
			sb.WriteString("    ")
			sb.WriteString(strings.ReplaceAll(util.FormatTable(headers, rows), "\n", "\n    "))
			sb.WriteString("\n")

			// Check each pod's health
			for i := range pods {
				p := &pods[i]
				if !isPodHealthy(p) {
					sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("Pod '%s' is unhealthy: %s", p.Name, podPhaseReason(p)))))
					findings++
					actions = append(actions, fmt.Sprintf("Diagnose pod '%s' with diagnose_pod tool", p.Name))
				}
				_, _, restarts := podContainerSummary(p)
				if restarts > util.HighRestartThreshold {
					sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("Pod '%s' has %d restarts", p.Name, restarts))))
					findings++
				}

				// Check resource limits
				for _, c := range p.Spec.Containers {
					if c.Resources.Limits == nil || (c.Resources.Limits.Cpu().IsZero() && c.Resources.Limits.Memory().IsZero()) {
						sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("INFO", fmt.Sprintf("Pod '%s' container '%s' has no resource limits", p.Name, c.Name))))
						findings++
					}
				}

				// Check probe config
				for _, c := range p.Spec.Containers {
					if c.ReadinessProbe == nil {
						sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("Pod '%s' container '%s' has no readiness probe", p.Name, c.Name))))
						findings++
						actions = append(actions, fmt.Sprintf("Add readiness probe to container '%s'", c.Name))
					}
					if c.LivenessProbe == nil {
						sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("INFO", fmt.Sprintf("Pod '%s' container '%s' has no liveness probe", p.Name, c.Name))))
					}
				}
			}
		}

		// --- [4] RESOURCE USAGE ---
		sb.WriteString("\n[4] RESOURCE USAGE\n")
		podMetrics, metricsErr := client.GetPodMetrics(ctx, ing.Namespace, metav1.ListOptions{})
		if metricsErr != nil {
			sb.WriteString("    (metrics-server not available)\n")
		} else if len(pods) > 0 {
			podMetricsMap := make(map[string]map[string][2]int64) // pod -> container -> [cpu_milli, mem_bytes]
			for _, pm := range podMetrics {
				containers := make(map[string][2]int64)
				for _, cm := range pm.Containers {
					containers[cm.Name] = [2]int64{cm.Usage.Cpu().MilliValue(), cm.Usage.Memory().Value()}
				}
				podMetricsMap[pm.Name] = containers
			}

			for i := range pods {
				p := &pods[i]
				cm, ok := podMetricsMap[p.Name]
				if !ok {
					continue
				}
				for _, c := range p.Spec.Containers {
					usage, ok := cm[c.Name]
					if !ok {
						continue
					}
					cpuUsage := usage[0]
					memUsage := usage[1]
					cpuLimit := int64(0)
					memLimit := int64(0)
					if c.Resources.Limits != nil {
						cpuLimit = c.Resources.Limits.Cpu().MilliValue()
						memLimit = c.Resources.Limits.Memory().Value()
					}

					cpuPct := "N/A"
					memPct := "N/A"
					if cpuLimit > 0 {
						pct := float64(cpuUsage) / float64(cpuLimit) * 100
						cpuPct = fmt.Sprintf("%.0f%%", pct)
						if pct >= 90 {
							sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%s/%s: CPU at %.0f%% of limit", p.Name, c.Name, pct))))
							findings++
						} else if pct >= 70 {
							sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%s/%s: CPU at %.0f%% of limit", p.Name, c.Name, pct))))
							findings++
						}
					}
					if memLimit > 0 {
						pct := float64(memUsage) / float64(memLimit) * 100
						memPct = fmt.Sprintf("%.0f%%", pct)
						if pct >= 90 {
							sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%s/%s: Memory at %.0f%% of limit — OOM risk", p.Name, c.Name, pct))))
							findings++
							actions = append(actions, fmt.Sprintf("Increase memory limit for %s/%s", p.Name, c.Name))
						} else if pct >= 70 {
							sb.WriteString(fmt.Sprintf("    %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%s/%s: Memory at %.0f%% of limit", p.Name, c.Name, pct))))
							findings++
						}
					}
					sb.WriteString(fmt.Sprintf("    %s/%s: CPU %dm/%dm (%s)  Mem %s/%s (%s)\n",
						p.Name, c.Name,
						cpuUsage, cpuLimit, cpuPct,
						formatBytes(memUsage), formatBytes(memLimit), memPct))
				}
			}
		}

		// --- SUMMARY ---
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Summary"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Request path appears healthy. All layers operational.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d finding(s) across the request path.\n", findings))
		}

		// --- SUGGESTED ACTIONS ---
		if len(actions) > 0 {
			sb.WriteString("\nSUGGESTED ACTIONS:\n")
			seen := make(map[string]bool)
			num := 1
			for _, a := range actions {
				if !seen[a] {
					sb.WriteString(fmt.Sprintf("%d. %s\n", num, a))
					seen[a] = true
					num++
				}
			}
		}

		// --- MERMAID TOPOLOGY DIAGRAM ---
		sb.WriteString("\nTOPOLOGY:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionTB)
		fc.AddNode("internet", "Internet", mermaid.ShapeCircle)
		fc.AddNode("agw", fmt.Sprintf("Ingress: %s%s%s: %s  Path: %s", ing.Name, mermaid.BR(), mermaid.BR(), rule.Host, matchedPath.Path), mermaid.ShapeTrapAlt)
		fc.AddNode("svc", fmt.Sprintf("Service: %s%sClusterIP:%s", svc.Name, mermaid.BR(), backendSvcPort), mermaid.ShapeRect)

		fc.AddEdge("internet", "agw", "HTTPS", mermaid.EdgeSolid)
		fc.AddEdge("agw", "svc", "", mermaid.EdgeSolid)
		fc.AddRawStyle("agw", "fill:#cce5ff,stroke:#4a90d9,stroke-width:2px")

		for i := range pods {
			p := &pods[i]
			podID := mermaid.SafeID("pod_" + p.Name)
			label := p.Name
			if len(label) > 30 {
				label = label[:30] + "..."
			}
			healthy := isPodHealthy(p)
			fc.AddNode(podID, label, mermaid.ShapeRound)
			fc.AddEdge("svc", podID, "", mermaid.EdgeSolid)
			if !healthy {
				fc.AddStyle(podID, mermaid.SeverityCritical)
			} else {
				fc.AddStyle(podID, mermaid.SeverityHealthy)
			}
		}
		sb.WriteString(fc.RenderBlock())

		// --- MERMAID SEQUENCE DIAGRAM ---
		sb.WriteString("\n\nREQUEST FLOW:\n")
		seq := mermaid.NewSequence()
		seq.AddParticipant("client", "Client")
		seq.AddParticipant("ing", fmt.Sprintf("Ingress: %s", ing.Name))
		seq.AddParticipant("svc", fmt.Sprintf("Service: %s", svc.Name))

		if len(pods) > 0 {
			p := &pods[0]
			podLabel := p.Name
			if len(podLabel) > 25 {
				podLabel = podLabel[:25] + "..."
			}
			seq.AddParticipant("pod", fmt.Sprintf("Pod: %s", podLabel))

			seq.AddMessage("client", "ing", fmt.Sprintf("HTTPS %s %s", "GET", path), mermaid.MsgSolid)

			// Ingress note
			ingNotes := []string{"Route matching"}
			if hasTLS {
				ingNotes = append(ingNotes, "TLS termination")
			}
			for _, a := range agicAnnotations {
				if a.Key == "request-timeout" {
					ingNotes = append(ingNotes, "Timeout: "+a.Value+"s")
				}
				if a.Key == "backend-protocol" {
					ingNotes = append(ingNotes, "Backend: "+a.Value)
				}
			}
			seq.AddNote("ing", strings.Join(ingNotes, mermaid.BR()), mermaid.NoteOver)

			seq.AddMessage("ing", "svc", fmt.Sprintf("Forward to %s", backendSvcPort), mermaid.MsgSolid)
			svcNote := fmt.Sprintf("%d/%d endpoints ready", 0, 0)
			if epHealth != nil {
				svcNote = fmt.Sprintf("%d/%d endpoints ready", epHealth.ReadyCount, epHealth.TotalEndpoints)
			}
			seq.AddNote("svc", svcNote, mermaid.NoteOver)

			seq.AddMessage("svc", "pod", "Forward to pod", mermaid.MsgSolid)
			seq.AddMessage("pod", "svc", "Response", mermaid.MsgDotted)
			seq.AddMessage("svc", "ing", "Response", mermaid.MsgDotted)
			seq.AddMessage("ing", "client", "Response", mermaid.MsgDotted)
		}

		sb.WriteString(seq.RenderBlock())

		return util.SuccessResult(sb.String()), nil, nil
	})

	// diagnose_service — comprehensive service diagnosis
	mcp.AddTool(server, &mcp.Tool{
		Name: "diagnose_service",
		Description: "Everything about a single Kubernetes service: endpoint health, backing pod status, resource usage, " +
			"Ingress exposure, network policies, events, and Mermaid dependency diagram. " +
			"Use this as the primary tool for investigating service-level issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input diagnoseServiceInput) (*mcp.CallToolResult, any, error) {
		svc, err := client.GetService(ctx, input.Namespace, input.ServiceName)
		if err != nil {
			return util.HandleK8sError(fmt.Sprintf("getting service %s/%s", input.Namespace, input.ServiceName), err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Service Diagnosis: %s (namespace: %s)", svc.Name, svc.Namespace)))
		sb.WriteString("\n\n")
		findings := 0

		// 1. Service spec
		sb.WriteString(util.FormatSubHeader("Service Configuration"))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  Type: %s\n", svc.Spec.Type))
		sb.WriteString(fmt.Sprintf("  ClusterIP: %s\n", svc.Spec.ClusterIP))
		sb.WriteString(fmt.Sprintf("  Ports: %s\n", formatServicePorts(svc)))
		sb.WriteString(fmt.Sprintf("  Selector: %s\n", util.FormatLabels(svc.Spec.Selector)))
		sb.WriteString(fmt.Sprintf("  Session Affinity: %s\n", svc.Spec.SessionAffinity))
		sb.WriteString(fmt.Sprintf("  Age: %s\n", util.FormatAge(svc.CreationTimestamp.Time)))

		// 2. Endpoint health
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Endpoint Health"))
		sb.WriteString("\n")
		epHealth, err := client.GetServiceEndpointHealth(ctx, input.Namespace, input.ServiceName)
		if err != nil {
			sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", "Could not get endpoints: "+err.Error())))
			findings++
		} else {
			sb.WriteString(fmt.Sprintf("  Total: %d, Ready: %d, NotReady: %d\n", epHealth.TotalEndpoints, epHealth.ReadyCount, epHealth.NotReadyCount))
			if epHealth.TotalEndpoints == 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", "Service has 0 endpoints — no pods match the selector")))
				findings++
			} else if epHealth.NotReadyCount > 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d endpoint(s) not ready", epHealth.NotReadyCount))))
				findings++
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("OK", "All endpoints ready")))
			}
		}

		// 3. Backing pods
		pods, err := client.GetPodsForService(ctx, svc)
		if err == nil && len(pods) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Backing Pods"))
			sb.WriteString("\n")

			headers := []string{"POD", "STATUS", "READY", "RESTARTS", "NODE", "AGE"}
			rows := make([][]string, 0, len(pods))
			for i := range pods {
				p := &pods[i]
				ready, total, restarts := podContainerSummary(p)
				rows = append(rows, []string{
					p.Name, podPhaseReason(p),
					fmt.Sprintf("%d/%d", ready, total),
					fmt.Sprintf("%d", restarts),
					p.Spec.NodeName,
					util.FormatAge(p.CreationTimestamp.Time),
				})
				if !isPodHealthy(p) {
					findings++
				}
			}
			sb.WriteString(util.FormatTable(headers, rows))

			unhealthyPods := 0
			for i := range pods {
				if !isPodHealthy(&pods[i]) {
					unhealthyPods++
				}
			}
			if unhealthyPods > 0 {
				sb.WriteString(fmt.Sprintf("\n  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%d/%d pods are unhealthy", unhealthyPods, len(pods)))))
			}
		}

		// 4. Resource usage
		podMetrics, metricsErr := client.GetPodMetrics(ctx, input.Namespace, metav1.ListOptions{})
		if metricsErr == nil && len(pods) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Resource Usage"))
			sb.WriteString("\n")
			pmMap := make(map[string]map[string][2]int64)
			for _, pm := range podMetrics {
				containers := make(map[string][2]int64)
				for _, cm := range pm.Containers {
					containers[cm.Name] = [2]int64{cm.Usage.Cpu().MilliValue(), cm.Usage.Memory().Value()}
				}
				pmMap[pm.Name] = containers
			}
			for i := range pods {
				p := &pods[i]
				cm, ok := pmMap[p.Name]
				if !ok {
					continue
				}
				for _, c := range p.Spec.Containers {
					usage, ok := cm[c.Name]
					if !ok {
						continue
					}
					cpuLimitStr := "no limit"
					memLimitStr := "no limit"
					if c.Resources.Limits != nil {
						if !c.Resources.Limits.Cpu().IsZero() {
							cpuLimitStr = fmt.Sprintf("%dm", c.Resources.Limits.Cpu().MilliValue())
						}
						if !c.Resources.Limits.Memory().IsZero() {
							memLimitStr = formatBytes(c.Resources.Limits.Memory().Value())
						}
					}
					sb.WriteString(fmt.Sprintf("  %s/%s: CPU %dm/%s  Mem %s/%s\n",
						p.Name, c.Name, usage[0], cpuLimitStr, formatBytes(usage[1]), memLimitStr))
				}
			}
		}

		// 5. Ingress exposure
		ingresses, err := client.ListIngresses(ctx, input.Namespace, metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Ingress Exposure"))
			sb.WriteString("\n")
			exposedVia := 0
			for _, ing := range ingresses {
				for _, rule := range ing.Spec.Rules {
					if rule.HTTP == nil {
						continue
					}
					for _, p := range rule.HTTP.Paths {
						if p.Backend.Service != nil && p.Backend.Service.Name == input.ServiceName {
							sb.WriteString(fmt.Sprintf("  Ingress '%s': %s%s\n", ing.Name, rule.Host, p.Path))
							exposedVia++
						}
					}
				}
			}
			if exposedVia == 0 {
				sb.WriteString("  Not exposed via any Ingress\n")
			}
		}

		// 6. Network policies
		netPols, err := client.ListNetworkPolicies(ctx, input.Namespace, metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Network Policies"))
			sb.WriteString("\n")
			matchingPols := 0
			for _, np := range netPols {
				if matchesSvcSelector(np.Spec.PodSelector.MatchLabels, svc.Spec.Selector) {
					sb.WriteString(fmt.Sprintf("  Policy '%s' applies to this service's pods\n", np.Name))
					matchingPols++
				}
			}
			if matchingPols == 0 && len(netPols) > 0 {
				sb.WriteString("  No network policies target this service's pods (all traffic allowed)\n")
			} else if len(netPols) == 0 {
				sb.WriteString("  No network policies in namespace (all traffic allowed)\n")
			}
		}

		// 7. Recent events
		events, _ := client.GetEventsForObject(ctx, input.Namespace, input.ServiceName)
		if len(events) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent Events"))
			sb.WriteString("\n")
			for _, e := range events {
				if len(events) > 10 {
					break
				}
				marker := "  "
				if e.Type == "Warning" {
					marker = "  [WARNING] "
				}
				sb.WriteString(fmt.Sprintf("%s%s: %s", marker, e.Reason, e.Message))
				if e.Count > 1 {
					sb.WriteString(fmt.Sprintf(" (x%d)", e.Count))
				}
				sb.WriteString("\n")
			}
		}

		// 8. Overall
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Service appears healthy. All endpoints ready, pods running.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d finding(s) identified. Review details above.\n", findings))
		}

		// 9. Mermaid diagram
		sb.WriteString("\nSERVICE CONTEXT:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionLR)
		svcID := mermaid.SafeID("svc_" + svc.Name)
		fc.AddNode(svcID, fmt.Sprintf("Service: %s%s%s", svc.Name, mermaid.BR(), formatServicePorts(svc)), mermaid.ShapeRect)
		fc.AddRawStyle(svcID, "fill:#cce5ff,stroke:#4a90d9,stroke-width:2px")

		for i := range pods {
			p := &pods[i]
			podID := mermaid.SafeID("pod_" + p.Name)
			podLabel := p.Name
			if len(podLabel) > 25 {
				podLabel = podLabel[:25] + "..."
			}
			fc.AddNode(podID, podLabel, mermaid.ShapeRound)
			fc.AddEdge(svcID, podID, "", mermaid.EdgeSolid)
			if isPodHealthy(p) {
				fc.AddStyle(podID, mermaid.SeverityHealthy)
			} else {
				fc.AddStyle(podID, mermaid.SeverityCritical)
			}
		}
		sb.WriteString(fc.RenderBlock())

		return util.SuccessResult(sb.String()), nil, nil
	})

	// cluster_health_overview — enhanced cluster dashboard
	mcp.AddTool(server, &mcp.Tool{
		Name: "cluster_health_overview",
		Description: "Comprehensive cluster health dashboard with node status, pod health by namespace, service endpoint health, " +
			"Ingress audit, resource utilization, top consumers, events, and Mermaid cluster topology diagram. " +
			"Use this for a complete picture of cluster health in one call.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input clusterHealthOverviewInput) (*mcp.CallToolResult, any, error) {
		var sb strings.Builder
		sb.WriteString(util.FormatHeader("Cluster Health Overview"))
		sb.WriteString("\n\n")
		findings := 0

		// 1. Node health
		nodes, err := client.ListNodes(ctx, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing nodes", err), nil, nil
		}

		sb.WriteString(util.FormatSubHeader("Nodes"))
		sb.WriteString("\n")
		readyNodes := 0
		for _, n := range nodes {
			status := nodeStatus(&n)
			if status == "Ready" {
				readyNodes++
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("Node '%s' is %s", n.Name, status))))
				findings++
			}
			for _, cond := range n.Status.Conditions {
				if (cond.Type == corev1.NodeMemoryPressure || cond.Type == corev1.NodeDiskPressure || cond.Type == corev1.NodePIDPressure) && cond.Status == corev1.ConditionTrue {
					sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("Node '%s' has %s", n.Name, cond.Type))))
					findings++
				}
			}
		}
		sb.WriteString(fmt.Sprintf("  %d/%d nodes ready\n", readyNodes, len(nodes)))

		// 2. Resource utilization
		nodeMetrics, metricsErr := client.GetNodeMetrics(ctx)
		if metricsErr == nil && len(nodeMetrics) > 0 {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Resource Utilization"))
			sb.WriteString("\n")
			var totalCPU, totalMem, capCPU, capMem int64
			for _, n := range nodes {
				capCPU += n.Status.Capacity.Cpu().MilliValue()
				capMem += n.Status.Capacity.Memory().Value()
			}
			for _, m := range nodeMetrics {
				totalCPU += m.Usage.Cpu().MilliValue()
				totalMem += m.Usage.Memory().Value()
			}
			cpuPct := float64(0)
			memPct := float64(0)
			if capCPU > 0 {
				cpuPct = float64(totalCPU) / float64(capCPU) * 100
			}
			if capMem > 0 {
				memPct = float64(totalMem) / float64(capMem) * 100
			}
			sb.WriteString(fmt.Sprintf("  CPU:    %dm / %dm (%.1f%%)\n", totalCPU, capCPU, cpuPct))
			sb.WriteString(fmt.Sprintf("  Memory: %s / %s (%.1f%%)\n", formatBytes(totalMem), formatBytes(capMem), memPct))
			if cpuPct > 85 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", "Cluster CPU utilization above 85%")))
				findings++
			}
			if memPct > 85 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", "Cluster memory utilization above 85%")))
				findings++
			}
		}

		// 3. Pod health by namespace
		allPods, err := client.ListPods(ctx, "", metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Pod Health by Namespace"))
			sb.WriteString("\n")
			nsPods := make(map[string]struct{ total, unhealthy, highRestart int })
			for i := range allPods {
				p := &allPods[i]
				ns := p.Namespace
				entry := nsPods[ns]
				entry.total++
				if !isPodHealthy(p) {
					entry.unhealthy++
				}
				_, _, restarts := podContainerSummary(p)
				if restarts > util.HighRestartThreshold {
					entry.highRestart++
				}
				nsPods[ns] = entry
			}

			headers := []string{"NAMESPACE", "TOTAL", "UNHEALTHY", "HIGH RESTARTS"}
			rows := make([][]string, 0)
			for ns, e := range nsPods {
				if e.unhealthy > 0 || e.highRestart > 0 {
					rows = append(rows, []string{ns, fmt.Sprintf("%d", e.total), fmt.Sprintf("%d", e.unhealthy), fmt.Sprintf("%d", e.highRestart)})
				}
			}
			if len(rows) > 0 {
				// Sort by unhealthy count desc
				sort.Slice(rows, func(i, j int) bool { return rows[i][2] > rows[j][2] })
				sb.WriteString(util.FormatTable(headers, rows))
				sb.WriteString("\n")
				totalUnhealthy := 0
				for _, e := range nsPods {
					totalUnhealthy += e.unhealthy
				}
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%d unhealthy pods cluster-wide", totalUnhealthy))))
				findings++
			} else {
				sb.WriteString(fmt.Sprintf("  All %d pods healthy across %d namespaces\n", len(allPods), len(nsPods)))
			}
		}

		// 4. Service endpoint health
		services, err := client.ListServices(ctx, "", metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Service Endpoint Health"))
			sb.WriteString("\n")
			deadServices := 0
			degradedServices := 0
			for _, svc := range services {
				if svc.Spec.Type == corev1.ServiceTypeExternalName {
					continue
				}
				if svc.Spec.Selector == nil || len(svc.Spec.Selector) == 0 {
					continue
				}
				epHealth, err := client.GetServiceEndpointHealth(ctx, svc.Namespace, svc.Name)
				if err != nil {
					continue
				}
				if epHealth.TotalEndpoints == 0 {
					sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%s/%s: 0 endpoints (DEAD)", svc.Namespace, svc.Name))))
					deadServices++
					findings++
				} else if epHealth.NotReadyCount > 0 {
					sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("WARNING", fmt.Sprintf("%s/%s: %d/%d not ready (DEGRADED)", svc.Namespace, svc.Name, epHealth.NotReadyCount, epHealth.TotalEndpoints))))
					degradedServices++
					findings++
				}
			}
			if deadServices == 0 && degradedServices == 0 {
				sb.WriteString(fmt.Sprintf("  All %d services with selectors have healthy endpoints\n", len(services)))
			}
		}

		// 5. Warning events (last hour)
		events, err := client.ListEvents(ctx, "", metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("Recent Warnings (last hour)"))
			sb.WriteString("\n")
			oneHourAgo := time.Now().Add(-1 * time.Hour)
			warningCount := 0
			reasonCounts := make(map[string]int)
			for _, e := range events {
				t := e.LastTimestamp.Time
				if t.IsZero() {
					t = e.CreationTimestamp.Time
				}
				if e.Type == "Warning" && t.After(oneHourAgo) {
					warningCount++
					reasonCounts[e.Reason]++
				}
			}
			if warningCount > 0 {
				sb.WriteString(fmt.Sprintf("  %d warning events\n", warningCount))
				for reason, count := range reasonCounts {
					sb.WriteString(fmt.Sprintf("    %s: %d\n", reason, count))
				}
				findings++
			} else {
				sb.WriteString("  No warning events in the last hour\n")
			}
		}

		// 6. kube-system check
		ksPods, err := client.ListPods(ctx, "kube-system", metav1.ListOptions{})
		if err == nil {
			sb.WriteString("\n")
			sb.WriteString(util.FormatSubHeader("kube-system Health"))
			sb.WriteString("\n")
			ksUnhealthy := 0
			for i := range ksPods {
				if !isPodHealthy(&ksPods[i]) {
					ksUnhealthy++
				}
			}
			if ksUnhealthy > 0 {
				sb.WriteString(fmt.Sprintf("  %s\n", util.FormatFinding("CRITICAL", fmt.Sprintf("%d/%d unhealthy", ksUnhealthy, len(ksPods)))))
				findings++
			} else {
				sb.WriteString(fmt.Sprintf("  All %d pods healthy\n", len(ksPods)))
			}
		}

		// 7. Overall
		sb.WriteString("\n")
		sb.WriteString(util.FormatSubHeader("Overall Assessment"))
		sb.WriteString("\n")
		if findings == 0 {
			sb.WriteString("  Cluster is healthy. No issues found.\n")
		} else {
			sb.WriteString(fmt.Sprintf("  %d issue(s) found. Review findings above.\n", findings))
		}

		// 8. Mermaid cluster topology
		sb.WriteString("\nCLUSTER TOPOLOGY:\n")
		fc := mermaid.NewFlowchart(mermaid.DirectionTB)
		fc.AddSubgraph("cluster", "AKS Cluster", func(sg *mermaid.Subgraph) {
			for i, n := range nodes {
				nodeID := mermaid.SafeID(fmt.Sprintf("node_%d", i))
				status := nodeStatus(&n)
				sg.AddNode(nodeID, fmt.Sprintf("%s%s%s", n.Name, mermaid.BR(), status), mermaid.ShapeRect)
			}
		})

		// Add ingress layer if present
		ingresses, err := client.ListIngresses(ctx, "", metav1.ListOptions{})
		if err == nil && len(ingresses) > 0 {
			fc.AddNode("internet", "Internet", mermaid.ShapeCircle)
			for i, ing := range ingresses {
				if i >= 5 {
					break // limit diagram size
				}
				ingID := mermaid.SafeID(fmt.Sprintf("ing_%d", i))
				host := ""
				for _, r := range ing.Spec.Rules {
					host = r.Host
					break
				}
				fc.AddNode(ingID, fmt.Sprintf("Ingress: %s%s%s", ing.Name, mermaid.BR(), host), mermaid.ShapeTrapAlt)
				fc.AddEdge("internet", ingID, "", mermaid.EdgeSolid)
			}
		}

		sb.WriteString(fc.RenderBlock())

		return util.SuccessResult(sb.String()), nil, nil
	})

	// analyze_service_logs — search pod logs for error patterns
	mcp.AddTool(server, &mcp.Tool{
		Name: "analyze_service_logs",
		Description: "Search pod logs for a deployment for error patterns (errors, exceptions, timeouts, stack traces). " +
			"Aggregates error counts by type across all pods. Use this when investigating application-level issues.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input analyzeServiceLogsInput) (*mcp.CallToolResult, any, error) {
		// Find pods for the deployment
		deployments, err := client.ListDeployments(ctx, input.Namespace, metav1.ListOptions{})
		if err != nil {
			return util.HandleK8sError("listing deployments", err), nil, nil
		}

		var matchLabels map[string]string
		for _, d := range deployments {
			if d.Name == input.DeploymentName {
				matchLabels = d.Spec.Selector.MatchLabels
				break
			}
		}
		if matchLabels == nil {
			return util.ErrorResult("Deployment '%s' not found in namespace '%s'", input.DeploymentName, input.Namespace), nil, nil
		}

		// Build label selector
		var parts []string
		for k, v := range matchLabels {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector := strings.Join(parts, ",")

		pods, err := client.ListPods(ctx, input.Namespace, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return util.HandleK8sError("listing pods", err), nil, nil
		}

		tailLines := input.TailLines
		if tailLines <= 0 {
			tailLines = 200
		}

		pattern := input.Pattern
		if pattern == "" {
			pattern = `(?i)(error|exception|fatal|panic|timeout|refused|failed|crash|oom)`
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return util.ErrorResult("Invalid pattern: %v", err), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Service Log Analysis: %s (namespace: %s)", input.DeploymentName, input.Namespace)))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("Pattern: %s\n", pattern))
		sb.WriteString(fmt.Sprintf("Pods: %d, Lines per pod: %d\n\n", len(pods), tailLines))

		totalMatches := 0
		errorCounts := make(map[string]int)
		podMatches := make(map[string]int)

		for i := range pods {
			p := &pods[i]
			for _, c := range p.Spec.Containers {
				logs, err := client.GetPodLogs(ctx, input.Namespace, p.Name, c.Name, tailLines, false, "")
				if err != nil {
					sb.WriteString(fmt.Sprintf("[%s/%s] Could not get logs: %v\n", p.Name, c.Name, err))
					continue
				}
				lines := strings.Split(logs, "\n")
				for _, line := range lines {
					if re.MatchString(line) {
						totalMatches++
						podMatches[p.Name]++
						// Categorize by matched word
						matches := re.FindStringSubmatch(line)
						if len(matches) > 0 {
							errorCounts[strings.ToLower(matches[0])]++
						}
					}
				}
			}
		}

		if totalMatches == 0 {
			sb.WriteString("No matching log entries found.\n")
		} else {
			sb.WriteString(util.FormatSubHeader(fmt.Sprintf("Found %d matching entries", totalMatches)))
			sb.WriteString("\n\n")

			// Error type breakdown
			sb.WriteString("Error Type Breakdown:\n")
			type errEntry struct {
				name  string
				count int
			}
			entries := make([]errEntry, 0, len(errorCounts))
			for name, count := range errorCounts {
				entries = append(entries, errEntry{name, count})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
			for _, e := range entries {
				sb.WriteString(fmt.Sprintf("  %-20s %d\n", e.name, e.count))
			}

			// Per-pod breakdown
			sb.WriteString("\nPer-Pod Breakdown:\n")
			for pod, count := range podMatches {
				sb.WriteString(fmt.Sprintf("  %-40s %d matches\n", pod, count))
			}

			// Sample lines
			sb.WriteString("\nSample Matching Lines (first 5 per pod):\n")
			for i := range pods {
				p := &pods[i]
				shown := 0
				for _, c := range p.Spec.Containers {
					if shown >= 5 {
						break
					}
					logs, err := client.GetPodLogs(ctx, input.Namespace, p.Name, c.Name, tailLines, false, "")
					if err != nil {
						continue
					}
					for _, line := range strings.Split(logs, "\n") {
						if shown >= 5 {
							break
						}
						if re.MatchString(line) {
							truncated := line
							if len(truncated) > 200 {
								truncated = truncated[:200] + "..."
							}
							sb.WriteString(fmt.Sprintf("  [%s] %s\n", p.Name, truncated))
							shown++
						}
					}
				}
			}
		}

		return util.SuccessResult(sb.String()), nil, nil
	})
}

// formatServicePorts returns a summary of service ports.
func formatServicePorts(svc *corev1.Service) string {
	if len(svc.Spec.Ports) == 0 {
		return "<none>"
	}
	var parts []string
	for _, p := range svc.Spec.Ports {
		target := p.TargetPort.String()
		if target == "0" || target == "" {
			target = fmt.Sprintf("%d", p.Port)
		}
		entry := fmt.Sprintf("%d→%s", p.Port, target)
		if p.Name != "" {
			entry = fmt.Sprintf("%s(%s)", p.Name, entry)
		}
		if p.Protocol != corev1.ProtocolTCP {
			entry += "/" + string(p.Protocol)
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, ", ")
}

// matchesSvcSelector checks if a NetworkPolicy's pod selector would match a service's selector.
func matchesSvcSelector(policyLabels, svcLabels map[string]string) bool {
	if len(policyLabels) == 0 {
		return true // empty selector matches all
	}
	for k, v := range policyLabels {
		if svcLabels[k] != v {
			return false
		}
	}
	return true
}
