import { apiClient } from '../client'

export interface AccountRiskConfig {
  rate_limit_429_enabled: boolean
  rate_limit_429_window_seconds: number
  rate_limit_429_threshold: number
  enabled: boolean
  notify_enabled: boolean
  capacity_notify_only: boolean
  probe_notify_only: boolean
  probe_429_stop_enabled: boolean
  probe_429_max_retries: number
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
export interface AccountRiskCapacityCheck {
  target: AccountRiskLoad
  reference?: AccountRiskLoad
  anomaly_since?: string
  next_probe_at?: string
  last_probe_at?: string
}
export interface AccountRiskRule {
  group_id: number
  revision: number
  test_accounts?: { id: number; name: string; schedulable: boolean }[]
  webhook_status?: 'ready' | 'missing' | 'unavailable'
  config: AccountRiskConfig
  observation: {
    checked_at: string
    mode: string
    message: string
    eligible_accounts: number
    first?: AccountRiskLoad
    second?: AccountRiskLoad
    capacity_checks?: Record<string, AccountRiskCapacityCheck>
    anomaly_since?: string
    next_probe_at?: string
    last_probe_at?: string
    last_probe_result?: string
    input_tokens: number
    output_tokens: number
  }
  events: { id?: string; at: string; account_id: number; reason: string; kind?: string; action?: 'stop' | 'pause' | 'resume' | 'notify_only'; occurrences?: number; notification?: 'pending' | 'sending' | 'sent' | 'failed' | 'unknown' | 'cancelled'; notification_at?: string }[]
}
export interface AccountRiskManualResult {
  account_id: number
  completed: boolean
  matched: boolean | null
  text: string
  truncated: boolean
  message: string
  input_tokens: number
  output_tokens: number
}
export const accountRiskAPI = {
  async test(groupID: number, accountID: number, revision: number, config: AccountRiskConfig): Promise<AccountRiskManualResult> {
    return (await apiClient.post<AccountRiskManualResult>('/admin/accounts/risk-detection/test', { account_id: accountID, revision, config }, { params: { group_id: groupID }, timeout: 100000 })).data
  },
  async get(groupID: number): Promise<AccountRiskRule> {
    return (await apiClient.get<AccountRiskRule>('/admin/accounts/risk-detection', { params: { group_id: groupID } })).data
  },
  async save(groupID: number, revision: number, config: AccountRiskConfig): Promise<AccountRiskRule> {
    return (await apiClient.put<AccountRiskRule>('/admin/accounts/risk-detection', { revision, config }, { params: { group_id: groupID } })).data
  }
}
