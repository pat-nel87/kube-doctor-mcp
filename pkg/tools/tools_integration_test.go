//go:build integration

package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
)

// Run with: go test ./pkg/tools/ -tags integration -v -run TestToolsLive
func TestToolsLive(t *testing.T) {
	client, err := k8s.NewClusterClient("")
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "kube-doctor-test",
		Version: "test",
	}, nil)

	RegisterAll(server, client)

	ctx := context.Background()

	// Set up in-memory client-server connection
	t1, t2 := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("Server connect: %v", err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "test",
	}, nil)

	clientSession, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("Client connect: %v", err)
	}
	defer clientSession.Close()

	callTool := func(t *testing.T, name string, args map[string]any) string {
		t.Helper()
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		})
		if err != nil {
			t.Fatalf("CallTool(%s) error: %v", name, err)
		}
		if result.IsError {
			for _, c := range result.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					t.Fatalf("Tool returned error: %s", tc.Text)
				}
			}
		}
		for _, c := range result.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				return tc.Text
			}
		}
		t.Fatal("no text content in result")
		return ""
	}

	t.Run("list_contexts", func(t *testing.T) {
		text := callTool(t, "list_contexts", nil)
		fmt.Println(text)
	})

	t.Run("list_namespaces", func(t *testing.T) {
		text := callTool(t, "list_namespaces", nil)
		fmt.Println(text)
	})

	t.Run("cluster_info", func(t *testing.T) {
		text := callTool(t, "cluster_info", nil)
		fmt.Println(text)
	})

	t.Run("list_pods_all", func(t *testing.T) {
		text := callTool(t, "list_pods", map[string]any{"namespace": "all"})
		fmt.Println(text)
	})

	t.Run("list_nodes", func(t *testing.T) {
		text := callTool(t, "list_nodes", nil)
		fmt.Println(text)
	})

	t.Run("get_events_warnings", func(t *testing.T) {
		text := callTool(t, "get_events", map[string]any{"event_type": "Warning"})
		fmt.Println(text)
	})

	t.Run("list_deployments_all", func(t *testing.T) {
		text := callTool(t, "list_deployments", map[string]any{"namespace": "all"})
		fmt.Println(text)
	})

	t.Run("list_services_all", func(t *testing.T) {
		text := callTool(t, "list_services", map[string]any{"namespace": "all"})
		fmt.Println(text)
	})

	t.Run("find_unhealthy_pods", func(t *testing.T) {
		text := callTool(t, "find_unhealthy_pods", nil)
		fmt.Println(text)
	})

	t.Run("diagnose_namespace_test_apps", func(t *testing.T) {
		text := callTool(t, "diagnose_namespace", map[string]any{"namespace": "test-apps"})
		fmt.Println(text)
	})

	t.Run("diagnose_cluster", func(t *testing.T) {
		text := callTool(t, "diagnose_cluster", nil)
		fmt.Println(text)
	})

	t.Run("get_node_metrics", func(t *testing.T) {
		text := callTool(t, "get_node_metrics", nil)
		fmt.Println(text)
	})

	t.Run("get_pod_metrics_test_apps", func(t *testing.T) {
		text := callTool(t, "get_pod_metrics", map[string]any{"namespace": "test-apps"})
		fmt.Println(text)
	})

	t.Run("top_resource_consumers_cpu", func(t *testing.T) {
		text := callTool(t, "top_resource_consumers", map[string]any{"resource": "cpu", "limit": 5})
		fmt.Println(text)
	})
}
