package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
)

// RegisterAll registers all MCP tools with the server.
func RegisterAll(server *mcp.Server, client *k8s.ClusterClient) {
	registerClusterTools(server, client)
	registerPodTools(server, client)
	registerEventTools(server, client)
	registerWorkloadTools(server, client)
	registerNodeTools(server, client)
	registerNetworkingTools(server, client)
	registerStorageTools(server, client)
	registerMetricsTools(server, client)
	registerDiagnosticTools(server, client)
}
