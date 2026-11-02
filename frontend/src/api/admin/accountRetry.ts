import { apiClient } from '../client'
export const accountRetryAPI = {
  async get() { return (await apiClient.get<{ count: number }>('/admin/accounts/retry-default')).data },
  async save(count: number) { return (await apiClient.put<{ count: number }>('/admin/accounts/retry-default', { count })).data }
}
