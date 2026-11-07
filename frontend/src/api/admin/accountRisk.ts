import { apiClient } from '../client'

export interface AccountRiskConfig {
  enabled: boolean
  notify_enabled: boolean
  capacity_notify_only: boolean
  probe_notify_only: boolean
  model_mismatch_action: 'off' | 'stop' | 'notify_only'
  first_below_percent: number
  second_at_least_percent: number
  sustain_seconds: number
  probe_interval_seconds: number
  probe_prompt: string
  expected_text: string
  model: string
  reasoning_effort: string
}
export interface AccountRiskLoad {
  account_id: number
  name: string
  priority: number
  used: number
  capacity: number
  percent: number
}
export interface AccountRiskRule {
  group_id: number
  revision: number
  webhook_status?: 'ready' | 'missing' | 'unavailable'
  config: AccountRiskConfig
  observation: {
    checked_at: string
    mode: string
    message: string
    eligible_accounts: number
    first?: AccountRiskLoad
    second?: AccountRiskLoad
    anomaly_since?: string
    next_probe_at?: string
    last_probe_at?: string
    last_probe_result?: string
    input_tokens: number
    output_tokens: number
  }
  events: { id?: string; at: string; account_id: number; reason: string; kind?: string; action?: 'stop' | 'notify_only'; occurrences?: number; notification?: 'pending' | 'sending' | 'sent' | 'failed' | 'unknown' | 'cancelled'; notification_at?: string }[]
}
export const accountRiskAPI = {
  async get(groupID: number): Promise<AccountRiskRule> {
    return (await apiClient.get<AccountRiskRule>('/admin/accounts/risk-detection', { params: { group_id: groupID } })).data
  },
  async save(groupID: number, revision: number, config: AccountRiskConfig): Promise<AccountRiskRule> {
    return (await apiClient.put<AccountRiskRule>('/admin/accounts/risk-detection', { revision, config }, { params: { group_id: groupID } })).data
  }
}
