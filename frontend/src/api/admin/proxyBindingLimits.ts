import { apiClient } from '../client'

export interface ProxyBindingLimit { proxy_id: number; max_accounts: number; account_count: number }
export const proxyBindingLimitsAPI = {
  async get(proxy_ids: number[], signal?: AbortSignal) {
    return (await apiClient.post<ProxyBindingLimit[]>('/admin/proxies/binding-limits/query', { proxy_ids }, { signal })).data
  },
  async save(proxy_ids: number[], max_accounts: number) {
    return (await apiClient.put<ProxyBindingLimit[]>('/admin/proxies/binding-limits', { proxy_ids, max_accounts })).data
  }
}
