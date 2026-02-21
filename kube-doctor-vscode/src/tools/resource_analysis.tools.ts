import * as vscode from 'vscode';
import * as k8s from '@kubernetes/client-node';
import { listPods, isPodHealthy, podPhaseReason, podContainerSummary } from '../k8s/pods';
import { listNodes, nodeStatus } from '../k8s/nodes';
import { getNodeMetrics, getPodMetrics, parseCPU, parseMemory, formatBytes, NodeMetrics, PodMetrics } from '../k8s/metrics';
import { listNetworkPolicies } from '../k8s/network_policies';
import { getPodLogs } from '../k8s/logs';
import { formatAge, formatTable, formatError } from '../util/formatting';
import { mermaidSafeId } from '../k8s/network_analysis';

// ---- analyze_resource_usage ----

interface AnalyzeResourceUsageInput { namespace: string; }

export class AnalyzeResourceUsageTool implements vscode.LanguageModelTool<AnalyzeResourceUsageInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeResourceUsageInput>): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Analyzing resource usage in ${options.input.namespace || 'all namespaces'}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<AnalyzeResourceUsageInput>): Promise<vscode.LanguageModelToolResult> {
        try {
            const ns = options.input.namespace;
            const pods = await listPods(ns);
            const metrics = await getPodMetrics(ns);

            // Build a metrics lookup by pod name + namespace
            const metricsMap: Record<string, { cpu: number; memory: number }> = {};
            for (const m of metrics) {
                let totalCPU = 0;
                let totalMem = 0;
                for (const c of m.containers) {
                    totalCPU += parseCPU(c.usage.cpu);
                    totalMem += parseMemory(c.usage.memory);
                }
                metricsMap[`${m.namespace}/${m.name}`] = { cpu: totalCPU, memory: totalMem };
            }

            const lines: string[] = [];
            const displayNs = !ns || ns === 'all' ? 'all' : ns;
            lines.push(`=== Resource Usage Analysis (namespace: ${displayNs}) ===`);
            lines.push('');

            const headers = ['NAME', 'CPU_REQ', 'CPU_LIM', 'CPU_USED', 'CPU%', 'MEM_REQ', 'MEM_LIM', 'MEM_USED', 'MEM%'];
            const rows: string[][] = [];
            const warnings: string[] = [];
            const criticals: string[] = [];
            const cpuUsages: { name: string; cpu: number; cpuReq: number }[] = [];

            for (const pod of pods) {
                const podName = pod.metadata?.name || '';
                const podNs = pod.metadata?.namespace || '';
                const key = `${podNs}/${podName}`;

                // Aggregate requests and limits across all containers
                let totalCPUReq = 0;
                let totalCPULim = 0;
                let totalMemReq = 0;
                let totalMemLim = 0;
                for (const c of pod.spec?.containers || []) {
                    const reqCPU = c.resources?.requests?.['cpu'];
                    const limCPU = c.resources?.limits?.['cpu'];
                    const reqMem = c.resources?.requests?.['memory'];
                    const limMem = c.resources?.limits?.['memory'];
                    if (reqCPU) { totalCPUReq += parseCPU(reqCPU); }
                    if (limCPU) { totalCPULim += parseCPU(limCPU); }
                    if (reqMem) { totalMemReq += parseMemory(reqMem); }
                    if (limMem) { totalMemLim += parseMemory(limMem); }
                }

                const usage = metricsMap[key];
                const cpuUsed = usage ? usage.cpu : 0;
                const memUsed = usage ? usage.memory : 0;

                // Calculate utilization percentages against limits (or requests if no limits)
                let cpuPct = 'N/A';
                let memPct = 'N/A';
                let cpuPctNum = 0;
                let memPctNum = 0;
                const cpuRef = totalCPULim > 0 ? totalCPULim : totalCPUReq;
                const memRef = totalMemLim > 0 ? totalMemLim : totalMemReq;

                if (cpuRef > 0 && usage) {
                    cpuPctNum = (cpuUsed / cpuRef) * 100;
                    cpuPct = `${cpuPctNum.toFixed(1)}%`;
                }
                if (memRef > 0 && usage) {
                    memPctNum = (memUsed / memRef) * 100;
                    memPct = `${memPctNum.toFixed(1)}%`;
                }

                // Flag pods based on utilization
                if (cpuPctNum > 95 || memPctNum > 95) {
                    criticals.push(`[CRITICAL] ${podName} - CPU: ${cpuPct}, MEM: ${memPct}`);
                } else if (cpuPctNum > 80 || memPctNum > 80) {
                    warnings.push(`[WARNING] ${podName} - CPU: ${cpuPct}, MEM: ${memPct}`);
                }

                cpuUsages.push({ name: podName, cpu: cpuUsed, cpuReq: totalCPUReq });

                rows.push([
                    podName,
                    totalCPUReq > 0 ? `${totalCPUReq}m` : '-',
                    totalCPULim > 0 ? `${totalCPULim}m` : '-',
                    usage ? `${cpuUsed}m` : 'N/A',
                    cpuPct,
                    totalMemReq > 0 ? formatBytes(totalMemReq) : '-',
                    totalMemLim > 0 ? formatBytes(totalMemLim) : '-',
                    usage ? formatBytes(memUsed) : 'N/A',
                    memPct,
                ]);
            }

            lines.push(formatTable(headers, rows));
            lines.push('');
            lines.push(`Found ${pods.length} pods, ${metrics.length} with metrics`);

            if (criticals.length > 0) {
                lines.push('');
                lines.push('--- Critical Utilization (>95%) ---');
                for (const c of criticals) { lines.push(c); }
            }
            if (warnings.length > 0) {
                lines.push('');
                lines.push('--- High Utilization (>80%) ---');
                for (const w of warnings) { lines.push(w); }
            }

            // Mermaid chart: top 10 pods by CPU usage
            const topCPU = [...cpuUsages]
                .sort((a, b) => b.cpu - a.cpu)
                .slice(0, 10);

            if (topCPU.length > 0) {
                const maxCPU = Math.max(...topCPU.map(t => t.cpu), ...topCPU.map(t => t.cpuReq));
                const yMax = Math.ceil(maxCPU / 100) * 100 || 1000;
                const xLabels = topCPU.map(t => `"${t.name.slice(0, 20)}"`).join(', ');
                const barValues = topCPU.map(t => t.cpu).join(', ');
                const lineValues = topCPU.map(t => t.cpuReq).join(', ');

                lines.push('');
                lines.push('```mermaid');
                lines.push(`%%{init: {'theme':'neutral'}}%%`);
                lines.push('xychart-beta');
                lines.push('    title "Top Pod CPU Usage"');
                lines.push(`    x-axis [${xLabels}]`);
                lines.push(`    y-axis "Millicores" 0 --> ${yMax}`);
                lines.push(`    bar [${barValues}]`);
                lines.push(`    line [${lineValues}]`);
                lines.push('```');
            }

            const output = lines.join('\n');
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing resource usage (requires metrics-server)', err))]);
        }
    }
}

// ---- analyze_node_capacity ----

export class AnalyzeNodeCapacityTool implements vscode.LanguageModelTool<Record<string, never>> {
    async prepareInvocation(): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: 'Analyzing node capacity...' };
    }

    async invoke(): Promise<vscode.LanguageModelToolResult> {
        try {
            const nodes = await listNodes();
            const metrics = await getNodeMetrics();
            const allPods = await listPods('all');

            // Build metrics lookup
            const metricsMap: Record<string, { cpu: number; memory: number }> = {};
            for (const m of metrics) {
                metricsMap[m.name] = { cpu: parseCPU(m.usage.cpu), memory: parseMemory(m.usage.memory) };
            }

            // Count pods per node
            const podCountMap: Record<string, number> = {};
            for (const pod of allPods) {
                const nodeName = pod.spec?.nodeName;
                if (nodeName) {
                    podCountMap[nodeName] = (podCountMap[nodeName] || 0) + 1;
                }
            }

            const lines: string[] = [];
            lines.push('=== Node Capacity Analysis ===');
            lines.push('');

            const headers = ['NODE', 'STATUS', 'ALLOC_CPU', 'USED_CPU', 'CPU%', 'ALLOC_MEM', 'USED_MEM', 'MEM%', 'PODS'];
            const rows: string[][] = [];
            const warnings: string[] = [];
            const chartNodes: { name: string; cpuPct: number; memPct: number }[] = [];

            for (const node of nodes) {
                const name = node.metadata?.name || '';
                const status = nodeStatus(node);
                const allocCPU = parseCPU(node.status?.allocatable?.['cpu'] || '0');
                const allocMem = parseMemory(node.status?.allocatable?.['memory'] || '0');
                const usage = metricsMap[name];
                const usedCPU = usage ? usage.cpu : 0;
                const usedMem = usage ? usage.memory : 0;
                const pods = podCountMap[name] || 0;

                let cpuPct = 'N/A';
                let memPct = 'N/A';
                let cpuPctNum = 0;
                let memPctNum = 0;

                if (allocCPU > 0 && usage) {
                    cpuPctNum = (usedCPU / allocCPU) * 100;
                    cpuPct = `${cpuPctNum.toFixed(1)}%`;
                }
                if (allocMem > 0 && usage) {
                    memPctNum = (usedMem / allocMem) * 100;
                    memPct = `${memPctNum.toFixed(1)}%`;
                }

                if (cpuPctNum > 80 || memPctNum > 80) {
                    warnings.push(`[WARNING] Node '${name}' is over 80% utilization - CPU: ${cpuPct}, MEM: ${memPct}`);
                }

                chartNodes.push({ name, cpuPct: cpuPctNum, memPct: memPctNum });

                rows.push([
                    name,
                    status,
                    `${allocCPU}m`,
                    usage ? `${usedCPU}m` : 'N/A',
                    cpuPct,
                    formatBytes(allocMem),
                    usage ? formatBytes(usedMem) : 'N/A',
                    memPct,
                    `${pods}`,
                ]);
            }

            lines.push(formatTable(headers, rows));
            lines.push('');
            lines.push(`Found ${nodes.length} nodes, ${metrics.length} with metrics`);

            if (warnings.length > 0) {
                lines.push('');
                for (const w of warnings) { lines.push(w); }
            }

            // Mermaid bar chart of node CPU/memory utilization
            if (chartNodes.length > 0) {
                const xLabels = chartNodes.map(n => `"${n.name.slice(0, 20)}"`).join(', ');
                const cpuBars = chartNodes.map(n => Math.round(n.cpuPct)).join(', ');
                const memBars = chartNodes.map(n => Math.round(n.memPct)).join(', ');

                lines.push('');
                lines.push('```mermaid');
                lines.push(`%%{init: {'theme':'neutral'}}%%`);
                lines.push('xychart-beta');
                lines.push('    title "Node Utilization %"');
                lines.push(`    x-axis [${xLabels}]`);
                lines.push('    y-axis "Percent" 0 --> 100');
                lines.push(`    bar [${cpuBars}]`);
                lines.push(`    line [${memBars}]`);
                lines.push('```');
            }

            const output = lines.join('\n');
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing node capacity (requires metrics-server)', err))]);
        }
    }
}

// ---- analyze_resource_efficiency ----

interface AnalyzeResourceEfficiencyInput { namespace?: string; }

export class AnalyzeResourceEfficiencyTool implements vscode.LanguageModelTool<AnalyzeResourceEfficiencyInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeResourceEfficiencyInput>): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Analyzing resource efficiency in ${options.input.namespace || 'all namespaces'}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<AnalyzeResourceEfficiencyInput>): Promise<vscode.LanguageModelToolResult> {
        try {
            const ns = options.input.namespace;
            const pods = await listPods(ns || 'all');
            const metrics = await getPodMetrics(ns);

            // Build metrics lookup
            const metricsMap: Record<string, { cpu: number; memory: number }> = {};
            for (const m of metrics) {
                let totalCPU = 0;
                let totalMem = 0;
                for (const c of m.containers) {
                    totalCPU += parseCPU(c.usage.cpu);
                    totalMem += parseMemory(c.usage.memory);
                }
                metricsMap[`${m.namespace}/${m.name}`] = { cpu: totalCPU, memory: totalMem };
            }

            const lines: string[] = [];
            const displayNs = !ns || ns === 'all' ? 'all' : ns;
            lines.push(`=== Resource Efficiency Report (namespace: ${displayNs}) ===`);
            lines.push('');

            // Track waste per namespace
            const nsWaste: Record<string, { cpuReq: number; cpuUsed: number; memReq: number; memUsed: number; podCount: number }> = {};
            let totalCPUReq = 0;
            let totalCPUUsed = 0;
            let totalMemReq = 0;
            let totalMemUsed = 0;
            let podsWithRequests = 0;
            let podsWithoutRequests = 0;
            let podsWithMetrics = 0;

            for (const pod of pods) {
                const podName = pod.metadata?.name || '';
                const podNs = pod.metadata?.namespace || '';
                const key = `${podNs}/${podName}`;

                // Aggregate requests across containers
                let podCPUReq = 0;
                let podMemReq = 0;
                let hasRequests = false;
                for (const c of pod.spec?.containers || []) {
                    const reqCPU = c.resources?.requests?.['cpu'];
                    const reqMem = c.resources?.requests?.['memory'];
                    if (reqCPU) { podCPUReq += parseCPU(reqCPU); hasRequests = true; }
                    if (reqMem) { podMemReq += parseMemory(reqMem); hasRequests = true; }
                }

                if (hasRequests) { podsWithRequests++; } else { podsWithoutRequests++; }

                const usage = metricsMap[key];
                const podCPUUsed = usage ? usage.cpu : 0;
                const podMemUsed = usage ? usage.memory : 0;
                if (usage) { podsWithMetrics++; }

                totalCPUReq += podCPUReq;
                totalCPUUsed += podCPUUsed;
                totalMemReq += podMemReq;
                totalMemUsed += podMemUsed;

                // Accumulate per-namespace
                if (!nsWaste[podNs]) {
                    nsWaste[podNs] = { cpuReq: 0, cpuUsed: 0, memReq: 0, memUsed: 0, podCount: 0 };
                }
                nsWaste[podNs].cpuReq += podCPUReq;
                nsWaste[podNs].cpuUsed += podCPUUsed;
                nsWaste[podNs].memReq += podMemReq;
                nsWaste[podNs].memUsed += podMemUsed;
                nsWaste[podNs].podCount++;
            }

            // Cluster-wide summary
            lines.push('--- Cluster-wide Summary ---');
            lines.push(`  Total pods: ${pods.length}`);
            lines.push(`  Pods with resource requests: ${podsWithRequests}`);
            lines.push(`  Pods without resource requests: ${podsWithoutRequests}`);
            lines.push(`  Pods with metrics: ${podsWithMetrics}`);
            lines.push('');

            const cpuWaste = totalCPUReq - totalCPUUsed;
            const memWaste = totalMemReq - totalMemUsed;
            const cpuEfficiency = totalCPUReq > 0 ? ((totalCPUUsed / totalCPUReq) * 100).toFixed(1) : 'N/A';
            const memEfficiency = totalMemReq > 0 ? ((totalMemUsed / totalMemReq) * 100).toFixed(1) : 'N/A';

            lines.push('--- Resource Utilization vs Requests ---');
            lines.push(`  CPU Requested: ${totalCPUReq}m | Used: ${totalCPUUsed}m | Waste: ${cpuWaste > 0 ? cpuWaste : 0}m | Efficiency: ${cpuEfficiency}%`);
            lines.push(`  MEM Requested: ${formatBytes(totalMemReq)} | Used: ${formatBytes(totalMemUsed)} | Waste: ${formatBytes(memWaste > 0 ? memWaste : 0)} | Efficiency: ${memEfficiency}%`);
            lines.push('');

            // Per-namespace breakdown
            const nsEntries = Object.entries(nsWaste).sort((a, b) => {
                const wasteA = (a[1].cpuReq - a[1].cpuUsed) + ((a[1].memReq - a[1].memUsed) / (1024 * 1024));
                const wasteB = (b[1].cpuReq - b[1].cpuUsed) + ((b[1].memReq - b[1].memUsed) / (1024 * 1024));
                return wasteB - wasteA;
            });

            const nsHeaders = ['NAMESPACE', 'PODS', 'CPU_REQ', 'CPU_USED', 'CPU_WASTE', 'CPU_EFF%', 'MEM_REQ', 'MEM_USED', 'MEM_WASTE', 'MEM_EFF%'];
            const nsRows: string[][] = [];
            for (const [nsName, data] of nsEntries) {
                const nsCPUWaste = Math.max(data.cpuReq - data.cpuUsed, 0);
                const nsMemWaste = Math.max(data.memReq - data.memUsed, 0);
                const nsCPUEff = data.cpuReq > 0 ? ((data.cpuUsed / data.cpuReq) * 100).toFixed(1) : 'N/A';
                const nsMemEff = data.memReq > 0 ? ((data.memUsed / data.memReq) * 100).toFixed(1) : 'N/A';
                nsRows.push([
                    nsName,
                    `${data.podCount}`,
                    `${data.cpuReq}m`,
                    `${data.cpuUsed}m`,
                    `${nsCPUWaste}m`,
                    `${nsCPUEff}%`,
                    formatBytes(data.memReq),
                    formatBytes(data.memUsed),
                    formatBytes(nsMemWaste),
                    `${nsMemEff}%`,
                ]);
            }

            lines.push('--- Per-Namespace Breakdown ---');
            lines.push(formatTable(nsHeaders, nsRows));
            lines.push('');

            // Findings
            lines.push('FINDINGS:');
            let findingCount = 0;

            if (podsWithoutRequests > 0) {
                lines.push(`[WARNING] ${podsWithoutRequests} pods have no resource requests set. This can lead to scheduling issues and resource contention.`);
                findingCount++;
            }

            const cpuEffNum = totalCPUReq > 0 ? (totalCPUUsed / totalCPUReq) * 100 : 100;
            const memEffNum = totalMemReq > 0 ? (totalMemUsed / totalMemReq) * 100 : 100;

            if (cpuEffNum < 30 && totalCPUReq > 0) {
                lines.push(`[WARNING] CPU efficiency is very low (${cpuEffNum.toFixed(1)}%). Consider reducing CPU requests.`);
                findingCount++;
            } else if (cpuEffNum < 50 && totalCPUReq > 0) {
                lines.push(`[INFO] CPU efficiency is moderate (${cpuEffNum.toFixed(1)}%). There may be room to optimize requests.`);
                findingCount++;
            }

            if (memEffNum < 30 && totalMemReq > 0) {
                lines.push(`[WARNING] Memory efficiency is very low (${memEffNum.toFixed(1)}%). Consider reducing memory requests.`);
                findingCount++;
            } else if (memEffNum < 50 && totalMemReq > 0) {
                lines.push(`[INFO] Memory efficiency is moderate (${memEffNum.toFixed(1)}%). There may be room to optimize requests.`);
                findingCount++;
            }

            // Highlight namespaces with worst efficiency
            for (const [nsName, data] of nsEntries) {
                const nsCPUEff = data.cpuReq > 0 ? (data.cpuUsed / data.cpuReq) * 100 : 100;
                if (nsCPUEff < 20 && data.cpuReq > 100) {
                    lines.push(`[WARNING] Namespace '${nsName}' has very low CPU efficiency (${nsCPUEff.toFixed(1)}%) with ${data.cpuReq}m requested.`);
                    findingCount++;
                }
            }

            if (findingCount === 0) {
                lines.push('  No efficiency concerns found. Resource requests appear well-tuned.');
            }

            const output = lines.join('\n');
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing resource efficiency (requires metrics-server)', err))]);
        }
    }
}

// ---- analyze_network_policies ----

interface AnalyzeNetworkPoliciesInput { namespace: string; }

export class AnalyzeNetworkPoliciesTool implements vscode.LanguageModelTool<AnalyzeNetworkPoliciesInput> {
    async prepareInvocation(options: vscode.LanguageModelToolInvocationPrepareOptions<AnalyzeNetworkPoliciesInput>): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: `Analyzing network policies in ${options.input.namespace || 'all namespaces'}...` };
    }

    async invoke(options: vscode.LanguageModelToolInvocationOptions<AnalyzeNetworkPoliciesInput>): Promise<vscode.LanguageModelToolResult> {
        try {
            const ns = options.input.namespace;
            const policies = await listNetworkPolicies(ns);
            const pods = await listPods(ns);

            const lines: string[] = [];
            const displayNs = !ns || ns === 'all' ? 'all' : ns;
            lines.push(`=== Network Policy Analysis (namespace: ${displayNs}) ===`);
            lines.push('');
            lines.push(`Found ${policies.length} network policies, ${pods.length} pods`);
            lines.push('');

            // Track which pods are covered by at least one policy
            const coveredPods = new Set<string>();

            // Mermaid edges for the flowchart
            const mermaidNodes = new Set<string>();
            const mermaidEdges: string[] = [];
            const mermaidSubgraphs: Record<string, Set<string>> = {};

            for (const policy of policies) {
                const policyName = policy.metadata?.name || '';
                const policyNs = policy.metadata?.namespace || '';
                lines.push(`--- Policy: ${policyName} (namespace: ${policyNs}) ---`);

                // Pod selector
                const podSelector = policy.spec?.podSelector?.matchLabels || {};
                const selectorStr = Object.entries(podSelector).map(([k, v]) => `${k}=${v}`).join(', ');
                lines.push(`  Pod Selector: ${selectorStr || '<all pods>'}`);

                // Determine which pods match this policy
                const selectorId = selectorStr || 'all-pods';
                const safeSelId = mermaidSafeId(selectorId);
                if (!mermaidSubgraphs[policyNs]) { mermaidSubgraphs[policyNs] = new Set(); }
                mermaidSubgraphs[policyNs].add(safeSelId);
                mermaidNodes.add(safeSelId);

                for (const pod of pods) {
                    if (pod.metadata?.namespace !== policyNs) { continue; }
                    const podLabels = pod.metadata?.labels || {};
                    let matches = true;
                    for (const [k, v] of Object.entries(podSelector)) {
                        if (podLabels[k] !== v) { matches = false; break; }
                    }
                    if (matches) {
                        coveredPods.add(`${pod.metadata?.namespace}/${pod.metadata?.name}`);
                    }
                }

                // Policy types
                const policyTypes = policy.spec?.policyTypes || [];
                lines.push(`  Policy Types: ${policyTypes.join(', ') || 'N/A'}`);

                // Ingress rules
                if (policy.spec?.ingress) {
                    lines.push('  Ingress Rules:');
                    if (policy.spec.ingress.length === 0) {
                        lines.push('    - Deny all ingress');
                    }
                    for (const rule of policy.spec.ingress) {
                        const ports = (rule.ports || []).map(p => `${p.port || '*'}/${p.protocol || 'TCP'}`).join(', ');
                        const portStr = ports ? ` on port ${ports}` : '';
                        if (!rule._from || rule._from.length === 0) {
                            lines.push(`    - Allow all sources${portStr}`);
                        } else {
                            for (const from of rule._from) {
                                if (from.podSelector) {
                                    const fromLabels = Object.entries(from.podSelector.matchLabels || {}).map(([k, v]) => `${k}=${v}`).join(', ');
                                    const fromStr = fromLabels || 'all pods';
                                    lines.push(`    - Allow from pods (${fromStr})${portStr}`);
                                    const safeFromId = mermaidSafeId(fromStr);
                                    mermaidNodes.add(safeFromId);
                                    if (!mermaidSubgraphs[policyNs]) { mermaidSubgraphs[policyNs] = new Set(); }
                                    mermaidSubgraphs[policyNs].add(safeFromId);
                                    mermaidEdges.push(`    ${safeFromId}[${fromStr}] -->|${ports || 'all'}| ${safeSelId}[${selectorId}]`);
                                } else if (from.namespaceSelector) {
                                    const nsLabels = Object.entries(from.namespaceSelector.matchLabels || {}).map(([k, v]) => `${k}=${v}`).join(', ');
                                    lines.push(`    - Allow from namespaces (${nsLabels || 'all'})${portStr}`);
                                    const safeNsId = mermaidSafeId(`ns_${nsLabels || 'all'}`);
                                    mermaidNodes.add(safeNsId);
                                    mermaidEdges.push(`    ${safeNsId}[ns: ${nsLabels || 'all'}] -->|${ports || 'all'}| ${safeSelId}[${selectorId}]`);
                                } else if (from.ipBlock) {
                                    lines.push(`    - Allow from CIDR ${from.ipBlock.cidr}${portStr}`);
                                    const safeCidrId = mermaidSafeId(`cidr_${from.ipBlock.cidr}`);
                                    mermaidEdges.push(`    ${safeCidrId}[${from.ipBlock.cidr}] -->|${ports || 'all'}| ${safeSelId}[${selectorId}]`);
                                }
                            }
                        }
                    }
                } else if (policyTypes.includes('Ingress')) {
                    lines.push('  Ingress Rules: Deny all ingress (no ingress rules defined)');
                }

                // Egress rules
                if (policy.spec?.egress) {
                    lines.push('  Egress Rules:');
                    if (policy.spec.egress.length === 0) {
                        lines.push('    - Deny all egress');
                    }
                    for (const rule of policy.spec.egress) {
                        const ports = (rule.ports || []).map(p => `${p.port || '*'}/${p.protocol || 'TCP'}`).join(', ');
                        const portStr = ports ? ` on port ${ports}` : '';
                        if (!rule.to || rule.to.length === 0) {
                            lines.push(`    - Allow to all destinations${portStr}`);
                        } else {
                            for (const to of rule.to) {
                                if (to.podSelector) {
                                    const toLabels = Object.entries(to.podSelector.matchLabels || {}).map(([k, v]) => `${k}=${v}`).join(', ');
                                    const toStr = toLabels || 'all pods';
                                    lines.push(`    - Allow to pods (${toStr})${portStr}`);
                                    const safeToId = mermaidSafeId(toStr);
                                    mermaidNodes.add(safeToId);
                                    if (!mermaidSubgraphs[policyNs]) { mermaidSubgraphs[policyNs] = new Set(); }
                                    mermaidSubgraphs[policyNs].add(safeToId);
                                    mermaidEdges.push(`    ${safeSelId}[${selectorId}] -->|${ports || 'all'}| ${safeToId}[${toStr}]`);
                                } else if (to.namespaceSelector) {
                                    const nsLabels = Object.entries(to.namespaceSelector.matchLabels || {}).map(([k, v]) => `${k}=${v}`).join(', ');
                                    lines.push(`    - Allow to namespaces (${nsLabels || 'all'})${portStr}`);
                                } else if (to.ipBlock) {
                                    lines.push(`    - Allow to CIDR ${to.ipBlock.cidr}${portStr}`);
                                }
                            }
                        }
                    }
                } else if (policyTypes.includes('Egress')) {
                    lines.push('  Egress Rules: Deny all egress (no egress rules defined)');
                }

                lines.push('');
            }

            // Check for uncovered pods
            const uncoveredPods = pods.filter(p => !coveredPods.has(`${p.metadata?.namespace}/${p.metadata?.name}`));
            if (uncoveredPods.length > 0) {
                lines.push('--- Pods NOT Covered by Any Network Policy ---');
                lines.push(`[WARNING] ${uncoveredPods.length} pods have no network policy applied:`);
                for (const p of uncoveredPods.slice(0, 20)) {
                    lines.push(`  - ${p.metadata?.namespace}/${p.metadata?.name}`);
                }
                if (uncoveredPods.length > 20) {
                    lines.push(`  ... and ${uncoveredPods.length - 20} more`);
                }
                lines.push('');
            } else if (pods.length > 0 && policies.length > 0) {
                lines.push('All pods are covered by at least one network policy.');
                lines.push('');
            }

            // Mermaid flowchart
            if (mermaidEdges.length > 0) {
                lines.push('```mermaid');
                lines.push('graph LR');
                for (const [sgNs, sgNodes] of Object.entries(mermaidSubgraphs)) {
                    lines.push(`    subgraph ${mermaidSafeId(sgNs)}["${sgNs}"]`);
                    // Nodes are referenced in edges, subgraph just groups them
                    for (const nodeId of sgNodes) {
                        lines.push(`        ${nodeId}`);
                    }
                    lines.push('    end');
                }
                // Deduplicate edges
                const uniqueEdges = [...new Set(mermaidEdges)];
                for (const edge of uniqueEdges) {
                    lines.push(edge);
                }
                lines.push('```');
            }

            const output = lines.join('\n');
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('analyzing network policies', err))]);
        }
    }
}

// ---- check_dns_health ----

export class CheckDNSHealthTool implements vscode.LanguageModelTool<Record<string, never>> {
    async prepareInvocation(): Promise<vscode.PreparedToolInvocation> {
        return { invocationMessage: 'Checking DNS health...' };
    }

    async invoke(): Promise<vscode.LanguageModelToolResult> {
        try {
            const lines: string[] = [];
            lines.push('=== DNS Health Report ===');
            lines.push('');

            // Find CoreDNS pods in kube-system
            const dnsPods = await listPods('kube-system', 'k8s-app=kube-dns');
            lines.push(`Found ${dnsPods.length} CoreDNS pods`);
            lines.push('');

            if (dnsPods.length === 0) {
                lines.push('[CRITICAL] No CoreDNS pods found in kube-system with label k8s-app=kube-dns');
                lines.push('');
                lines.push('SUGGESTED ACTIONS:');
                lines.push('1. Verify CoreDNS is deployed: check kube-system namespace for DNS pods');
                lines.push('2. Check if CoreDNS uses different labels in your cluster');
                lines.push('3. Ensure CoreDNS deployment has not been scaled to zero');
                const output = lines.join('\n');
                return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
            }

            let findings = 0;
            const errorPatterns = ['SERVFAIL', 'NXDOMAIN', 'timeout', 'refused', 'connection refused', 'i/o timeout', 'no such host'];

            // Check each DNS pod
            lines.push('--- Pod Status ---');
            const headers = ['POD', 'STATUS', 'READY', 'RESTARTS', 'AGE', 'NODE'];
            const rows: string[][] = [];
            for (const pod of dnsPods) {
                const { ready, total, restarts } = podContainerSummary(pod);
                const phase = podPhaseReason(pod);
                const name = pod.metadata?.name || '';
                rows.push([
                    name,
                    phase,
                    `${ready}/${total}`,
                    `${restarts}`,
                    formatAge(pod.metadata?.creationTimestamp),
                    pod.spec?.nodeName || '',
                ]);

                if (!isPodHealthy(pod)) {
                    lines.push(`[CRITICAL] CoreDNS pod '${name}' is unhealthy: ${phase}`);
                    findings++;
                }
                if (restarts > 5) {
                    lines.push(`[WARNING] CoreDNS pod '${name}' has high restart count: ${restarts}`);
                    findings++;
                }
            }
            lines.push(formatTable(headers, rows));
            lines.push('');

            // Scan logs for error patterns
            lines.push('--- Log Analysis ---');
            const logErrors: Record<string, number> = {};
            let totalLogLines = 0;
            let totalErrorLines = 0;

            for (const pod of dnsPods) {
                const podName = pod.metadata?.name || '';
                try {
                    const logs = await getPodLogs('kube-system', podName, undefined, 200);
                    const logLines = logs.split('\n').filter(l => l.trim().length > 0);
                    totalLogLines += logLines.length;

                    for (const line of logLines) {
                        for (const pattern of errorPatterns) {
                            if (line.toLowerCase().includes(pattern.toLowerCase())) {
                                logErrors[pattern] = (logErrors[pattern] || 0) + 1;
                                totalErrorLines++;
                                break;
                            }
                        }
                    }
                } catch {
                    lines.push(`  (could not fetch logs for ${podName})`);
                }
            }

            if (Object.keys(logErrors).length > 0) {
                lines.push(`  Scanned ${totalLogLines} log lines, found ${totalErrorLines} lines with error patterns:`);
                for (const [pattern, count] of Object.entries(logErrors).sort((a, b) => b[1] - a[1])) {
                    const severity = (pattern === 'SERVFAIL' || pattern === 'timeout' || pattern === 'i/o timeout') ? 'WARNING' : 'INFO';
                    lines.push(`  [${severity}] ${pattern}: ${count} occurrences`);
                    if (severity === 'WARNING') { findings++; }
                }
            } else {
                lines.push(`  Scanned ${totalLogLines} log lines, no error patterns found.`);
            }
            lines.push('');

            // Overall assessment
            lines.push('--- Assessment ---');
            if (findings === 0) {
                lines.push('  DNS appears healthy. All CoreDNS pods are running with no significant errors.');
            } else {
                lines.push(`  ${findings} issue(s) found. Review findings above.`);
            }
            lines.push('');

            // Suggested actions
            lines.push('SUGGESTED ACTIONS:');
            let actionNum = 1;

            for (const pod of dnsPods) {
                if (!isPodHealthy(pod)) {
                    lines.push(`${actionNum++}. Investigate unhealthy CoreDNS pod '${pod.metadata?.name}' - check events and describe pod`);
                }
            }

            if (logErrors['SERVFAIL']) {
                lines.push(`${actionNum++}. SERVFAIL errors detected - check upstream DNS configuration in CoreDNS ConfigMap (Corefile)`);
            }
            if (logErrors['timeout'] || logErrors['i/o timeout']) {
                lines.push(`${actionNum++}. Timeout errors detected - check network connectivity to upstream DNS servers and CoreDNS resource limits`);
            }
            if (logErrors['NXDOMAIN']) {
                lines.push(`${actionNum++}. NXDOMAIN responses detected (may be normal) - verify application DNS queries are using correct service names`);
            }
            if (logErrors['refused'] || logErrors['connection refused']) {
                lines.push(`${actionNum++}. Connection refused errors - check if upstream DNS servers are reachable and CoreDNS service endpoints are healthy`);
            }

            if (dnsPods.length < 2) {
                lines.push(`${actionNum++}. Only ${dnsPods.length} CoreDNS replica(s) running - consider scaling to at least 2 for high availability`);
            }

            if (actionNum === 1) {
                lines.push('  No specific actions needed - DNS is healthy.');
            }

            const output = lines.join('\n');
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(output)]);
        } catch (err) {
            return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(formatError('checking DNS health', err))]);
        }
    }
}
