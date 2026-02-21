import * as vscode from 'vscode';
import * as k8s from '@kubernetes/client-node';
import { listServices, listIngresses, getEndpoints } from '../k8s/networking';
import { listPods, isPodHealthy, podPhaseReason, podContainerSummary } from '../k8s/pods';
import { getEventsForObject } from '../k8s/events';
import { getService, formatServicePorts, podMatchesSelector, findIngressForHostPath, parseAGICAnnotations, mermaidSafeId } from '../k8s/network_analysis';
import { listNetworkPolicies } from '../k8s/network_policies';
import { getPodLogs } from '../k8s/logs';
import { formatAge, formatTable, formatError } from '../util/formatting';

// ---- map_service_topology ----

interface MapServiceTopologyInput {
    namespace: string;
}

export class MapServiceTopologyTool implements vscode.LanguageModelTool<MapServiceTopologyInput> {
    async prepareInvocation(
        options: vscode.LanguageModelToolInvocationPrepareOptions<MapServiceTopologyInput>
    ): Promise<vscode.PreparedToolInvocation> {
        const ns = options.input.namespace || 'all namespaces';
        return { invocationMessage: `Mapping service topology in ${ns}...` };
    }

    async invoke(
        options: vscode.LanguageModelToolInvocationOptions<MapServiceTopologyInput>
    ): Promise<vscode.LanguageModelToolResult> {
        try {
            const { namespace } = options.input;
            const services = await listServices(namespace);
            const pods = await listPods(namespace);
            const ingresses = await listIngresses(namespace);

            const lines: string[] = [];
            const displayNs = !namespace || namespace === 'all' ? 'all' : namespace;
            lines.push(`=== Service Topology (namespace: ${displayNs}) ===`);
            lines.push('');
            lines.push(`Services: ${services.length}, Ingresses: ${ingresses.length}, Pods: ${pods.length}`);
            lines.push('');

            // Build service -> pods mapping
            const svcPodMap: Map<string, { svc: k8s.V1Service; pods: k8s.V1Pod[]; endpoints: { ready: number; notReady: number } }> = new Map();

            for (const svc of services) {
                const svcName = svc.metadata?.name || '';
                const svcNs = svc.metadata?.namespace || '';
                const selector = svc.spec?.selector || {};
                const backingPods = Object.keys(selector).length > 0
                    ? pods.filter(p => p.metadata?.namespace === svcNs && podMatchesSelector(p, selector))
                    : [];

                let readyCount = 0;
                let notReadyCount = 0;
                try {
                    const ep = await getEndpoints(svcNs, svcName);
                    for (const subset of ep.subsets || []) {
                        readyCount += (subset.addresses || []).length;
                        notReadyCount += (subset.notReadyAddresses || []).length;
                    }
                } catch { /* endpoints may not exist */ }

                svcPodMap.set(`${svcNs}/${svcName}`, {
                    svc,
                    pods: backingPods,
                    endpoints: { ready: readyCount, notReady: notReadyCount },
                });
            }

            // Build ingress -> service mapping
            const ingSvcMap: Map<string, { ing: k8s.V1Ingress; backends: string[] }> = new Map();
            for (const ing of ingresses) {
                const ingName = ing.metadata?.name || '';
                const ingNs = ing.metadata?.namespace || '';
                const backends: string[] = [];
                for (const rule of ing.spec?.rules || []) {
                    for (const path of rule.http?.paths || []) {
                        const svcName = path.backend?.service?.name;
                        if (svcName) {
                            backends.push(`${ingNs}/${svcName}`);
                        }
                    }
                }
                ingSvcMap.set(`${ingNs}/${ingName}`, { ing, backends });
            }

            // Text summary per service
            lines.push('--- Services ---');
            for (const [key, entry] of svcPodMap) {
                const svc = entry.svc;
                const svcType = svc.spec?.type || 'ClusterIP';
                const ports = formatServicePorts(svc);
                const healthy = entry.pods.filter(p => isPodHealthy(p)).length;
                lines.push(`  ${svc.metadata?.name} (${svcType}, ${ports})`);
                lines.push(`    Endpoints: ${entry.endpoints.ready} ready, ${entry.endpoints.notReady} not-ready`);
                lines.push(`    Backing Pods: ${entry.pods.length} (${healthy} healthy)`);
            }

            if (ingresses.length > 0) {
                lines.push('');
                lines.push('--- Ingresses ---');
                for (const ing of ingresses) {
                    const hosts = (ing.spec?.rules || []).map(r => r.host || '*').join(', ');
                    const tls = (ing.spec?.tls?.length || 0) > 0 ? 'TLS' : 'No TLS';
                    lines.push(`  ${ing.metadata?.name} (${hosts}) [${tls}]`);
                }
            }

            // Mermaid flowchart
            lines.push('');
            lines.push('TOPOLOGY DIAGRAM:');
            lines.push('```mermaid');
            lines.push('graph TD');
            lines.push('    Internet((Internet))');

            const nsName = displayNs === 'all' ? 'cluster' : displayNs;
            lines.push(`    subgraph ns[Namespace: ${nsName}]`);

            for (const [key, entry] of svcPodMap) {
                const svc = entry.svc;
                const svcName = svc.metadata?.name || '';
                const svcType = svc.spec?.type || 'ClusterIP';
                const ports = formatServicePorts(svc);
                const svcId = mermaidSafeId(`svc_${svcName}`);
                lines.push(`        ${svcId}[fa:fa-cog ${svcName}<br>${svcType}<br>${ports}]`);

                for (const pod of entry.pods) {
                    const podName = pod.metadata?.name || '';
                    const podStatus = podPhaseReason(pod);
                    const podId = mermaidSafeId(`pod_${podName}`);
                    lines.push(`        ${podId}[fa:fa-cube ${podName}<br>${podStatus}]`);
                    lines.push(`        ${svcId} --> ${podId}`);
                }
            }

            lines.push('    end');

            // Ingress nodes outside the namespace subgraph
            for (const [key, entry] of ingSvcMap) {
                const ing = entry.ing;
                const ingName = ing.metadata?.name || '';
                const hosts = (ing.spec?.rules || []).map(r => r.host || '*').join(', ');
                const ingId = mermaidSafeId(`ing_${ingName}`);
                lines.push(`    ${ingId}[fa:fa-globe ${ingName}<br>${hosts}]`);
                lines.push(`    Internet --> ${ingId}`);

                for (const backend of entry.backends) {
                    const svcName = backend.split('/')[1] || '';
                    const svcId = mermaidSafeId(`svc_${svcName}`);
                    lines.push(`    ${ingId} --> ${svcId}`);
                }
            }

            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('mapping service topology', err))]);
        }
    }
}

// ---- trace_ingress_to_backend ----

interface TraceIngressToBackendInput {
    hostname: string;
    path: string;
}

export class TraceIngressToBackendTool implements vscode.LanguageModelTool<TraceIngressToBackendInput> {
    async prepareInvocation(
        options: vscode.LanguageModelToolInvocationPrepareOptions<TraceIngressToBackendInput>
    ): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Tracing ingress route for ${options.input.hostname}${options.input.path}...` };
    }

    async invoke(
        options: vscode.LanguageModelToolInvocationOptions<TraceIngressToBackendInput>
    ): Promise<vscode.LanguageModelToolResult> {
        try {
            const { hostname, path } = options.input;

            const lines: string[] = [];
            lines.push(`=== Ingress Trace: ${hostname}${path} ===`);
            lines.push('');

            // Find matching ingress (search all namespaces)
            const match = await findIngressForHostPath('', hostname, path);
            if (!match) {
                lines.push(`[WARNING] No ingress found matching host '${hostname}' and path '${path}'`);
                lines.push('');
                lines.push('Check that:');
                lines.push('  1. An ingress resource exists with this host/path combination');
                lines.push('  2. The hostname is spelled correctly');
                lines.push('  3. The path matches the ingress pathType (Prefix or Exact)');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            const { ingress, rule, ingressPath } = match;
            const ingName = ingress.metadata?.name || '';
            const ingNs = ingress.metadata?.namespace || '';

            lines.push('--- Ingress Layer ---');
            lines.push(`  Ingress: ${ingName} (namespace: ${ingNs})`);
            lines.push(`  Host: ${rule.host || '*'}`);
            lines.push(`  Path: ${ingressPath.path || '/'} (${ingressPath.pathType || 'Prefix'})`);

            // TLS config
            const tlsHosts = (ingress.spec?.tls || []).flatMap(t => t.hosts || []);
            if (tlsHosts.includes(hostname)) {
                const tlsEntry = (ingress.spec?.tls || []).find(t => (t.hosts || []).includes(hostname));
                lines.push(`  TLS: Enabled (secret: ${tlsEntry?.secretName || 'N/A'})`);
            } else {
                lines.push('  TLS: Not configured for this host');
            }

            // AGIC annotations
            const agicAnnotations = parseAGICAnnotations(ingress);
            if (agicAnnotations.length > 0) {
                lines.push('  AGIC Annotations:');
                for (const ann of agicAnnotations) {
                    lines.push(`    ${ann.key}: ${ann.value}`);
                }
            }

            // Service layer
            const backendSvcName = ingressPath.backend?.service?.name;
            const backendPort = ingressPath.backend?.service?.port?.number || ingressPath.backend?.service?.port?.name || '';
            if (!backendSvcName) {
                lines.push('');
                lines.push('[WARNING] No backend service defined for this path');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            lines.push('');
            lines.push('--- Service Layer ---');
            let svc: k8s.V1Service;
            try {
                svc = await getService(ingNs, backendSvcName);
                lines.push(`  Service: ${backendSvcName} (${svc.spec?.type || 'ClusterIP'})`);
                lines.push(`  Ports: ${formatServicePorts(svc)}`);
                lines.push(`  Selector: ${Object.entries(svc.spec?.selector || {}).map(([k, v]) => `${k}=${v}`).join(', ') || '<none>'}`);
            } catch {
                lines.push(`  [CRITICAL] Service '${backendSvcName}' not found in namespace '${ingNs}'`);
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            // Endpoints layer
            lines.push('');
            lines.push('--- Endpoints Layer ---');
            let readyAddresses: { ip: string; podName: string }[] = [];
            let notReadyAddresses: { ip: string; podName: string }[] = [];
            try {
                const ep = await getEndpoints(ingNs, backendSvcName);
                for (const subset of ep.subsets || []) {
                    for (const addr of subset.addresses || []) {
                        readyAddresses.push({ ip: addr.ip || '', podName: addr.targetRef?.name || '' });
                    }
                    for (const addr of subset.notReadyAddresses || []) {
                        notReadyAddresses.push({ ip: addr.ip || '', podName: addr.targetRef?.name || '' });
                    }
                }
                lines.push(`  Ready: ${readyAddresses.length}, Not Ready: ${notReadyAddresses.length}`);
                if (readyAddresses.length === 0 && notReadyAddresses.length === 0) {
                    lines.push('  [CRITICAL] No endpoints — traffic will fail');
                }
            } catch {
                lines.push(`  [WARNING] Could not fetch endpoints for '${backendSvcName}'`);
            }

            // Pod layer
            lines.push('');
            lines.push('--- Pod Layer ---');
            const allEndpointPodNames = [...readyAddresses, ...notReadyAddresses].map(a => a.podName).filter(Boolean);
            const allPods = await listPods(ingNs);
            const selector = svc.spec?.selector || {};
            const backingPods = Object.keys(selector).length > 0
                ? allPods.filter(p => podMatchesSelector(p, selector))
                : allPods.filter(p => allEndpointPodNames.includes(p.metadata?.name || ''));

            if (backingPods.length === 0) {
                lines.push('  [CRITICAL] No backing pods found');
            } else {
                for (const pod of backingPods) {
                    const healthy = isPodHealthy(pod);
                    const status = podPhaseReason(pod);
                    const { ready, total, restarts } = podContainerSummary(pod);
                    const healthTag = healthy ? 'HEALTHY' : 'UNHEALTHY';
                    lines.push(`  ${pod.metadata?.name} [${healthTag}] ${status} (${ready}/${total} ready, ${restarts} restarts)`);
                }
            }

            // Mermaid sequence diagram
            lines.push('');
            lines.push('TRACE DIAGRAM:');
            lines.push('```mermaid');
            lines.push('sequenceDiagram');
            lines.push('    participant C as Client');
            lines.push(`    participant I as Ingress<br>${ingName}`);
            lines.push(`    participant S as Service<br>${backendSvcName}`);

            const firstPod = backingPods.length > 0 ? backingPods[0] : null;
            const podDisplayName = firstPod?.metadata?.name || 'no-pod';
            const podIp = firstPod?.status?.podIP || '?';
            lines.push(`    participant P as Pod<br>${podDisplayName}`);

            lines.push(`    C->>I: ${path === '/' ? 'GET /' : `GET ${path}`}`);
            lines.push(`    Note over I: Host: ${hostname}`);
            lines.push(`    I->>S: Route to ${backendSvcName}:${backendPort}`);

            if (firstPod) {
                const targetPort = svc.spec?.ports?.[0]?.targetPort || backendPort;
                lines.push(`    S->>P: Forward to ${podIp}:${targetPort}`);
            } else {
                lines.push('    Note over S: No healthy pods available');
            }

            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('tracing ingress to backend', err))]);
        }
    }
}

// ---- list_endpoint_health ----

interface ListEndpointHealthInput {
    namespace: string;
}

export class ListEndpointHealthTool implements vscode.LanguageModelTool<ListEndpointHealthInput> {
    async prepareInvocation(
        options: vscode.LanguageModelToolInvocationPrepareOptions<ListEndpointHealthInput>
    ): Promise<vscode.PreparedToolInvocation> {
        const ns = options.input.namespace || 'all namespaces';
        return { invocationMessage: `Checking endpoint health in ${ns}...` };
    }

    async invoke(
        options: vscode.LanguageModelToolInvocationOptions<ListEndpointHealthInput>
    ): Promise<vscode.LanguageModelToolResult> {
        try {
            const { namespace } = options.input;
            const services = await listServices(namespace);

            const lines: string[] = [];
            const displayNs = !namespace || namespace === 'all' ? 'all' : namespace;
            lines.push(`=== Endpoint Health (namespace: ${displayNs}) ===`);
            lines.push('');

            const headers = ['SERVICE', 'NAMESPACE', 'TYPE', 'READY', 'NOT-READY', 'STATUS'];
            const rows: string[][] = [];
            let healthyCount = 0;
            let degradedCount = 0;
            let deadCount = 0;
            let noSelectorCount = 0;

            for (const svc of services) {
                const svcName = svc.metadata?.name || '';
                const svcNs = svc.metadata?.namespace || '';
                const svcType = svc.spec?.type || 'ClusterIP';
                const selector = svc.spec?.selector || {};

                if (Object.keys(selector).length === 0) {
                    rows.push([svcName, svcNs, svcType, '-', '-', 'NO_SELECTOR']);
                    noSelectorCount++;
                    continue;
                }

                let readyCount = 0;
                let notReadyCount = 0;
                try {
                    const ep = await getEndpoints(svcNs, svcName);
                    for (const subset of ep.subsets || []) {
                        readyCount += (subset.addresses || []).length;
                        notReadyCount += (subset.notReadyAddresses || []).length;
                    }
                } catch {
                    // No endpoints object at all
                }

                let status: string;
                if (readyCount === 0 && notReadyCount === 0) {
                    status = 'DEAD';
                    deadCount++;
                } else if (readyCount > 0 && notReadyCount === 0) {
                    status = 'HEALTHY';
                    healthyCount++;
                } else {
                    status = 'DEGRADED';
                    degradedCount++;
                }

                rows.push([svcName, svcNs, svcType, `${readyCount}`, `${notReadyCount}`, status]);
            }

            lines.push(formatTable(headers, rows));
            lines.push('');
            lines.push('--- Summary ---');
            lines.push(`  HEALTHY: ${healthyCount}`);
            lines.push(`  DEGRADED: ${degradedCount}`);
            lines.push(`  DEAD: ${deadCount}`);
            lines.push(`  NO_SELECTOR: ${noSelectorCount}`);
            lines.push(`  Total: ${services.length}`);

            if (deadCount > 0) {
                lines.push('');
                lines.push(`[CRITICAL] ${deadCount} service(s) have no ready endpoints — traffic will fail`);
            }
            if (degradedCount > 0) {
                lines.push(`[WARNING] ${degradedCount} service(s) have some endpoints not ready`);
            }

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('checking endpoint health', err))]);
        }
    }
}

// ---- analyze_service_connectivity ----

interface AnalyzeServiceConnectivityInput {
    namespace: string;
    service_name: string;
}

export class AnalyzeServiceConnectivityTool implements vscode.LanguageModelTool<AnalyzeServiceConnectivityInput> {
    async prepareInvocation(
        options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeServiceConnectivityInput>
    ): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Analyzing connectivity for service ${options.input.namespace}/${options.input.service_name}...` };
    }

    async invoke(
        options: vscode.LanguageModelToolInvocationOptions<AnalyzeServiceConnectivityInput>
    ): Promise<vscode.LanguageModelToolResult> {
        try {
            const { namespace, service_name } = options.input;

            const lines: string[] = [];
            lines.push(`=== Service Connectivity Analysis: ${service_name} (namespace: ${namespace}) ===`);
            lines.push('');

            // Get service
            const svc = await getService(namespace, service_name);
            const svcType = svc.spec?.type || 'ClusterIP';
            const selector = svc.spec?.selector || {};

            lines.push('--- Service Details ---');
            lines.push(`  Type: ${svcType}`);
            lines.push(`  ClusterIP: ${svc.spec?.clusterIP || '<none>'}`);
            lines.push(`  Ports: ${formatServicePorts(svc)}`);
            lines.push(`  Selector: ${Object.entries(selector).map(([k, v]) => `${k}=${v}`).join(', ') || '<none>'}`);
            if (svcType === 'LoadBalancer') {
                const lbIngress = svc.status?.loadBalancer?.ingress || [];
                const lbIPs = lbIngress.map(i => i.ip || i.hostname || '').filter(Boolean).join(', ');
                lines.push(`  External IP: ${lbIPs || '<pending>'}`);
            }
            if (svcType === 'NodePort') {
                const nodePorts = (svc.spec?.ports || []).map(p => p.nodePort).filter(Boolean).join(', ');
                lines.push(`  NodePorts: ${nodePorts}`);
            }

            // Backing pods
            lines.push('');
            lines.push('--- Backing Pods ---');
            const allPods = await listPods(namespace);
            const backingPods = Object.keys(selector).length > 0
                ? allPods.filter(p => podMatchesSelector(p, selector))
                : [];

            if (Object.keys(selector).length === 0) {
                lines.push('  [INFO] Service has no selector — endpoints are managed externally');
            } else if (backingPods.length === 0) {
                lines.push('  [CRITICAL] No pods match the service selector');
            } else {
                const healthyPods = backingPods.filter(p => isPodHealthy(p));
                const unhealthyPods = backingPods.filter(p => !isPodHealthy(p));
                lines.push(`  Total: ${backingPods.length}, Healthy: ${healthyPods.length}, Unhealthy: ${unhealthyPods.length}`);
                for (const pod of backingPods) {
                    const healthy = isPodHealthy(pod);
                    const status = podPhaseReason(pod);
                    const { ready, total, restarts } = podContainerSummary(pod);
                    const tag = healthy ? 'OK' : 'ISSUE';
                    lines.push(`  [${tag}] ${pod.metadata?.name}: ${status} (${ready}/${total} ready, ${restarts} restarts)`);
                }
            }

            // Endpoints
            lines.push('');
            lines.push('--- Endpoints ---');
            let readyEps = 0;
            let notReadyEps = 0;
            try {
                const ep = await getEndpoints(namespace, service_name);
                for (const subset of ep.subsets || []) {
                    for (const addr of subset.addresses || []) {
                        const ref = addr.targetRef ? ` (${addr.targetRef.kind}/${addr.targetRef.name})` : '';
                        for (const port of subset.ports || []) {
                            lines.push(`  [READY] ${addr.ip}:${port.port}${ref}`);
                        }
                        readyEps++;
                    }
                    for (const addr of subset.notReadyAddresses || []) {
                        const ref = addr.targetRef ? ` (${addr.targetRef.kind}/${addr.targetRef.name})` : '';
                        for (const port of subset.ports || []) {
                            lines.push(`  [NOT READY] ${addr.ip}:${port.port}${ref}`);
                        }
                        notReadyEps++;
                    }
                }
                if (readyEps === 0 && notReadyEps === 0) {
                    lines.push('  [CRITICAL] No endpoints — check service selector and pod labels');
                }
            } catch {
                lines.push('  [WARNING] Could not fetch endpoints');
            }

            // Network policies
            lines.push('');
            lines.push('--- Network Policies ---');
            try {
                const policies = await listNetworkPolicies(namespace);
                const matchingPolicies = policies.filter(np => {
                    const npSelector = np.spec?.podSelector?.matchLabels || {};
                    // Empty selector matches all pods
                    if (Object.keys(npSelector).length === 0 && (!np.spec?.podSelector?.matchExpressions || np.spec.podSelector.matchExpressions.length === 0)) {
                        return true;
                    }
                    // Check if the policy selector matches the service's pod selector
                    for (const [k, v] of Object.entries(npSelector)) {
                        if (selector[k] !== v) { return false; }
                    }
                    return true;
                });

                if (matchingPolicies.length === 0) {
                    lines.push('  [INFO] No network policies affect backing pods — all traffic allowed');
                } else {
                    lines.push(`  ${matchingPolicies.length} policy(ies) affect backing pods:`);
                    for (const np of matchingPolicies) {
                        const policyTypes = np.spec?.policyTypes || ['Ingress'];
                        lines.push(`  - ${np.metadata?.name} (${policyTypes.join(', ')})`);
                        if (policyTypes.includes('Ingress') && (!np.spec?.ingress || np.spec.ingress.length === 0)) {
                            lines.push('    [WARNING] Ingress policy with no rules — all ingress DENIED');
                        }
                        if (policyTypes.includes('Egress') && (!np.spec?.egress || np.spec.egress.length === 0)) {
                            lines.push('    [WARNING] Egress policy with no rules — all egress DENIED');
                        }
                    }
                }
            } catch {
                lines.push('  [WARNING] Could not fetch network policies');
            }

            // Ingress exposure
            lines.push('');
            lines.push('--- Ingress Exposure ---');
            try {
                const ingresses = await listIngresses(namespace);
                const exposingIngresses = ingresses.filter(ing => {
                    for (const rule of ing.spec?.rules || []) {
                        for (const p of rule.http?.paths || []) {
                            if (p.backend?.service?.name === service_name) { return true; }
                        }
                    }
                    return false;
                });

                if (exposingIngresses.length === 0) {
                    lines.push('  [INFO] Service is not exposed via any ingress');
                } else {
                    for (const ing of exposingIngresses) {
                        const hosts = (ing.spec?.rules || []).map(r => r.host || '*').join(', ');
                        const tls = (ing.spec?.tls?.length || 0) > 0 ? 'TLS' : 'No TLS';
                        lines.push(`  ${ing.metadata?.name}: ${hosts} [${tls}]`);
                    }
                }
            } catch {
                lines.push('  [WARNING] Could not check ingress exposure');
            }

            // Mermaid connectivity diagram
            lines.push('');
            lines.push('CONNECTIVITY DIAGRAM:');
            lines.push('```mermaid');
            lines.push('graph TD');

            const svcId = mermaidSafeId(`svc_${service_name}`);
            lines.push(`    ${svcId}[fa:fa-cog ${service_name}<br>${svcType}<br>${formatServicePorts(svc)}]`);

            // Pods
            for (const pod of backingPods) {
                const podName = pod.metadata?.name || '';
                const podId = mermaidSafeId(`pod_${podName}`);
                const healthy = isPodHealthy(pod);
                const status = podPhaseReason(pod);
                if (healthy) {
                    lines.push(`    ${podId}[fa:fa-cube ${podName}<br>${status}]`);
                } else {
                    lines.push(`    ${podId}[fa:fa-exclamation-triangle ${podName}<br>${status}]`);
                }
                lines.push(`    ${svcId} --> ${podId}`);
            }

            // Ingress connections
            try {
                const ingresses = await listIngresses(namespace);
                for (const ing of ingresses) {
                    let connects = false;
                    for (const rule of ing.spec?.rules || []) {
                        for (const p of rule.http?.paths || []) {
                            if (p.backend?.service?.name === service_name) { connects = true; }
                        }
                    }
                    if (connects) {
                        const ingName = ing.metadata?.name || '';
                        const ingId = mermaidSafeId(`ing_${ingName}`);
                        const hosts = (ing.spec?.rules || []).map(r => r.host || '*').join(', ');
                        lines.push(`    ${ingId}[fa:fa-globe ${ingName}<br>${hosts}]`);
                        lines.push(`    Internet((Internet)) --> ${ingId}`);
                        lines.push(`    ${ingId} --> ${svcId}`);
                    }
                }
            } catch { /* best effort */ }

            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing service connectivity', err))]);
        }
    }
}

// ---- analyze_all_ingresses ----

interface AnalyzeAllIngressesInput {
    namespace: string;
}

export class AnalyzeAllIngressesTool implements vscode.LanguageModelTool<AnalyzeAllIngressesInput> {
    async prepareInvocation(
        options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeAllIngressesInput>
    ): Promise<vscode.PreparedToolInvocation> {
        const ns = options.input.namespace || 'all namespaces';
        return { invocationMessage: `Analyzing all ingresses in ${ns}...` };
    }

    async invoke(
        options: vscode.LanguageModelToolInvocationOptions<AnalyzeAllIngressesInput>
    ): Promise<vscode.LanguageModelToolResult> {
        try {
            const { namespace } = options.input;
            const ingresses = await listIngresses(namespace);

            const lines: string[] = [];
            const displayNs = !namespace || namespace === 'all' ? 'all' : namespace;
            lines.push(`=== Ingress Analysis (namespace: ${displayNs}) ===`);
            lines.push('');

            if (ingresses.length === 0) {
                lines.push('No ingresses found.');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            lines.push(`Found ${ingresses.length} ingress(es)`);
            lines.push('');

            // Track host/path combinations for conflict detection
            const hostPathMap: Map<string, { ingressName: string; namespace: string }[]> = new Map();

            let issueCount = 0;

            for (const ing of ingresses) {
                const ingName = ing.metadata?.name || '';
                const ingNs = ing.metadata?.namespace || '';

                lines.push(`--- Ingress: ${ingName} (namespace: ${ingNs}) ---`);
                lines.push(`  Age: ${formatAge(ing.metadata?.creationTimestamp)}`);

                // TLS configuration
                const tlsEntries = ing.spec?.tls || [];
                if (tlsEntries.length > 0) {
                    lines.push('  TLS:');
                    for (const tls of tlsEntries) {
                        const hosts = (tls.hosts || []).join(', ');
                        lines.push(`    Hosts: ${hosts}, Secret: ${tls.secretName || '<none>'}`);
                        if (!tls.secretName) {
                            lines.push('    [WARNING] TLS entry without secret name');
                            issueCount++;
                        }
                    }
                } else {
                    lines.push('  TLS: Not configured');
                }

                // AGIC annotations
                const agicAnnotations = parseAGICAnnotations(ing);
                if (agicAnnotations.length > 0) {
                    lines.push('  AGIC Annotations:');
                    for (const ann of agicAnnotations) {
                        lines.push(`    ${ann.key}: ${ann.value}`);
                    }
                }

                // Rules and backend health
                const rules = ing.spec?.rules || [];
                if (rules.length === 0) {
                    lines.push('  [WARNING] No rules defined');
                    issueCount++;
                }

                for (const rule of rules) {
                    const host = rule.host || '*';
                    lines.push(`  Host: ${host}`);

                    // Check TLS coverage
                    const allTlsHosts = tlsEntries.flatMap(t => t.hosts || []);
                    if (host !== '*' && !allTlsHosts.includes(host)) {
                        lines.push(`    [INFO] Host '${host}' has no TLS configuration`);
                    }

                    for (const p of rule.http?.paths || []) {
                        const pathStr = p.path || '/';
                        const pathType = p.pathType || 'Prefix';
                        const backendSvcName = p.backend?.service?.name || '<none>';
                        const backendPort = p.backend?.service?.port?.number || p.backend?.service?.port?.name || '?';

                        // Track for conflict detection
                        const hostPathKey = `${host}:${pathStr}`;
                        if (!hostPathMap.has(hostPathKey)) {
                            hostPathMap.set(hostPathKey, []);
                        }
                        hostPathMap.get(hostPathKey)!.push({ ingressName: ingName, namespace: ingNs });

                        lines.push(`    Path: ${pathStr} (${pathType}) -> ${backendSvcName}:${backendPort}`);

                        // Check backend service health
                        if (backendSvcName !== '<none>') {
                            try {
                                const svc = await getService(ingNs, backendSvcName);
                                let readyEndpoints = 0;
                                let notReadyEndpoints = 0;
                                try {
                                    const ep = await getEndpoints(ingNs, backendSvcName);
                                    for (const subset of ep.subsets || []) {
                                        readyEndpoints += (subset.addresses || []).length;
                                        notReadyEndpoints += (subset.notReadyAddresses || []).length;
                                    }
                                } catch { /* endpoints may not exist */ }

                                if (readyEndpoints === 0) {
                                    lines.push(`      [CRITICAL] Backend '${backendSvcName}' has 0 ready endpoints`);
                                    issueCount++;
                                } else if (notReadyEndpoints > 0) {
                                    lines.push(`      [WARNING] Backend '${backendSvcName}' has ${notReadyEndpoints} not-ready endpoint(s) (${readyEndpoints} ready)`);
                                    issueCount++;
                                } else {
                                    lines.push(`      Backend OK: ${readyEndpoints} ready endpoint(s)`);
                                }
                            } catch {
                                lines.push(`      [CRITICAL] Backend service '${backendSvcName}' not found`);
                                issueCount++;
                            }
                        }
                    }
                }

                lines.push('');
            }

            // Check for host/path conflicts
            const conflicts: string[] = [];
            for (const [key, entries] of hostPathMap) {
                if (entries.length > 1) {
                    const names = entries.map(e => `${e.namespace}/${e.ingressName}`).join(', ');
                    conflicts.push(`  ${key} -> [${names}]`);
                }
            }

            if (conflicts.length > 0) {
                lines.push('--- Host/Path Conflicts ---');
                lines.push(`[WARNING] ${conflicts.length} conflicting host/path rule(s):`);
                for (const c of conflicts) {
                    lines.push(c);
                }
                issueCount += conflicts.length;
                lines.push('');
            }

            // Overall summary
            lines.push('--- Summary ---');
            lines.push(`  Ingresses: ${ingresses.length}`);
            lines.push(`  Issues: ${issueCount}`);
            if (issueCount === 0) {
                lines.push('  All ingresses appear healthy.');
            } else {
                lines.push(`  ${issueCount} issue(s) found. Review findings above.`);
            }

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing ingresses', err))]);
        }
    }
}

// ---- check_agic_health ----

export class CheckAGICHealthTool implements vscode.LanguageModelTool<Record<string, never>> {
    async prepareInvocation(): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: 'Checking AGIC (Azure Application Gateway Ingress Controller) health...' };
    }

    async invoke(): Promise<vscode.LanguageModelToolResult> {
        try {
            const lines: string[] = [];
            lines.push('=== AGIC Health Check ===');
            lines.push('');

            // Find AGIC pods across all namespaces
            const agicPods = await listPods('all', 'app=ingress-azure');

            if (agicPods.length === 0) {
                lines.push('[CRITICAL] No AGIC pods found (label: app=ingress-azure)');
                lines.push('');
                lines.push('Possible causes:');
                lines.push('  1. AGIC is not installed in this cluster');
                lines.push('  2. AGIC pods use a different label selector');
                lines.push('  3. The AGIC deployment has been scaled to 0');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            lines.push(`Found ${agicPods.length} AGIC pod(s)`);
            lines.push('');

            let overallHealthy = true;

            for (const pod of agicPods) {
                const podName = pod.metadata?.name || '';
                const podNs = pod.metadata?.namespace || '';
                const healthy = isPodHealthy(pod);
                const status = podPhaseReason(pod);
                const { ready, total, restarts } = podContainerSummary(pod);

                lines.push(`--- Pod: ${podName} (namespace: ${podNs}) ---`);
                lines.push(`  Status: ${status}`);
                lines.push(`  Containers: ${ready}/${total} ready`);
                lines.push(`  Restarts: ${restarts}`);
                lines.push(`  Node: ${pod.spec?.nodeName || 'unassigned'}`);
                lines.push(`  Age: ${formatAge(pod.metadata?.creationTimestamp)}`);

                if (!healthy) {
                    overallHealthy = false;
                    lines.push(`  [CRITICAL] Pod is not healthy`);

                    // Check container details
                    for (const cs of pod.status?.containerStatuses || []) {
                        if (cs.state?.waiting) {
                            lines.push(`  [CRITICAL] Container '${cs.name}' is waiting: ${cs.state.waiting.reason || 'Unknown'}`);
                        }
                        if (cs.state?.terminated) {
                            lines.push(`  [WARNING] Container '${cs.name}' terminated: ${cs.state.terminated.reason || 'Unknown'} (exit code: ${cs.state.terminated.exitCode})`);
                        }
                    }
                }

                if (restarts > 5) {
                    overallHealthy = false;
                    lines.push(`  [WARNING] High restart count: ${restarts}`);
                }

                // Recent events
                try {
                    const events = await getEventsForObject(podNs, podName);
                    const warnings = events.filter(e => e.type === 'Warning');
                    if (warnings.length > 0) {
                        overallHealthy = false;
                        lines.push(`  Recent Warning Events (${warnings.length}):`);
                        for (const e of warnings.slice(0, 10)) {
                            lines.push(`    - ${e.reason}: ${e.message}${(e.count || 1) > 1 ? ` (x${e.count})` : ''}`);
                        }
                    } else {
                        lines.push('  Recent Warning Events: none');
                    }
                } catch {
                    lines.push('  Recent Events: (could not fetch)');
                }

                // Recent logs for errors
                try {
                    const logs = await getPodLogs(podNs, podName, undefined, 100, false);
                    const logLines = logs.split('\n');
                    const errorLines = logLines.filter(l => {
                        const lower = l.toLowerCase();
                        return lower.includes('error') || lower.includes('fatal') || lower.includes('panic') || lower.includes('fail');
                    });

                    if (errorLines.length > 0) {
                        overallHealthy = false;
                        lines.push(`  Recent Log Errors (${errorLines.length} lines):`);
                        for (const line of errorLines.slice(0, 15)) {
                            lines.push(`    ${line.trim()}`);
                        }
                    } else {
                        lines.push('  Recent Log Errors: none');
                    }
                } catch {
                    lines.push('  Recent Logs: (could not fetch)');
                }

                lines.push('');
            }

            // Overall assessment
            lines.push('--- Overall Assessment ---');
            if (overallHealthy) {
                lines.push('  AGIC appears healthy. All pods are running with no errors.');
            } else {
                lines.push('  [WARNING] AGIC health issues detected. Review findings above.');
                lines.push('');
                lines.push('SUGGESTED ACTIONS:');
                let actionNum = 1;
                for (const pod of agicPods) {
                    if (!isPodHealthy(pod)) {
                        lines.push(`${actionNum++}. Investigate unhealthy AGIC pod '${pod.metadata?.name}'`);
                    }
                    if (podContainerSummary(pod).restarts > 5) {
                        lines.push(`${actionNum++}. Check logs for AGIC pod '${pod.metadata?.name}' — high restart count suggests recurring crashes`);
                    }
                }
                if (actionNum === 1) {
                    lines.push(`${actionNum++}. Review AGIC pod logs for error details`);
                }
            }

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('checking AGIC health', err))]);
        }
    }
}
