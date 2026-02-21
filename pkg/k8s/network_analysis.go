package k8s

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/pat-nel87/kube-doctor-mcp/pkg/util"
)

// GetService returns a single service by name.
func (c *ClusterClient) GetService(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetIngress returns a single ingress by name.
func (c *ClusterClient) GetIngress(ctx context.Context, namespace, name string) (*networkingv1.Ingress, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	return c.Clientset.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
}

// EndpointHealth summarizes the health of endpoints for a service.
type EndpointHealth struct {
	ServiceName    string
	ServiceNS      string
	TotalEndpoints int
	ReadyCount     int
	NotReadyCount  int
	ReadyAddresses []EndpointAddress
	NotReadyPods   []EndpointAddress
}

// EndpointAddress is an individual endpoint address with pod info.
type EndpointAddress struct {
	IP       string
	PodName  string
	NodeName string
}

// GetServiceEndpointHealth returns endpoint health for a service.
func (c *ClusterClient) GetServiceEndpointHealth(ctx context.Context, namespace, serviceName string) (*EndpointHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	ep, err := c.Clientset.CoreV1().Endpoints(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	health := &EndpointHealth{
		ServiceName: serviceName,
		ServiceNS:   namespace,
	}

	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			ea := EndpointAddress{IP: addr.IP}
			if addr.TargetRef != nil {
				ea.PodName = addr.TargetRef.Name
			}
			if addr.NodeName != nil {
				ea.NodeName = *addr.NodeName
			}
			health.ReadyAddresses = append(health.ReadyAddresses, ea)
			health.ReadyCount++
		}
		for _, addr := range subset.NotReadyAddresses {
			ea := EndpointAddress{IP: addr.IP}
			if addr.TargetRef != nil {
				ea.PodName = addr.TargetRef.Name
			}
			if addr.NodeName != nil {
				ea.NodeName = *addr.NodeName
			}
			health.NotReadyPods = append(health.NotReadyPods, ea)
			health.NotReadyCount++
		}
	}
	health.TotalEndpoints = health.ReadyCount + health.NotReadyCount
	return health, nil
}

// ServiceDependency represents an inferred dependency from one service to another.
type ServiceDependency struct {
	FromService string
	FromNS      string
	ToService   string
	ToNS        string
	Confidence  string // "high", "medium", "low"
	Source      string // "env", "configmap", "port"
}

// InferServiceDependencies infers inter-service dependencies from pod env vars.
func (c *ClusterClient) InferServiceDependencies(ctx context.Context, namespace string) ([]ServiceDependency, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	// Get all services to build name lookup
	services, err := c.Clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	svcNames := make(map[string]bool)
	for _, svc := range services.Items {
		svcNames[svc.Name] = true
	}

	// Get all pods
	pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	// For each service, find its pods and check their env for references to other services
	svcToPods := make(map[string][]corev1.Pod)
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, svc := range services.Items {
			if svc.Spec.Selector != nil && len(svc.Spec.Selector) > 0 {
				sel := labels.SelectorFromSet(svc.Spec.Selector)
				if sel.Matches(labels.Set(pod.Labels)) {
					svcToPods[svc.Name] = append(svcToPods[svc.Name], *pod)
					break
				}
			}
		}
	}

	var deps []ServiceDependency
	seen := make(map[string]bool)

	for svcName, svcPods := range svcToPods {
		for _, pod := range svcPods {
			for _, container := range pod.Spec.Containers {
				for _, env := range container.Env {
					// Check for Kubernetes-injected service env vars (*_SERVICE_HOST)
					if strings.HasSuffix(env.Name, "_SERVICE_HOST") {
						targetName := strings.TrimSuffix(env.Name, "_SERVICE_HOST")
						targetName = strings.ToLower(strings.ReplaceAll(targetName, "_", "-"))
						if svcNames[targetName] && targetName != svcName {
							key := svcName + "->" + targetName
							if !seen[key] {
								deps = append(deps, ServiceDependency{
									FromService: svcName, FromNS: namespace,
									ToService: targetName, ToNS: namespace,
									Confidence: "high", Source: "env",
								})
								seen[key] = true
							}
						}
					}
					// Check env value for service DNS patterns
					if env.Value != "" {
						for candidateSvc := range svcNames {
							if candidateSvc != svcName && strings.Contains(env.Value, candidateSvc+"."+namespace) {
								key := svcName + "->" + candidateSvc
								if !seen[key] {
									deps = append(deps, ServiceDependency{
										FromService: svcName, FromNS: namespace,
										ToService: candidateSvc, ToNS: namespace,
										Confidence: "medium", Source: "env",
									})
									seen[key] = true
								}
							}
						}
					}
				}
			}
		}
	}

	return deps, nil
}

// AGICAnnotation represents a parsed AGIC annotation.
type AGICAnnotation struct {
	Key   string
	Value string
}

// AGICAnnotationPrefix is the prefix for Azure Application Gateway Ingress Controller annotations.
const AGICAnnotationPrefix = "appgw.ingress.kubernetes.io/"

// ParseAGICAnnotations extracts AGIC annotations from an Ingress.
func ParseAGICAnnotations(ingress *networkingv1.Ingress) []AGICAnnotation {
	var annotations []AGICAnnotation
	for k, v := range ingress.Annotations {
		if strings.HasPrefix(k, AGICAnnotationPrefix) {
			annotations = append(annotations, AGICAnnotation{
				Key:   strings.TrimPrefix(k, AGICAnnotationPrefix),
				Value: v,
			})
		}
	}
	return annotations
}

// GetPodsForService returns pods matching a service's selector.
func (c *ClusterClient) GetPodsForService(ctx context.Context, svc *corev1.Service) ([]corev1.Pod, error) {
	if svc.Spec.Selector == nil || len(svc.Spec.Selector) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	sel := labels.SelectorFromSet(svc.Spec.Selector)
	opts := metav1.ListOptions{LabelSelector: sel.String()}
	list, err := c.Clientset.CoreV1().Pods(svc.Namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// FindIngressForHostPath searches ingresses for a matching host+path.
func (c *ClusterClient) FindIngressForHostPath(ctx context.Context, namespace, host, path string) (*networkingv1.Ingress, *networkingv1.IngressRule, *networkingv1.HTTPIngressPath, error) {
	ctx, cancel := context.WithTimeout(ctx, util.DefaultTimeout)
	defer cancel()

	ingresses, err := c.Clientset.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, err
	}

	// Search all namespaces if needed
	if namespace == "" {
		ingresses, err = c.Clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, nil, err
		}
	}

	for i := range ingresses.Items {
		ing := &ingresses.Items[i]
		for j := range ing.Spec.Rules {
			rule := &ing.Spec.Rules[j]
			if rule.Host != host {
				continue
			}
			if rule.HTTP == nil {
				continue
			}
			for k := range rule.HTTP.Paths {
				p := &rule.HTTP.Paths[k]
				pathType := networkingv1.PathTypePrefix
				if p.PathType != nil {
					pathType = *p.PathType
				}
				if matchPath(p.Path, path, pathType) {
					return ing, rule, p, nil
				}
			}
		}
	}
	return nil, nil, nil, fmt.Errorf("no ingress found for %s%s", host, path)
}

func matchPath(pattern, path string, pathType networkingv1.PathType) bool {
	switch pathType {
	case networkingv1.PathTypeExact:
		return path == pattern
	case networkingv1.PathTypePrefix:
		return strings.HasPrefix(path, pattern)
	default: // ImplementationSpecific
		return strings.HasPrefix(path, pattern)
	}
}
