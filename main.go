package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/tools"
)

func main() {
	// All logging MUST go to stderr — stdout is reserved for MCP JSON-RPC
	log.SetOutput(os.Stderr)

	// Initialize the default Kubernetes client
	client, err := k8s.NewClusterClient("")
	if err != nil {
		log.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	// Create MCP server
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "kube-doctor",
			Version: "0.1.0",
		},
		nil,
	)

	// Register all tools
	tools.RegisterAll(server, client)

	log.Println("kube-doctor MCP server starting on stdio...")

	// Run on stdio transport
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
