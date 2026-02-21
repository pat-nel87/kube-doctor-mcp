package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type getEventsInput struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"Namespace (empty for all namespaces)"`
	InvolvedObject string `json:"involved_object,omitempty" jsonschema:"Filter events by object name"`
	EventType      string `json:"event_type,omitempty" jsonschema:"Filter by event type: Normal or Warning"`
	Limit          int    `json:"limit,omitempty" jsonschema:"Max events to return (default 50)"`
}

func registerEventTools(server *mcp.Server, client *k8s.ClusterClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_events",
		Description: "Get Kubernetes events, optionally filtered by namespace, resource name, or event type (Normal/Warning). Events are sorted by most recent first. Use event_type='Warning' to find problems.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getEventsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)

		// Build field selector
		var selectors []string
		if input.InvolvedObject != "" {
			selectors = append(selectors, "involvedObject.name="+input.InvolvedObject)
		}
		if input.EventType != "" {
			selectors = append(selectors, "type="+input.EventType)
		}

		opts := metav1.ListOptions{}
		if len(selectors) > 0 {
			opts.FieldSelector = strings.Join(selectors, ",")
		}

		events, err := client.ListEvents(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing events", err), nil, nil
		}

		limit := input.Limit
		if limit <= 0 {
			limit = util.MaxEvents
		}
		if len(events) > limit {
			events = events[:limit]
		}

		headers := []string{"TYPE", "REASON", "OBJECT", "MESSAGE", "COUNT", "LAST SEEN"}
		rows := make([][]string, 0, len(events))
		for _, e := range events {
			obj := fmt.Sprintf("%s/%s", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name)
			lastSeen := util.FormatAge(e.LastTimestamp.Time)
			if e.LastTimestamp.IsZero() {
				lastSeen = util.FormatAge(e.CreationTimestamp.Time)
			}

			msg := e.Message
			if len(msg) > 80 {
				msg = msg[:77] + "..."
			}

			rows = append(rows, []string{
				e.Type,
				e.Reason,
				obj,
				msg,
				fmt.Sprintf("%d", e.Count),
				lastSeen,
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("Events (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("events", len(events))))

		return util.SuccessResult(sb.String()), nil, nil
	})
}
