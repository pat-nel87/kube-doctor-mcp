package k8s

import (
	"context"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// GetPodLogs retrieves logs from a pod container.
func (c *ClusterClient) GetPodLogs(ctx context.Context, namespace, name, container string, tailLines int64, previous bool, sinceDuration string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	opts := &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
	}

	if tailLines > 0 {
		opts.TailLines = &tailLines
	} else {
		defaultTail := util.DefaultTailLines
		opts.TailLines = &defaultTail
	}

	if sinceDuration != "" {
		d, err := time.ParseDuration(sinceDuration)
		if err == nil {
			sinceSeconds := int64(d.Seconds())
			opts.SinceSeconds = &sinceSeconds
		}
	}

	stream, err := c.Clientset.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	// Read up to MaxLogBytes
	data, err := io.ReadAll(io.LimitReader(stream, int64(util.MaxLogBytes)+1))
	if err != nil {
		return "", err
	}

	result := string(data)
	if len(data) > util.MaxLogBytes {
		result = result[:util.MaxLogBytes] + "\n... [logs truncated at 50KB]"
	}

	return result, nil
}
