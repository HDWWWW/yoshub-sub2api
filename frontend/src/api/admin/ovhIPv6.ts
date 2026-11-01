import { apiClient } from '../client'

export interface OvhIPv6Status {
  configured: boolean
  available: boolean
  enabled: boolean
  prefix: string
  issued: string
  bound: number
  pending_cleanup: number
  error?: string
}

export const ovhIPv6API = {
  async get(signal?: AbortSignal): Promise<OvhIPv6Status> {
    return (await apiClient.get<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { signal })).data
  },
  async setEnabled(enabled: boolean): Promise<OvhIPv6Status> {
    return (await apiClient.put<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { enabled })).data
  }
}
