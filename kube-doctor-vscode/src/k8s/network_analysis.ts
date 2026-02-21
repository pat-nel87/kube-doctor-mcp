import * as k8s from '@kubernetes/client-node';
import { getCoreApi, getNetworkingApi } from './client';

/** Get a single service by name. */
export async function getService(namespace: string, name: string): Promise<k8s.V1Service> {
    const api = getCoreApi();
    return api.readNamespacedService({ namespace, name });
}

/** Get a single ingress by name. */
export async function getIngress(namespace: string, name: string): Promise<k8s.V1Ingress> {
    const api = getNetworkingApi();
    return api.readNamespacedIngress({ namespace, name });
}

/** Format service ports as "80/TCP, 443/TCP". */
export function formatServicePorts(svc: k8s.V1Service): string {
    return (svc.spec?.ports || []).map(p => {
        const base = `${p.port}/${p.protocol || 'TCP'}`;
        return p.nodePort ? `${p.port}:${p.nodePort}/${p.protocol || 'TCP'}` : base;
    }).join(', ');
}

/** Check if a pod matches a label selector. */
export function podMatchesSelector(pod: k8s.V1Pod, selector: Record<string, string>): boolean {
    const labels = pod.metadata?.labels || {};
    for (const [k, v] of Object.entries(selector)) {
        if (labels[k] !== v) { return false; }
    }
    return true;
}

/** Find ingresses matching a hostname+path. */
export async function findIngressForHostPath(
    namespace: string, host: string, path: string
): Promise<{ ingress: k8s.V1Ingress; rule: k8s.V1IngressRule; ingressPath: k8s.V1HTTPIngressPath } | null> {
    const api = getNetworkingApi();
    let ingresses: k8s.V1Ingress[];
    if (!namespace) {
        const resp = await api.listIngressForAllNamespaces();
        ingresses = resp.items;
    } else {
        const resp = await api.listNamespacedIngress({ namespace });
        ingresses = resp.items;
    }
    for (const ing of ingresses) {
        for (const rule of ing.spec?.rules || []) {
            if (rule.host !== host) { continue; }
            for (const p of rule.http?.paths || []) {
                const pathType = p.pathType || 'Prefix';
                if (pathType === 'Exact' && path === p.path) {
                    return { ingress: ing, rule, ingressPath: p };
                }
                if (p.path && path.startsWith(p.path)) {
                    return { ingress: ing, rule, ingressPath: p };
                }
            }
        }
    }
    return null;
}

/** AGIC annotation prefix. */
export const AGIC_PREFIX = 'appgw.ingress.kubernetes.io/';

/** Parse AGIC annotations from an ingress. */
export function parseAGICAnnotations(ingress: k8s.V1Ingress): { key: string; value: string }[] {
    const result: { key: string; value: string }[] = [];
    for (const [k, v] of Object.entries(ingress.metadata?.annotations || {})) {
        if (k.startsWith(AGIC_PREFIX)) {
            result.push({ key: k.replace(AGIC_PREFIX, ''), value: v });
        }
    }
    return result;
}

/** Escape label text for Mermaid diagrams. */
export function mermaidSafeId(text: string): string {
    return text.replace(/[^a-zA-Z0-9_]/g, '_');
}
