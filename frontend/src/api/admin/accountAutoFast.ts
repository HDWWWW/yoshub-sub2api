import { apiClient } from '../client'

export interface AccountAutoFastConfig {
  enabled: boolean
  require_multiple_accounts: boolean
  stages: { minutes: number; used_below_percent: number }[]
  final_minutes: number
  usage_max_age_seconds: number
}
export interface AccountAutoFastRule {
  group_id: number
  revision: number
  config: AccountAutoFastConfig
  checked_at: string
  total: number
  eligible_accounts: number
  accounts: {
    account_id: number; name: string; deadline?: string; source: string
    used_percent?: number; stage_minutes?: number; mode: string; message: string
    fast: boolean; managed: boolean
  }[]
  events: { at: string; account_id: number; enabled: boolean; reason: string }[]
}

export const accountAutoFastAPI = {
  async get(groupID: number): Promise<AccountAutoFastRule> {
    return (await apiClient.get<AccountAutoFastRule>('/admin/accounts/auto-fast', { params: { group_id: groupID } })).data
  },
  async save(groupID: number, revision: number, config: AccountAutoFastConfig): Promise<AccountAutoFastRule> {
    return (await apiClient.put<AccountAutoFastRule>('/admin/accounts/auto-fast', { revision, config }, { params: { group_id: groupID } })).data
  }
}
