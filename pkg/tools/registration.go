package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/flux"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
)

// RegisterAll registers all MCP tools with the server.
// fluxClient may be nil if FluxCD is not available.
func RegisterAll(server *mcp.Server, client *k8s.ClusterClient, fluxClient *flux.FluxClient) {
	registerClusterTools(server, client)
	registerPodTools(server, client)
	registerEventTools(server, client)
	registerWorkloadTools(server, client)
	registerNodeTools(server, client)
	registerNetworkingTools(server, client)
	registerStorageTools(server, client)
	registerMetricsTools(server, client)
	registerDiagnosticTools(server, client)
	registerPolicyTools(server, client)
	registerSecurityTools(server, client)
	registerResourceTools(server, client)
	registerDiscoveryTools(server, client)
	registerNetworkAnalysisTools(server, client)
	registerResourceAnalysisTools(server, client)
	registerCompositeDiagnosticTools(server, client)
	if fluxClient != nil {
		registerFluxTools(server, fluxClient, client)
	}
}
