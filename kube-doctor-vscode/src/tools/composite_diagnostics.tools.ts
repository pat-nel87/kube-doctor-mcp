import * as vscode from 'vscode';
import * as k8s from '@kubernetes/client-node';
import { listServices, listIngresses, getEndpoints } from '../k8s/networking';
import { listPods, isPodHealthy, podPhaseReason, podContainerSummary } from '../k8s/pods';
import { listEvents, getEventsForObject } from '../k8s/events';
import { listNodes, nodeStatus } from '../k8s/nodes';
import { getNodeMetrics, getPodMetrics, parseCPU, parseMemory, formatBytes, PodMetrics } from '../k8s/metrics';
import { getDeployment } from '../k8s/workloads';
import { listNetworkPolicies } from '../k8s/network_policies';
import { getPodLogs } from '../k8s/logs';
import { getService, formatServicePorts, findIngressForHostPath, parseAGICAnnotations, mermaidSafeId } from '../k8s/network_analysis';
import { formatAge, formatTable, formatError } from '../util/formatting';

// ============================================================================
// 1. DiagnoseRequestPathTool — THE FLAGSHIP TOOL
// ============================================================================

interface DiagnoseRequestPathInput {
    hostname: string;
    path?: string;
    namespace?: string;
}

export class DiagnoseRequestPathTool implements vscode.LanguageModelTool<DiagnoseRequestPathInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<DiagnoseRequestPathInput>): Promise<vscode.PreparedToolInvocation> {
        const p = options.input.path || '/';
        return { invocationMessage: `Tracing request path for ${options.input.hostname}${p}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<DiagnoseRequestPathInput>): Promise<vscode.LanguageModelToolResult> {
        const { hostname, namespace } = options.input;
        const path = options.input.path || '/';
        const lines: string[] = [];
        const findings: string[] = [];

        try {
            lines.push(`=== Request Path Diagnosis: ${hostname}${path} ===`);
            lines.push('');

            // ------------------------------------------------------------------
            // [1] INGRESS LAYER
            // ------------------------------------------------------------------
            const match = await findIngressForHostPath(namespace || '', hostname, path);

            if (!match) {
                lines.push('[1] INGRESS LAYER');
                lines.push(`  No ingress found matching host "${hostname}" and path "${path}"`);
                lines.push('');
                findings.push(`[CRITICAL] No ingress found for ${hostname}${path}`);
                lines.push('FINDINGS:');
                for (const f of findings) { lines.push(`  ${f}`); }
                lines.push('');
                lines.push('SUGGESTED ACTIONS:');
                lines.push('1. Verify the hostname is correct and an Ingress resource exists for it');
                if (namespace) {
                    lines.push(`2. Check ingresses in namespace "${namespace}" — the ingress may be in a different namespace`);
                } else {
                    lines.push('2. Try specifying a namespace if you know which one the ingress should be in');
                }
                lines.push('3. List all ingresses: use the list_ingresses tool to find available ingress resources');
                lines.push('4. Verify DNS records point to the correct ingress controller');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            const { ingress, rule, ingressPath } = match;
            const ingName = ingress.metadata?.name || '<unknown>';
            const ingNs = ingress.metadata?.namespace || 'default';

            // Extract backend info
            const backendSvcName = ingressPath.backend?.service?.name || '';
            const backendSvcPort = ingressPath.backend?.service?.port?.number
                || ingressPath.backend?.service?.port?.name
                || '';

            // TLS check
            const tlsSecrets = (ingress.spec?.tls || [])
                .filter(t => (t.hosts || []).includes(hostname));
            const hasTLS = tlsSecrets.length > 0;
            const tlsSecretName = hasTLS ? (tlsSecrets[0].secretName || '<none>') : 'N/A';

            // AGIC annotations
            const agicAnnotations = parseAGICAnnotations(ingress);

            lines.push('[1] INGRESS LAYER');
            lines.push(`  Ingress: ${ingName} (namespace: ${ingNs})`);
            lines.push(`  Host: ${hostname}`);
            lines.push(`  Path: ${ingressPath.path || '/'} -> ${backendSvcName}:${backendSvcPort}`);
            lines.push(`  Path Type: ${ingressPath.pathType || 'Prefix'}`);
            lines.push(`  TLS: ${hasTLS ? `Yes (secret: ${tlsSecretName})` : 'No'}`);
            if (agicAnnotations.length > 0) {
                lines.push('  AGIC Annotations:');
                for (const ann of agicAnnotations) {
                    lines.push(`    ${ann.key}: ${ann.value}`);
                }
            }

            // Ingress events
            try {
                const ingressEvents = await getEventsForObject(ingNs, ingName);
                const warnings = ingressEvents.filter(e => e.type === 'Warning');
                if (warnings.length > 0) {
                    lines.push(`  Events: ${warnings.length} warning(s)`);
                    for (const e of warnings.slice(0, 5)) {
                        lines.push(`    - ${e.reason}: ${e.message}${(e.count || 1) > 1 ? ` (x${e.count})` : ''}`);
                    }
                    findings.push(`[WARNING] ${warnings.length} warning event(s) on ingress "${ingName}"`);
                } else {
                    lines.push('  Events: No warnings');
                }
            } catch { lines.push('  Events: (could not fetch)'); }

            lines.push('');

            // ------------------------------------------------------------------
            // [2] SERVICE LAYER
            // ------------------------------------------------------------------
            lines.push('[2] SERVICE LAYER');

            let svc: k8s.V1Service | undefined;
            let svcSelector: Record<string, string> = {};

            try {
                svc = await getService(ingNs, backendSvcName);
                const svcType = svc.spec?.type || 'ClusterIP';
                const svcPorts = formatServicePorts(svc);
                svcSelector = svc.spec?.selector || {};
                const targetPorts = (svc.spec?.ports || []).map(p => {
                    const target = p.targetPort ?? p.port;
                    return `${p.port}/${p.protocol || 'TCP'} -> ${target}`;
                }).join(', ');

                lines.push(`  Service: ${backendSvcName} (${svcType})`);
                lines.push(`  Ports: ${targetPorts}`);
                lines.push(`  Selector: ${Object.entries(svcSelector).map(([k, v]) => `${k}=${v}`).join(', ') || '<none>'}`);

                if (Object.keys(svcSelector).length === 0) {
                    findings.push(`[WARNING] Service "${backendSvcName}" has no selector — it will not route to any pods`);
                }

                // Service events
                try {
                    const svcEvents = await getEventsForObject(ingNs, backendSvcName);
                    const svcWarnings = svcEvents.filter(e => e.type === 'Warning');
                    if (svcWarnings.length > 0) {
                        lines.push(`  Events: ${svcWarnings.length} warning(s)`);
                        for (const e of svcWarnings.slice(0, 3)) {
                            lines.push(`    - ${e.reason}: ${e.message}`);
                        }
                    } else {
                        lines.push('  Events: No warnings');
                    }
                } catch { lines.push('  Events: (could not fetch)'); }
            } catch (err) {
                lines.push(`  Service: ${backendSvcName} — NOT FOUND`);
                findings.push(`[CRITICAL] Backend service "${backendSvcName}" not found in namespace "${ingNs}"`);
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [3] ENDPOINT & POD LAYER
            // ------------------------------------------------------------------
            lines.push('[3] ENDPOINT & POD LAYER');

            let readyAddresses: number = 0;
            let notReadyAddresses: number = 0;
            let backingPods: k8s.V1Pod[] = [];

            try {
                const endpoints = await getEndpoints(ingNs, backendSvcName);
                for (const subset of endpoints.subsets || []) {
                    readyAddresses += (subset.addresses || []).length;
                    notReadyAddresses += (subset.notReadyAddresses || []).length;
                }
                lines.push(`  Endpoints: ${readyAddresses} ready, ${notReadyAddresses} not-ready`);

                if (readyAddresses === 0 && notReadyAddresses === 0) {
                    findings.push(`[CRITICAL] No endpoints for service "${backendSvcName}" — no pods match the selector or no pods are running`);
                } else if (notReadyAddresses > 0) {
                    findings.push(`[WARNING] ${notReadyAddresses} not-ready endpoint(s) for service "${backendSvcName}"`);
                }
            } catch {
                lines.push('  Endpoints: (could not fetch)');
            }

            // Get backing pods via selector
            if (svc && Object.keys(svcSelector).length > 0) {
                try {
                    const selectorStr = Object.entries(svcSelector).map(([k, v]) => `${k}=${v}`).join(',');
                    backingPods = await listPods(ingNs, selectorStr);

                    if (backingPods.length === 0) {
                        findings.push(`[CRITICAL] No pods match service selector (${selectorStr}) in namespace "${ingNs}"`);
                        lines.push('  Pods: None found matching selector');
                    } else {
                        lines.push('  Pod Status:');
                        for (const pod of backingPods) {
                            const podName = pod.metadata?.name || '';
                            const phase = podPhaseReason(pod);
                            const { restarts, ready, total } = podContainerSummary(pod);
                            const readyStr = ready === total ? 'Ready' : `${ready}/${total} Ready`;
                            lines.push(`    ${podName}  ${phase}  ${restarts} restarts  ${readyStr}`);

                            if (!isPodHealthy(pod)) {
                                findings.push(`[WARNING] Pod "${podName}" is unhealthy: ${phase}`);
                            }
                            if (restarts > 5) {
                                findings.push(`[WARNING] Pod "${podName}" has ${restarts} restarts`);
                            }
                        }
                    }
                } catch {
                    lines.push('  Pods: (could not fetch)');
                }
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [4] RESOURCE USAGE
            // ------------------------------------------------------------------
            lines.push('[4] RESOURCE USAGE');

            let metricsAvailable = false;
            const podMetricsMap: Record<string, PodMetrics> = {};

            try {
                const allPodMetrics = await getPodMetrics(ingNs);
                for (const pm of allPodMetrics) {
                    podMetricsMap[pm.name] = pm;
                }
                metricsAvailable = true;
            } catch { /* metrics server not available */ }

            if (metricsAvailable && backingPods.length > 0) {
                for (const pod of backingPods) {
                    const podName = pod.metadata?.name || '';
                    const pm = podMetricsMap[podName];
                    if (!pm) {
                        lines.push(`  ${podName}: (no metrics available)`);
                        continue;
                    }

                    // Sum across containers
                    let totalCpuUsage = 0;
                    let totalMemUsage = 0;
                    for (const c of pm.containers) {
                        totalCpuUsage += parseCPU(c.usage.cpu);
                        totalMemUsage += parseMemory(c.usage.memory);
                    }

                    // Sum limits from pod spec
                    let totalCpuLimit = 0;
                    let totalMemLimit = 0;
                    let hasLimits = false;
                    for (const c of pod.spec?.containers || []) {
                        const cpuLim = c.resources?.limits?.['cpu'];
                        const memLim = c.resources?.limits?.['memory'];
                        if (cpuLim) { totalCpuLimit += parseCPU(cpuLim); hasLimits = true; }
                        if (memLim) { totalMemLimit += parseMemory(memLim); hasLimits = true; }
                    }

                    if (hasLimits) {
                        const cpuPct = totalCpuLimit > 0 ? `${(totalCpuUsage / totalCpuLimit * 100).toFixed(0)}%` : 'N/A';
                        const memPct = totalMemLimit > 0 ? `${(totalMemUsage / totalMemLimit * 100).toFixed(0)}%` : 'N/A';
                        const cpuLimStr = totalCpuLimit > 0 ? `${totalCpuLimit}m` : 'none';
                        const memLimStr = totalMemLimit > 0 ? formatBytes(totalMemLimit) : 'none';
                        lines.push(`  ${podName}: CPU ${totalCpuUsage}m/${cpuLimStr} (${cpuPct}) MEM ${formatBytes(totalMemUsage)}/${memLimStr} (${memPct})`);

                        // High resource usage warnings
                        if (totalCpuLimit > 0 && (totalCpuUsage / totalCpuLimit) > 0.9) {
                            findings.push(`[WARNING] Pod "${podName}" CPU usage is above 90% of limit`);
                        }
                        if (totalMemLimit > 0 && (totalMemUsage / totalMemLimit) > 0.9) {
                            findings.push(`[CRITICAL] Pod "${podName}" memory usage is above 90% of limit — risk of OOMKill`);
                        }
                    } else {
                        lines.push(`  ${podName}: CPU ${totalCpuUsage}m MEM ${formatBytes(totalMemUsage)} (no limits set)`);
                        findings.push(`[INFO] Pod "${podName}" has no resource limits configured`);
                    }
                }
            } else if (!metricsAvailable) {
                lines.push('  Metrics server not available — resource usage data unavailable');
                findings.push('[INFO] Metrics server not available — cannot assess resource usage');
            } else {
                lines.push('  No backing pods to report metrics for');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // FINDINGS
            // ------------------------------------------------------------------
            lines.push('FINDINGS:');
            if (findings.length === 0) {
                lines.push('  No issues found — request path appears healthy.');
            } else {
                for (const f of findings) {
                    lines.push(`  ${f}`);
                }
            }

            // ------------------------------------------------------------------
            // SUGGESTED ACTIONS
            // ------------------------------------------------------------------
            lines.push('');
            lines.push('SUGGESTED ACTIONS:');
            let actionNum = 1;

            const hasCritical = findings.some(f => f.startsWith('[CRITICAL]'));
            const hasWarning = findings.some(f => f.startsWith('[WARNING]'));

            if (findings.some(f => f.includes('No endpoints'))) {
                lines.push(`${actionNum++}. Check that pods matching the service selector are running and ready`);
            }
            if (findings.some(f => f.includes('NOT FOUND'))) {
                lines.push(`${actionNum++}. Create the missing backend service "${backendSvcName}" in namespace "${ingNs}"`);
            }
            if (findings.some(f => f.includes('unhealthy'))) {
                lines.push(`${actionNum++}. Investigate unhealthy pods using the diagnose_pod tool`);
            }
            if (findings.some(f => f.includes('no selector'))) {
                lines.push(`${actionNum++}. Add a pod selector to service "${backendSvcName}"`);
            }
            if (findings.some(f => f.includes('90% of limit'))) {
                lines.push(`${actionNum++}. Consider increasing resource limits or scaling out to more replicas`);
            }
            if (findings.some(f => f.includes('no resource limits'))) {
                lines.push(`${actionNum++}. Set resource requests and limits for production stability`);
            }
            if (findings.some(f => f.includes('restarts'))) {
                lines.push(`${actionNum++}. Check pod logs for crash causes — high restart counts indicate application issues`);
            }
            if (actionNum === 1) {
                lines.push('  No specific actions needed — the request path is healthy.');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // MERMAID TOPOLOGY DIAGRAM
            // ------------------------------------------------------------------
            const ingId = mermaidSafeId(ingName);
            const svcId = mermaidSafeId(backendSvcName);

            // Determine health colors
            const hasNoEndpoints = readyAddresses === 0 && notReadyAddresses === 0;
            const ingColor = findings.some(f => f.includes('ingress') && f.startsWith('[CRITICAL]')) ? '#FF6B6B' : '#90EE90';
            const svcColor = (!svc || findings.some(f => f.includes('NOT FOUND'))) ? '#FF6B6B'
                : findings.some(f => f.includes('no selector')) ? '#FFD700'
                : '#90EE90';
            const epColor = hasNoEndpoints ? '#FF6B6B'
                : notReadyAddresses > 0 ? '#FFD700'
                : '#90EE90';

            lines.push('TOPOLOGY:');
            lines.push('```mermaid');
            lines.push('graph TD');
            lines.push(`    Client((Client)) --> ${ingId}[${ingName}<br>${hostname}${path}]`);
            lines.push(`    ${ingId} --> ${svcId}[${backendSvcName}<br>${svc ? (svc.spec?.type || 'ClusterIP') : 'MISSING'}:${backendSvcPort}]`);

            if (backingPods.length > 0) {
                for (const pod of backingPods) {
                    const podName = pod.metadata?.name || '';
                    const podId = mermaidSafeId(podName);
                    const phase = podPhaseReason(pod);
                    const podColor = isPodHealthy(pod) ? '#90EE90' : (phase === 'CrashLoopBackOff' ? '#FF6B6B' : '#FFD700');
                    lines.push(`    ${svcId} --> ${podId}[${podName}<br>${phase}]`);
                    lines.push(`    style ${podId} fill:${podColor}`);
                }
            } else {
                lines.push(`    ${svcId} --> NoPods[No Pods<br>No endpoints]`);
                lines.push('    style NoPods fill:#FF6B6B');
            }

            lines.push(`    style ${ingId} fill:${ingColor}`);
            lines.push(`    style ${svcId} fill:${svcColor}`);
            lines.push('```');
            lines.push('');

            // ------------------------------------------------------------------
            // MERMAID SEQUENCE DIAGRAM
            // ------------------------------------------------------------------
            const podCount = backingPods.length;
            const healthyPods = backingPods.filter(p => isPodHealthy(p)).length;
            const podStatusNote = podCount > 0
                ? `${healthyPods}/${podCount} pods healthy`
                : 'No pods available';

            lines.push('REQUEST FLOW:');
            lines.push('```mermaid');
            lines.push('sequenceDiagram');
            lines.push(`    participant C as Client`);
            lines.push(`    participant I as Ingress<br>${ingName}`);
            lines.push(`    participant S as Service<br>${backendSvcName}`);
            lines.push(`    participant P as Pods (${readyAddresses} ready)`);
            lines.push(`    C->>I: ${hostname}${path}`);

            if (!svc) {
                lines.push(`    I--xS: Service not found`);
                lines.push(`    Note over I,S: Backend service missing`);
            } else if (hasNoEndpoints) {
                lines.push(`    I->>S: Route to ${backendSvcName}:${backendSvcPort}`);
                lines.push(`    S--xP: No endpoints`);
                lines.push(`    Note over S,P: No pods match selector`);
            } else {
                lines.push(`    I->>S: Route to ${backendSvcName}:${backendSvcPort}`);
                lines.push(`    S->>P: Forward to endpoints`);
                lines.push(`    Note over P: ${podStatusNote}`);
            }

            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError(`tracing request path for ${hostname}${path}`, err))]);
        }
    }
}

// ============================================================================
// 2. DiagnoseServiceTool
// ============================================================================

interface DiagnoseServiceInput {
    namespace: string;
    service_name: string;
}

export class DiagnoseServiceTool implements vscode.LanguageModelTool<DiagnoseServiceInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<DiagnoseServiceInput>): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Diagnosing service ${options.input.namespace}/${options.input.service_name}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<DiagnoseServiceInput>): Promise<vscode.LanguageModelToolResult> {
        const { namespace, service_name } = options.input;
        const lines: string[] = [];
        const findings: string[] = [];

        try {
            lines.push(`=== Service Diagnosis: ${service_name} (namespace: ${namespace}) ===`);
            lines.push('');

            // ------------------------------------------------------------------
            // [1] SERVICE CONFIG
            // ------------------------------------------------------------------
            lines.push('--- Service Configuration ---');

            let svc: k8s.V1Service;
            try {
                svc = await getService(namespace, service_name);
            } catch (err) {
                lines.push(formatError(`fetching service ${namespace}/${service_name}`, err));
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            const svcType = svc.spec?.type || 'ClusterIP';
            const selector = svc.spec?.selector || {};
            const selectorStr = Object.entries(selector).map(([k, v]) => `${k}=${v}`).join(', ') || '<none>';
            const ports = formatServicePorts(svc);
            const targetPorts = (svc.spec?.ports || []).map(p => {
                const target = p.targetPort ?? p.port;
                return `${p.port}/${p.protocol || 'TCP'} -> ${target}`;
            }).join(', ');

            lines.push(`  Type: ${svcType}`);
            lines.push(`  ClusterIP: ${svc.spec?.clusterIP || '<none>'}`);
            lines.push(`  Ports: ${targetPorts}`);
            lines.push(`  Selector: ${selectorStr}`);
            lines.push(`  Session Affinity: ${svc.spec?.sessionAffinity || 'None'}`);
            if (svcType === 'LoadBalancer') {
                const lbIPs = (svc.status?.loadBalancer?.ingress || []).map(i => i.ip || i.hostname || '').filter(Boolean);
                lines.push(`  LoadBalancer IPs: ${lbIPs.length > 0 ? lbIPs.join(', ') : 'pending'}`);
                if (lbIPs.length === 0) {
                    findings.push('[WARNING] LoadBalancer IP not yet assigned — check cloud provider integration');
                }
            }

            if (Object.keys(selector).length === 0) {
                findings.push('[WARNING] Service has no selector — it will not route to any pods automatically');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [2] ENDPOINT HEALTH
            // ------------------------------------------------------------------
            lines.push('--- Endpoint Health ---');

            let readyCount = 0;
            let notReadyCount = 0;

            try {
                const endpoints = await getEndpoints(namespace, service_name);
                for (const subset of endpoints.subsets || []) {
                    readyCount += (subset.addresses || []).length;
                    notReadyCount += (subset.notReadyAddresses || []).length;
                }
                lines.push(`  Ready: ${readyCount}`);
                lines.push(`  Not-Ready: ${notReadyCount}`);

                if (readyCount === 0 && notReadyCount === 0) {
                    findings.push(`[CRITICAL] No endpoints — no pods match selector or no pods are running`);
                } else if (readyCount === 0) {
                    findings.push(`[CRITICAL] All ${notReadyCount} endpoints are not-ready`);
                } else if (notReadyCount > 0) {
                    findings.push(`[WARNING] ${notReadyCount} not-ready endpoint(s)`);
                }
            } catch {
                lines.push('  (could not fetch endpoints)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [3] BACKING POD STATUS
            // ------------------------------------------------------------------
            lines.push('--- Backing Pods ---');

            let backingPods: k8s.V1Pod[] = [];

            if (Object.keys(selector).length > 0) {
                try {
                    const labelStr = Object.entries(selector).map(([k, v]) => `${k}=${v}`).join(',');
                    backingPods = await listPods(namespace, labelStr);

                    if (backingPods.length === 0) {
                        lines.push('  No pods match the service selector.');
                        findings.push('[CRITICAL] No pods match the service selector');
                    } else {
                        const headers = ['NAME', 'STATUS', 'RESTARTS', 'AGE', 'READY'];
                        const rows = backingPods.map(p => {
                            const { ready, total, restarts } = podContainerSummary(p);
                            return [
                                p.metadata?.name || '',
                                podPhaseReason(p),
                                `${restarts}`,
                                formatAge(p.metadata?.creationTimestamp),
                                `${ready}/${total}`,
                            ];
                        });
                        lines.push(formatTable(headers, rows));

                        const unhealthy = backingPods.filter(p => !isPodHealthy(p));
                        if (unhealthy.length > 0) {
                            findings.push(`[WARNING] ${unhealthy.length}/${backingPods.length} backing pods are unhealthy`);
                        }
                        const highRestarts = backingPods.filter(p => podContainerSummary(p).restarts > 5);
                        if (highRestarts.length > 0) {
                            findings.push(`[WARNING] ${highRestarts.length} pod(s) have >5 restarts`);
                        }
                    }
                } catch {
                    lines.push('  (could not fetch pods)');
                }
            } else {
                lines.push('  N/A — service has no selector');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [4] RESOURCE USAGE
            // ------------------------------------------------------------------
            lines.push('--- Resource Usage ---');

            try {
                if (backingPods.length > 0) {
                    const allPodMetrics = await getPodMetrics(namespace);
                    const metricsMap: Record<string, PodMetrics> = {};
                    for (const pm of allPodMetrics) { metricsMap[pm.name] = pm; }

                    for (const pod of backingPods) {
                        const podName = pod.metadata?.name || '';
                        const pm = metricsMap[podName];
                        if (!pm) { continue; }

                        let cpuUsage = 0;
                        let memUsage = 0;
                        for (const c of pm.containers) {
                            cpuUsage += parseCPU(c.usage.cpu);
                            memUsage += parseMemory(c.usage.memory);
                        }

                        let cpuLimit = 0;
                        let memLimit = 0;
                        for (const c of pod.spec?.containers || []) {
                            if (c.resources?.limits?.['cpu']) { cpuLimit += parseCPU(c.resources.limits['cpu']); }
                            if (c.resources?.limits?.['memory']) { memLimit += parseMemory(c.resources.limits['memory']); }
                        }

                        const cpuStr = cpuLimit > 0
                            ? `CPU ${cpuUsage}m/${cpuLimit}m (${(cpuUsage / cpuLimit * 100).toFixed(0)}%)`
                            : `CPU ${cpuUsage}m (no limit)`;
                        const memStr = memLimit > 0
                            ? `MEM ${formatBytes(memUsage)}/${formatBytes(memLimit)} (${(memUsage / memLimit * 100).toFixed(0)}%)`
                            : `MEM ${formatBytes(memUsage)} (no limit)`;
                        lines.push(`  ${podName}: ${cpuStr}  ${memStr}`);

                        if (memLimit > 0 && (memUsage / memLimit) > 0.9) {
                            findings.push(`[CRITICAL] Pod "${podName}" memory at ${(memUsage / memLimit * 100).toFixed(0)}% of limit`);
                        }
                        if (cpuLimit > 0 && (cpuUsage / cpuLimit) > 0.9) {
                            findings.push(`[WARNING] Pod "${podName}" CPU at ${(cpuUsage / cpuLimit * 100).toFixed(0)}% of limit`);
                        }
                    }
                } else {
                    lines.push('  No backing pods to report metrics for');
                }
            } catch {
                lines.push('  Metrics server not available');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [5] INGRESS EXPOSURE
            // ------------------------------------------------------------------
            lines.push('--- Ingress Exposure ---');

            try {
                const allIngresses = await listIngresses(namespace);
                const matchingIngresses = allIngresses.filter(ing => {
                    for (const rule of ing.spec?.rules || []) {
                        for (const p of rule.http?.paths || []) {
                            if (p.backend?.service?.name === service_name) {
                                return true;
                            }
                        }
                    }
                    return false;
                });

                if (matchingIngresses.length === 0) {
                    lines.push('  No ingresses route to this service');
                    findings.push('[INFO] Service is not exposed via any Ingress');
                } else {
                    for (const ing of matchingIngresses) {
                        for (const rule of ing.spec?.rules || []) {
                            for (const p of rule.http?.paths || []) {
                                if (p.backend?.service?.name === service_name) {
                                    const host = rule.host || '*';
                                    const ingPath = p.path || '/';
                                    lines.push(`  ${ing.metadata?.name}: ${host}${ingPath} -> ${service_name}:${p.backend?.service?.port?.number || p.backend?.service?.port?.name || '?'}`);
                                }
                            }
                        }
                    }
                }
            } catch {
                lines.push('  (could not fetch ingresses)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [6] NETWORK POLICIES
            // ------------------------------------------------------------------
            lines.push('--- Network Policies ---');

            try {
                const policies = await listNetworkPolicies(namespace);
                const matchingPolicies = policies.filter(np => {
                    const npSelector = np.spec?.podSelector?.matchLabels || {};
                    if (Object.keys(npSelector).length === 0 && (!np.spec?.podSelector?.matchExpressions || np.spec.podSelector.matchExpressions.length === 0)) {
                        return true; // empty selector matches all
                    }
                    return Object.entries(npSelector).every(([k, v]) => selector[k] === v);
                });

                if (matchingPolicies.length === 0) {
                    lines.push('  No network policies affect pods selected by this service');
                    lines.push('  [INFO] All traffic allowed by default');
                } else {
                    for (const np of matchingPolicies) {
                        const types = np.spec?.policyTypes || ['Ingress'];
                        lines.push(`  ${np.metadata?.name}: ${types.join(', ')}`);

                        if (types.includes('Ingress') && (!np.spec?.ingress || np.spec.ingress.length === 0)) {
                            findings.push(`[WARNING] Network policy "${np.metadata?.name}" denies all ingress traffic`);
                        }
                        if (types.includes('Egress') && (!np.spec?.egress || np.spec.egress.length === 0)) {
                            findings.push(`[WARNING] Network policy "${np.metadata?.name}" denies all egress traffic`);
                        }
                    }
                }
            } catch {
                lines.push('  (could not fetch network policies)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [7] RECENT EVENTS
            // ------------------------------------------------------------------
            lines.push('--- Recent Events ---');

            try {
                const events = await getEventsForObject(namespace, service_name);
                if (events.length === 0) {
                    lines.push('  No recent events');
                } else {
                    for (const e of events.slice(0, 10)) {
                        const prefix = e.type === 'Warning' ? '[WARNING]' : '[INFO]';
                        lines.push(`  ${prefix} ${e.reason}: ${e.message}${(e.count || 1) > 1 ? ` (x${e.count})` : ''}`);
                    }
                }
            } catch {
                lines.push('  (could not fetch events)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // ASSESSMENT
            // ------------------------------------------------------------------
            lines.push('--- Assessment ---');
            lines.push('FINDINGS:');
            if (findings.length === 0) {
                lines.push('  No issues found — service appears healthy.');
            } else {
                for (const f of findings) {
                    lines.push(`  ${f}`);
                }
            }

            lines.push('');

            // ------------------------------------------------------------------
            // MERMAID TOPOLOGY
            // ------------------------------------------------------------------
            const svcId = mermaidSafeId(service_name);

            lines.push('SERVICE TOPOLOGY:');
            lines.push('```mermaid');
            lines.push('graph TD');

            // Ingresses pointing to this service
            try {
                const allIngresses = await listIngresses(namespace);
                let ingIdx = 0;
                for (const ing of allIngresses) {
                    for (const rule of ing.spec?.rules || []) {
                        for (const p of rule.http?.paths || []) {
                            if (p.backend?.service?.name === service_name) {
                                const iId = mermaidSafeId(`ing_${ing.metadata?.name}_${ingIdx}`);
                                lines.push(`    ${iId}[Ingress: ${ing.metadata?.name}<br>${rule.host || '*'}${p.path || '/'}] --> ${svcId}[${service_name}<br>${svcType}]`);
                                lines.push(`    style ${iId} fill:#90EE90`);
                                ingIdx++;
                            }
                        }
                    }
                }
                if (ingIdx === 0) {
                    lines.push(`    Clients((Clients)) --> ${svcId}[${service_name}<br>${svcType}]`);
                }
            } catch {
                lines.push(`    Clients((Clients)) --> ${svcId}[${service_name}<br>${svcType}]`);
            }

            // Pods
            if (backingPods.length > 0) {
                for (const pod of backingPods) {
                    const podName = pod.metadata?.name || '';
                    const podId = mermaidSafeId(podName);
                    const phase = podPhaseReason(pod);
                    const podColor = isPodHealthy(pod) ? '#90EE90' : (phase === 'CrashLoopBackOff' ? '#FF6B6B' : '#FFD700');
                    lines.push(`    ${svcId} --> ${podId}[${podName}<br>${phase}]`);
                    lines.push(`    style ${podId} fill:${podColor}`);
                }
            } else {
                lines.push(`    ${svcId} --> NoPods[No Pods]`);
                lines.push('    style NoPods fill:#FF6B6B');
            }

            const svcNodeColor = readyCount === 0 ? '#FF6B6B' : notReadyCount > 0 ? '#FFD700' : '#90EE90';
            lines.push(`    style ${svcId} fill:${svcNodeColor}`);
            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError(`diagnosing service ${namespace}/${service_name}`, err))]);
        }
    }
}

// ============================================================================
// 3. ClusterHealthOverviewTool
// ============================================================================

export class ClusterHealthOverviewTool implements vscode.LanguageModelTool<Record<string, never>> {
    async prepareInvocation(): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: 'Generating cluster health overview...' };
    }

    async invoke(): Promise<vscode.LanguageModelToolResult> {
        const lines: string[] = [];
        const findings: string[] = [];

        try {
            lines.push('=== Cluster Health Overview ===');
            lines.push('');

            // ------------------------------------------------------------------
            // [1] NODE SUMMARY
            // ------------------------------------------------------------------
            lines.push('--- Node Summary ---');

            const nodes = await listNodes();
            let readyNodes = 0;
            let notReadyNodes = 0;
            const nodeIssues: string[] = [];

            // Try to get node metrics
            let nodeMetricsMap: Record<string, { cpu: string; memory: string }> = {};
            let nodeCapacityMap: Record<string, { cpu: number; mem: number }> = {};
            try {
                const nodeMetrics = await getNodeMetrics();
                for (const nm of nodeMetrics) {
                    nodeMetricsMap[nm.name] = nm.usage;
                }
            } catch { /* metrics not available */ }

            const nodeHeaders = ['NODE', 'STATUS', 'CPU USAGE', 'MEM USAGE', 'AGE'];
            const nodeRows: string[][] = [];

            for (const node of nodes) {
                const name = node.metadata?.name || '';
                const status = nodeStatus(node);
                if (status === 'Ready') { readyNodes++; } else { notReadyNodes++; }

                const cpuCap = node.status?.capacity?.['cpu'] || '0';
                const memCap = node.status?.capacity?.['memory'] || '0';
                const cpuCapMilli = parseCPU(cpuCap) * 1000;
                const memCapBytes = parseMemory(memCap);
                nodeCapacityMap[name] = { cpu: cpuCapMilli, mem: memCapBytes };

                let cpuStr = 'N/A';
                let memStr = 'N/A';
                const usage = nodeMetricsMap[name];
                if (usage) {
                    const cpuUsage = parseCPU(usage.cpu);
                    const memUsage = parseMemory(usage.memory);
                    cpuStr = cpuCapMilli > 0 ? `${cpuUsage}m (${(cpuUsage / cpuCapMilli * 100).toFixed(0)}%)` : `${cpuUsage}m`;
                    memStr = memCapBytes > 0 ? `${formatBytes(memUsage)} (${(memUsage / memCapBytes * 100).toFixed(0)}%)` : formatBytes(memUsage);

                    if (cpuCapMilli > 0 && (cpuUsage / cpuCapMilli) > 0.9) {
                        findings.push(`[WARNING] Node "${name}" CPU usage above 90%`);
                    }
                    if (memCapBytes > 0 && (memUsage / memCapBytes) > 0.9) {
                        findings.push(`[CRITICAL] Node "${name}" memory usage above 90%`);
                    }
                }

                if (status !== 'Ready') {
                    findings.push(`[CRITICAL] Node "${name}" is ${status}`);
                }

                // Check pressure conditions
                for (const cond of node.status?.conditions || []) {
                    if (['MemoryPressure', 'DiskPressure', 'PIDPressure'].includes(cond.type || '') && cond.status === 'True') {
                        findings.push(`[WARNING] Node "${name}" has ${cond.type}`);
                    }
                }

                nodeRows.push([name, status, cpuStr, memStr, formatAge(node.metadata?.creationTimestamp)]);
            }

            lines.push(formatTable(nodeHeaders, nodeRows));
            lines.push(`  Total: ${nodes.length}, Ready: ${readyNodes}, NotReady: ${notReadyNodes}`);
            lines.push('');

            // ------------------------------------------------------------------
            // [2] POD HEALTH BY NAMESPACE
            // ------------------------------------------------------------------
            lines.push('--- Pod Health by Namespace ---');

            const allPods = await listPods('all');
            const nsPodStats: Record<string, { running: number; pending: number; failed: number; crashloop: number; succeeded: number; total: number }> = {};

            for (const pod of allPods) {
                const ns = pod.metadata?.namespace || 'default';
                if (!nsPodStats[ns]) {
                    nsPodStats[ns] = { running: 0, pending: 0, failed: 0, crashloop: 0, succeeded: 0, total: 0 };
                }
                const stats = nsPodStats[ns];
                stats.total++;

                const reason = podPhaseReason(pod);
                if (reason === 'CrashLoopBackOff') {
                    stats.crashloop++;
                } else if (pod.status?.phase === 'Running') {
                    stats.running++;
                } else if (pod.status?.phase === 'Pending') {
                    stats.pending++;
                } else if (pod.status?.phase === 'Failed') {
                    stats.failed++;
                } else if (pod.status?.phase === 'Succeeded') {
                    stats.succeeded++;
                }
            }

            const podHeaders = ['NAMESPACE', 'RUNNING', 'PENDING', 'FAILED', 'CRASHLOOP', 'TOTAL'];
            const podRows: string[][] = [];
            const sortedNamespaces = Object.keys(nsPodStats).sort();

            for (const ns of sortedNamespaces) {
                const s = nsPodStats[ns];
                podRows.push([ns, `${s.running}`, `${s.pending}`, `${s.failed}`, `${s.crashloop}`, `${s.total}`]);

                if (s.crashloop > 0) {
                    findings.push(`[CRITICAL] ${s.crashloop} pod(s) in CrashLoopBackOff in namespace "${ns}"`);
                }
                if (s.failed > 0) {
                    findings.push(`[WARNING] ${s.failed} failed pod(s) in namespace "${ns}"`);
                }
                if (s.pending > 0) {
                    findings.push(`[WARNING] ${s.pending} pending pod(s) in namespace "${ns}"`);
                }
            }

            lines.push(formatTable(podHeaders, podRows));

            // Grand totals
            let totalRunning = 0, totalPending = 0, totalFailed = 0, totalCrashloop = 0;
            for (const s of Object.values(nsPodStats)) {
                totalRunning += s.running;
                totalPending += s.pending;
                totalFailed += s.failed;
                totalCrashloop += s.crashloop;
            }
            lines.push(`  Totals — Running: ${totalRunning}, Pending: ${totalPending}, Failed: ${totalFailed}, CrashLoop: ${totalCrashloop}, Total: ${allPods.length}`);
            lines.push('');

            // ------------------------------------------------------------------
            // [3] SERVICE ENDPOINT HEALTH
            // ------------------------------------------------------------------
            lines.push('--- Service Endpoint Health ---');

            let healthySvc = 0, degradedSvc = 0, deadSvc = 0;

            try {
                const services = await listServices('all');
                // Skip headless and kubernetes default
                const targetServices = services.filter(s =>
                    s.spec?.clusterIP !== 'None' &&
                    !(s.metadata?.name === 'kubernetes' && s.metadata?.namespace === 'default')
                );

                for (const svc of targetServices) {
                    const svcName = svc.metadata?.name || '';
                    const svcNs = svc.metadata?.namespace || '';
                    const svcSelector = svc.spec?.selector || {};

                    if (Object.keys(svcSelector).length === 0) { continue; }

                    try {
                        const ep = await getEndpoints(svcNs, svcName);
                        let ready = 0;
                        let notReady = 0;
                        for (const subset of ep.subsets || []) {
                            ready += (subset.addresses || []).length;
                            notReady += (subset.notReadyAddresses || []).length;
                        }

                        if (ready === 0 && notReady === 0) {
                            deadSvc++;
                        } else if (ready === 0) {
                            deadSvc++;
                        } else if (notReady > 0) {
                            degradedSvc++;
                        } else {
                            healthySvc++;
                        }
                    } catch {
                        deadSvc++;
                    }
                }

                lines.push(`  Healthy: ${healthySvc}, Degraded: ${degradedSvc}, No Endpoints: ${deadSvc}`);
                if (deadSvc > 0) {
                    findings.push(`[CRITICAL] ${deadSvc} service(s) have no ready endpoints`);
                }
                if (degradedSvc > 0) {
                    findings.push(`[WARNING] ${degradedSvc} service(s) have degraded endpoints`);
                }
            } catch {
                lines.push('  (could not evaluate service endpoints)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [4] RECENT WARNING EVENTS
            // ------------------------------------------------------------------
            lines.push('--- Recent Warning Events (last hour) ---');

            try {
                const events = await listEvents(undefined);
                const oneHourAgo = Date.now() - 3600000;
                const warnings = events.filter(e => {
                    const t = e.lastTimestamp ? new Date(e.lastTimestamp).getTime() : (e.metadata?.creationTimestamp ? new Date(e.metadata.creationTimestamp).getTime() : 0);
                    return e.type === 'Warning' && t > oneHourAgo;
                });

                if (warnings.length === 0) {
                    lines.push('  No warning events in the last hour.');
                } else {
                    // Group by reason
                    const grouped: Record<string, number> = {};
                    for (const e of warnings) {
                        const reason = e.reason || 'Unknown';
                        grouped[reason] = (grouped[reason] || 0) + (e.count || 1);
                    }
                    const sorted = Object.entries(grouped).sort((a, b) => b[1] - a[1]);
                    for (const [reason, count] of sorted.slice(0, 10)) {
                        lines.push(`  ${reason}: ${count} occurrence(s)`);
                    }
                    lines.push(`  Total: ${warnings.length} warning event(s)`);
                    findings.push(`[WARNING] ${warnings.length} warning events in the last hour`);
                }
            } catch {
                lines.push('  (could not fetch events)');
            }

            lines.push('');

            // ------------------------------------------------------------------
            // [5] OVERALL ASSESSMENT
            // ------------------------------------------------------------------
            lines.push('--- Overall Assessment ---');
            lines.push('FINDINGS:');
            if (findings.length === 0) {
                lines.push('  Cluster appears healthy. No issues found.');
            } else {
                const criticals = findings.filter(f => f.startsWith('[CRITICAL]'));
                const warns = findings.filter(f => f.startsWith('[WARNING]'));
                const infos = findings.filter(f => f.startsWith('[INFO]'));
                for (const f of criticals) { lines.push(`  ${f}`); }
                for (const f of warns) { lines.push(`  ${f}`); }
                for (const f of infos) { lines.push(`  ${f}`); }
                lines.push('');
                lines.push(`  Summary: ${criticals.length} critical, ${warns.length} warning, ${infos.length} info`);
            }

            lines.push('');

            // ------------------------------------------------------------------
            // MERMAID PIE CHART — POD STATUS
            // ------------------------------------------------------------------
            lines.push('POD STATUS DISTRIBUTION:');
            lines.push('```mermaid');
            lines.push('pie title Pod Status Across Cluster');
            if (totalRunning > 0) { lines.push(`    "Running" : ${totalRunning}`); }
            if (totalPending > 0) { lines.push(`    "Pending" : ${totalPending}`); }
            if (totalFailed > 0) { lines.push(`    "Failed" : ${totalFailed}`); }
            if (totalCrashloop > 0) { lines.push(`    "CrashLoopBackOff" : ${totalCrashloop}`); }
            const totalSucceeded = Object.values(nsPodStats).reduce((sum, s) => sum + s.succeeded, 0);
            if (totalSucceeded > 0) { lines.push(`    "Succeeded" : ${totalSucceeded}`); }
            lines.push('```');

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('generating cluster health overview', err))]);
        }
    }
}

// ============================================================================
// 4. AnalyzeServiceLogsTool
// ============================================================================

interface AnalyzeServiceLogsInput {
    namespace: string;
    deployment_name: string;
    pattern?: string;
    tail_lines?: number;
}

export class AnalyzeServiceLogsTool implements vscode.LanguageModelTool<AnalyzeServiceLogsInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeServiceLogsInput>): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Analyzing logs for deployment ${options.input.namespace}/${options.input.deployment_name}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<AnalyzeServiceLogsInput>): Promise<vscode.LanguageModelToolResult> {
        const { namespace, deployment_name } = options.input;
        const pattern = options.input.pattern || 'error|exception|fatal|panic|timeout|refused';
        const tailLines = options.input.tail_lines || 200;
        const lines: string[] = [];

        try {
            lines.push(`=== Service Log Analysis: ${deployment_name} (namespace: ${namespace}) ===`);
            lines.push(`Pattern: /${pattern}/i`);
            lines.push(`Tail Lines: ${tailLines}`);
            lines.push('');

            // Get the deployment
            let deployment: k8s.V1Deployment;
            try {
                deployment = await getDeployment(namespace, deployment_name);
            } catch (err) {
                lines.push(formatError(`fetching deployment ${namespace}/${deployment_name}`, err));
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            // Get pods by selector
            const selector = deployment.spec?.selector?.matchLabels || {};
            if (Object.keys(selector).length === 0) {
                lines.push('[WARNING] Deployment has no matchLabels selector — cannot find pods');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            const labelStr = Object.entries(selector).map(([k, v]) => `${k}=${v}`).join(',');
            const pods = await listPods(namespace, labelStr);

            if (pods.length === 0) {
                lines.push('No pods found matching the deployment selector.');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
            }

            lines.push(`Found ${pods.length} pod(s) for deployment "${deployment_name}"`);
            lines.push('');

            const regex = new RegExp(pattern, 'i');
            const allErrorTypes: Record<string, number> = {};
            let totalMatches = 0;
            let podsWithErrors = 0;

            for (const pod of pods) {
                const podName = pod.metadata?.name || '';
                lines.push(`--- ${podName} ---`);

                // Get logs for each container
                const containers = pod.spec?.containers || [];
                let podHasErrors = false;

                for (const container of containers) {
                    let logs: string;
                    try {
                        logs = await getPodLogs(namespace, podName, container.name, tailLines, false);
                    } catch {
                        lines.push(`  [${container.name}] (could not fetch logs)`);
                        continue;
                    }

                    if (!logs || logs.trim().length === 0) {
                        lines.push(`  [${container.name}] (no logs)`);
                        continue;
                    }

                    const logLines = logs.split('\n');
                    const matchingLines: string[] = [];
                    const localErrorTypes: Record<string, number> = {};

                    for (const logLine of logLines) {
                        if (regex.test(logLine)) {
                            matchingLines.push(logLine);
                            totalMatches++;

                            // Classify the error type
                            const lowerLine = logLine.toLowerCase();
                            let classified = false;
                            const errorCategories = ['fatal', 'panic', 'exception', 'timeout', 'refused', 'error'];
                            for (const category of errorCategories) {
                                if (lowerLine.includes(category)) {
                                    const key = category.charAt(0).toUpperCase() + category.slice(1);
                                    localErrorTypes[key] = (localErrorTypes[key] || 0) + 1;
                                    allErrorTypes[key] = (allErrorTypes[key] || 0) + 1;
                                    classified = true;
                                    break;
                                }
                            }
                            if (!classified) {
                                localErrorTypes['Other'] = (localErrorTypes['Other'] || 0) + 1;
                                allErrorTypes['Other'] = (allErrorTypes['Other'] || 0) + 1;
                            }
                        }
                    }

                    if (matchingLines.length === 0) {
                        lines.push(`  [${container.name}] No matching lines`);
                    } else {
                        podHasErrors = true;
                        lines.push(`  [${container.name}] ${matchingLines.length} match(es):`);

                        // Show error type breakdown for this container
                        for (const [errType, count] of Object.entries(localErrorTypes).sort((a, b) => b[1] - a[1])) {
                            lines.push(`    ${errType}: ${count}`);
                        }

                        // Show sample matching lines (up to 5)
                        lines.push('    Sample lines:');
                        for (const sample of matchingLines.slice(-5)) {
                            // Truncate very long lines
                            const truncated = sample.length > 200 ? sample.slice(0, 200) + '...' : sample;
                            lines.push(`      ${truncated}`);
                        }
                    }
                }

                if (podHasErrors) { podsWithErrors++; }
                lines.push('');
            }

            // ------------------------------------------------------------------
            // SUMMARY
            // ------------------------------------------------------------------
            lines.push('--- Summary ---');
            lines.push(`  Pods analyzed: ${pods.length}`);
            lines.push(`  Pods with errors: ${podsWithErrors}`);
            lines.push(`  Total matching lines: ${totalMatches}`);
            lines.push('');

            if (Object.keys(allErrorTypes).length > 0) {
                lines.push('  Error Type Breakdown:');
                const sortedTypes = Object.entries(allErrorTypes).sort((a, b) => b[1] - a[1]);
                for (const [errType, count] of sortedTypes) {
                    const pct = totalMatches > 0 ? `(${(count / totalMatches * 100).toFixed(0)}%)` : '';
                    lines.push(`    ${errType}: ${count} ${pct}`);
                }
            }

            lines.push('');

            // Assessment
            lines.push('FINDINGS:');
            if (totalMatches === 0) {
                lines.push('  [INFO] No errors matching the pattern found in recent logs.');
            } else {
                if (podsWithErrors === pods.length) {
                    lines.push(`  [CRITICAL] All ${pods.length} pod(s) are logging errors`);
                } else if (podsWithErrors > 0) {
                    lines.push(`  [WARNING] ${podsWithErrors}/${pods.length} pod(s) are logging errors`);
                }

                if (allErrorTypes['Fatal'] || allErrorTypes['Panic']) {
                    const fatalCount = (allErrorTypes['Fatal'] || 0) + (allErrorTypes['Panic'] || 0);
                    lines.push(`  [CRITICAL] ${fatalCount} fatal/panic errors detected`);
                }
                if (allErrorTypes['Timeout'] || allErrorTypes['Refused']) {
                    const connCount = (allErrorTypes['Timeout'] || 0) + (allErrorTypes['Refused'] || 0);
                    lines.push(`  [WARNING] ${connCount} timeout/connection-refused errors — possible connectivity issues`);
                }
            }

            lines.push('');
            lines.push('SUGGESTED ACTIONS:');
            let actionNum = 1;
            if (allErrorTypes['Fatal'] || allErrorTypes['Panic']) {
                lines.push(`${actionNum++}. Investigate fatal/panic errors immediately — the application may be crashing`);
            }
            if (allErrorTypes['Timeout'] || allErrorTypes['Refused']) {
                lines.push(`${actionNum++}. Check network connectivity and dependent service availability`);
            }
            if (allErrorTypes['Exception']) {
                lines.push(`${actionNum++}. Review exception stack traces for application bugs`);
            }
            if (totalMatches > 50) {
                lines.push(`${actionNum++}. High error volume (${totalMatches} matches) — consider increasing log verbosity or adding structured logging`);
            }
            if (actionNum === 1) {
                lines.push('  No specific actions needed — logs appear clean.');
            }

            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(lines.join('\n'))]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError(`analyzing logs for deployment ${namespace}/${deployment_name}`, err))]);
        }
    }
}
