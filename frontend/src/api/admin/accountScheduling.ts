import { apiClient } from '../client'

export interface AccountSchedulingMetadata {
  account_id: number
  source_expires_at: string | null
  source_expiry_origin: string
  effective_expires_at: string | null
  grace_minutes: number
  manual_priority: number | null
}
export interface AccountSchedulingPreviewItem extends AccountSchedulingMetadata {
  name: string
  native_priority: number
  automatic_priority: number
  effective_priority: number
}
export const accountSchedulingAPI = {
  async metadata(account_ids: number[], signal?: AbortSignal) {
    return (await apiClient.post<AccountSchedulingMetadata[]>('/admin/accounts/scheduling-metadata', { account_ids }, { signal })).data
  },
  async preview(group_id: number, platform: string) {
    return (await apiClient.get<{ expiry_priority_enabled: boolean; items: AccountSchedulingPreviewItem[] }>('/admin/accounts/scheduling-preview', { params: { group_id, platform } })).data
  }
}
