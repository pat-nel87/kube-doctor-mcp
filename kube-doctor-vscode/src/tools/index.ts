import * as vscode from 'vscode';
import { ListNamespacesTool, ListNodesTool, ListContextsTool, ClusterInfoTool, GetNodeDetailTool } from './cluster.tools';
import { ListPodsTool, GetPodLogsTool, GetPodDetailTool } from './pod.tools';
import { GetEventsTool } from './event.tools';
import { ListDeploymentsTool, GetDeploymentDetailTool, ListStatefulSetsTool, ListDaemonSetsTool, ListJobsTool } from './workload.tools';
import { ListServicesTool } from './network.tools';
import { ListIngressesTool, GetEndpointsTool } from './network_extra.tools';
import { ListPVCsTool, ListPVsTool } from './storage.tools';
import { GetNodeMetricsTool, GetPodMetricsTool, TopResourceConsumersTool } from './metrics.tools';
import { AnalyzeResourceAllocationTool, ListLimitRangesTool } from './resources.tools';
import { ListCRDsTool, GetAPIResourcesTool, ListWebhookConfigsTool } from './discovery.tools';
import { DiagnosePodTool, DiagnoseNamespaceTool, DiagnoseClusterTool, FindUnhealthyPodsTool } from './diagnostics.tools';
import { CheckResourceQuotasTool } from './diagnostics_extra.tools';
import { ListNetworkPoliciesTool } from './policy.tools';
import { ListHPAsTool, ListPDBsTool } from './policy_extra.tools';
import { AnalyzePodConnectivityTool } from './connectivity.tools';
import { AnalyzePodSecurityTool } from './security.tools';
import { ListRBACBindingsTool, AuditNamespaceSecurityTool } from './security_extra.tools';
import { GetWorkloadDependenciesTool } from './dependencies.tools';
import { ListFluxKustomizationsTool, ListFluxHelmReleasesTool, ListFluxSourcesTool, ListFluxImagePoliciesTool, DiagnoseFluxKustomizationTool, DiagnoseFluxHelmReleaseTool, DiagnoseFluxSystemTool, GetFluxResourceTreeTool } from './flux.tools';
import { MapServiceTopologyTool, TraceIngressToBackendTool, ListEndpointHealthTool, AnalyzeServiceConnectivityTool, AnalyzeAllIngressesTool, CheckAGICHealthTool } from './network_analysis.tools';
import { AnalyzeResourceUsageTool, AnalyzeNodeCapacityTool, AnalyzeResourceEfficiencyTool, AnalyzeNetworkPoliciesTool, CheckDNSHealthTool } from './resource_analysis.tools';
import { DiagnoseRequestPathTool, DiagnoseServiceTool, ClusterHealthOverviewTool, AnalyzeServiceLogsTool } from './composite_diagnostics.tools';

export function registerAllTools(context: vscode.ExtensionContext): void {
    // Each name MUST match the "name" in package.json contributes.languageModelTools
    context.subscriptions.push(
        // Cluster discovery
        vscode.lm.registerTool('kube-doctor_listNamespaces', new ListNamespacesTool()),
        vscode.lm.registerTool('kube-doctor_listNodes', new ListNodesTool()),
        vscode.lm.registerTool('kube-doctor_listContexts', new ListContextsTool()),
        vscode.lm.registerTool('kube-doctor_clusterInfo', new ClusterInfoTool()),
        vscode.lm.registerTool('kube-doctor_getNodeDetail', new GetNodeDetailTool()),

        // Pods
        vscode.lm.registerTool('kube-doctor_listPods', new ListPodsTool()),
        vscode.lm.registerTool('kube-doctor_getPodLogs', new GetPodLogsTool()),
        vscode.lm.registerTool('kube-doctor_getPodDetail', new GetPodDetailTool()),

        // Events
        vscode.lm.registerTool('kube-doctor_getEvents', new GetEventsTool()),

        // Workloads
        vscode.lm.registerTool('kube-doctor_listDeployments', new ListDeploymentsTool()),
        vscode.lm.registerTool('kube-doctor_getDeploymentDetail', new GetDeploymentDetailTool()),
        vscode.lm.registerTool('kube-doctor_listStatefulSets', new ListStatefulSetsTool()),
        vscode.lm.registerTool('kube-doctor_listDaemonSets', new ListDaemonSetsTool()),
        vscode.lm.registerTool('kube-doctor_listJobs', new ListJobsTool()),

        // Networking
        vscode.lm.registerTool('kube-doctor_listServices', new ListServicesTool()),
        vscode.lm.registerTool('kube-doctor_listIngresses', new ListIngressesTool()),
        vscode.lm.registerTool('kube-doctor_getEndpoints', new GetEndpointsTool()),

        // Storage
        vscode.lm.registerTool('kube-doctor_listPVCs', new ListPVCsTool()),
        vscode.lm.registerTool('kube-doctor_listPVs', new ListPVsTool()),

        // Metrics
        vscode.lm.registerTool('kube-doctor_getNodeMetrics', new GetNodeMetricsTool()),
        vscode.lm.registerTool('kube-doctor_getPodMetrics', new GetPodMetricsTool()),
        vscode.lm.registerTool('kube-doctor_topResourceConsumers', new TopResourceConsumersTool()),

        // Resource analysis
        vscode.lm.registerTool('kube-doctor_analyzeResourceAllocation', new AnalyzeResourceAllocationTool()),
        vscode.lm.registerTool('kube-doctor_listLimitRanges', new ListLimitRangesTool()),
        vscode.lm.registerTool('kube-doctor_checkResourceQuotas', new CheckResourceQuotasTool()),

        // API discovery
        vscode.lm.registerTool('kube-doctor_listCRDs', new ListCRDsTool()),
        vscode.lm.registerTool('kube-doctor_getAPIResources', new GetAPIResourcesTool()),
        vscode.lm.registerTool('kube-doctor_listWebhookConfigs', new ListWebhookConfigsTool()),

        // Diagnostics
        vscode.lm.registerTool('kube-doctor_diagnosePod', new DiagnosePodTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseNamespace', new DiagnoseNamespaceTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseCluster', new DiagnoseClusterTool()),
        vscode.lm.registerTool('kube-doctor_findUnhealthyPods', new FindUnhealthyPodsTool()),

        // Policy & autoscaling
        vscode.lm.registerTool('kube-doctor_listNetworkPolicies', new ListNetworkPoliciesTool()),
        vscode.lm.registerTool('kube-doctor_listHPAs', new ListHPAsTool()),
        vscode.lm.registerTool('kube-doctor_listPDBs', new ListPDBsTool()),

        // Security & RBAC
        vscode.lm.registerTool('kube-doctor_analyzePodConnectivity', new AnalyzePodConnectivityTool()),
        vscode.lm.registerTool('kube-doctor_analyzePodSecurity', new AnalyzePodSecurityTool()),
        vscode.lm.registerTool('kube-doctor_listRBACBindings', new ListRBACBindingsTool()),
        vscode.lm.registerTool('kube-doctor_auditNamespaceSecurity', new AuditNamespaceSecurityTool()),

        // Dependencies
        vscode.lm.registerTool('kube-doctor_getWorkloadDependencies', new GetWorkloadDependenciesTool()),

        // FluxCD GitOps
        vscode.lm.registerTool('kube-doctor_listFluxKustomizations', new ListFluxKustomizationsTool()),
        vscode.lm.registerTool('kube-doctor_listFluxHelmReleases', new ListFluxHelmReleasesTool()),
        vscode.lm.registerTool('kube-doctor_listFluxSources', new ListFluxSourcesTool()),
        vscode.lm.registerTool('kube-doctor_listFluxImagePolicies', new ListFluxImagePoliciesTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseFluxKustomization', new DiagnoseFluxKustomizationTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseFluxHelmRelease', new DiagnoseFluxHelmReleaseTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseFluxSystem', new DiagnoseFluxSystemTool()),
        vscode.lm.registerTool('kube-doctor_getFluxResourceTree', new GetFluxResourceTreeTool()),

        // Network Analysis & Topology
        vscode.lm.registerTool('kube-doctor_mapServiceTopology', new MapServiceTopologyTool()),
        vscode.lm.registerTool('kube-doctor_traceIngressToBackend', new TraceIngressToBackendTool()),
        vscode.lm.registerTool('kube-doctor_listEndpointHealth', new ListEndpointHealthTool()),
        vscode.lm.registerTool('kube-doctor_analyzeServiceConnectivity', new AnalyzeServiceConnectivityTool()),
        vscode.lm.registerTool('kube-doctor_analyzeAllIngresses', new AnalyzeAllIngressesTool()),
        vscode.lm.registerTool('kube-doctor_checkAGICHealth', new CheckAGICHealthTool()),

        // Resource Analysis
        vscode.lm.registerTool('kube-doctor_analyzeResourceUsage', new AnalyzeResourceUsageTool()),
        vscode.lm.registerTool('kube-doctor_analyzeNodeCapacity', new AnalyzeNodeCapacityTool()),
        vscode.lm.registerTool('kube-doctor_analyzeResourceEfficiency', new AnalyzeResourceEfficiencyTool()),
        vscode.lm.registerTool('kube-doctor_analyzeNetworkPolicies', new AnalyzeNetworkPoliciesTool()),
        vscode.lm.registerTool('kube-doctor_checkDNSHealth', new CheckDNSHealthTool()),

        // Composite Diagnostics
        vscode.lm.registerTool('kube-doctor_diagnoseRequestPath', new DiagnoseRequestPathTool()),
        vscode.lm.registerTool('kube-doctor_diagnoseService', new DiagnoseServiceTool()),
        vscode.lm.registerTool('kube-doctor_clusterHealthOverview', new ClusterHealthOverviewTool()),
        vscode.lm.registerTool('kube-doctor_analyzeServiceLogs', new AnalyzeServiceLogsTool()),
    );
}
