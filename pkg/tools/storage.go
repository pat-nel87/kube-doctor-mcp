package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/k8s"
	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

type listPVCsInput struct {
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (use 'all' for all namespaces)"`
}

type listPVsInput struct{}

func registerStorageTools(server *mcp.Server, client *k8s.ClusterClient) {
	// list_pvcs
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_pvcs",
		Description: "List PersistentVolumeClaims with status, capacity, storage class, and access modes. Use namespace='all' for all namespaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listPVCsInput) (*mcp.CallToolResult, any, error) {
		ns := util.NamespaceOrAll(input.Namespace)
		opts := util.ListOptions("", "")

		pvcs, err := client.ListPVCs(ctx, ns, opts)
		if err != nil {
			return util.HandleK8sError("listing PVCs", err), nil, nil
		}

		headers := []string{"NAME", "NAMESPACE", "STATUS", "CAPACITY", "STORAGE CLASS", "ACCESS MODES", "AGE"}
		rows := make([][]string, 0, len(pvcs))
		for _, pvc := range pvcs {
			capacity := "<pending>"
			if pvc.Status.Capacity != nil {
				if storage, ok := pvc.Status.Capacity["storage"]; ok {
					capacity = storage.String()
				}
			}

			storageClass := "<default>"
			if pvc.Spec.StorageClassName != nil {
				storageClass = *pvc.Spec.StorageClassName
			}

			accessModes := make([]string, 0, len(pvc.Spec.AccessModes))
			for _, am := range pvc.Spec.AccessModes {
				accessModes = append(accessModes, string(am))
			}

			rows = append(rows, []string{
				pvc.Name,
				pvc.Namespace,
				string(pvc.Status.Phase),
				capacity,
				storageClass,
				strings.Join(accessModes, ","),
				util.FormatAge(pvc.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader(fmt.Sprintf("PersistentVolumeClaims (namespace: %s)", displayNS(input.Namespace))))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("PVCs", len(pvcs))))

		return util.SuccessResult(sb.String()), nil, nil
	})

	// list_pvs
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_pvs",
		Description: "List PersistentVolumes with status, capacity, reclaim policy, and storage class.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listPVsInput) (*mcp.CallToolResult, any, error) {
		pvs, err := client.ListPVs(ctx)
		if err != nil {
			return util.HandleK8sError("listing PVs", err), nil, nil
		}

		headers := []string{"NAME", "STATUS", "CAPACITY", "RECLAIM POLICY", "STORAGE CLASS", "CLAIM", "AGE"}
		rows := make([][]string, 0, len(pvs))
		for _, pv := range pvs {
			capacity := "<unknown>"
			if storage, ok := pv.Spec.Capacity["storage"]; ok {
				capacity = storage.String()
			}

			claim := "<unbound>"
			if pv.Spec.ClaimRef != nil {
				claim = fmt.Sprintf("%s/%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
			}

			rows = append(rows, []string{
				pv.Name,
				string(pv.Status.Phase),
				capacity,
				string(pv.Spec.PersistentVolumeReclaimPolicy),
				pv.Spec.StorageClassName,
				claim,
				util.FormatAge(pv.CreationTimestamp.Time),
			})
		}

		var sb strings.Builder
		sb.WriteString(util.FormatHeader("PersistentVolumes"))
		sb.WriteString("\n")
		sb.WriteString(util.FormatTable(headers, rows))
		sb.WriteString(fmt.Sprintf("\n%s\n", util.FormatCount("PVs", len(pvs))))

		return util.SuccessResult(sb.String()), nil, nil
	})
}
