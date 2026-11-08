import { apiClient } from '../client'

export interface RefillDeliveryConfig {
  ws_mode: string
  proxy_mode: string
  proxy_id: number | null
  name_template: string
  notes_template: string
  priority: number
  load_factor: number
  rate_multiplier: number
  status: string
  schedulable: boolean
  auto_pause_on_expired: boolean
  fingerprint_mode: string
  same_account_retry_count?: number | null
  privacy_enabled?: boolean
  openai_responses_flatten_namespaces?: boolean | null
  expiry_mode: string
  fixed_expires_at: number | null
  model_mapping: Record<string, string>
}
export interface RefillConfig {
  daily_schedule?: { enabled: boolean; time: string; quantity: number }
	rotation_enabled: boolean
	rotation: { plan: string; count: number }[]
  notifications: { refill_started: boolean; refill_needed?: boolean; low_balance: boolean; low_balance_threshold: number }
  enabled: boolean
  monitor_enabled: boolean
  immediate_failure_retry?: boolean | null
  timeout_retry_enabled?: boolean | null
  timeout_retry_notify?: boolean | null
  timeout_retry_minutes?: number | null
  mode: string
  group_id: number
  delivery_group_ids: number[]
  push_enabled: boolean
  max_in_flight: number
  interval_minutes: number
  interval_quantity: number
  plan: string
  min_ready: number
  max_ready: number
  lead_minutes: number
  min_remaining_5h: number
  min_remaining_7d: number
  plan_quota_thresholds?: Record<string, { min_remaining_5h: number; min_remaining_7d: number }>
  target_concurrency_percent: number
  concurrency_sustain_seconds: number
  forecast_enabled?: boolean | null
  import_expiry_grace_minutes: number
  check_seconds: number
  cooldown_seconds: number
  daily_max_count: number
  daily_max_points: number
  max_unit_price: number
  import_concurrency: number
  plan_import_concurrency?: Record<string, number>
  window_enabled: boolean
  window_start: string
  window_end: string
  delivery: RefillDeliveryConfig
}
export interface RefillJob {
  daily_batch_id?: string
  force?: boolean
  timeout_retry_of?: string
  rotation_generation?: number
  expiry_target?: { account_id: number; source_expires_at: number }
  manual_request_id?: string
  id: string
  supplier_id?: string
  status: string
  supplier_status?: string
  supplier_error?: string
  result_ready?: boolean
  budget_released?: boolean
  reason: string
  reason_detail?: string
  plan: string
  price: number
  created_at: string
  updated_at: string
  finished_at?: string
  expires_at?: number | null
  error?: string
  account_id?: number
  attempts: number
  next_attempt_at: string
}
export interface ManualRefillRequest {
	force?: boolean
	group_id?: number
  request_id: string
  plan: string
  revision: number
}
export interface RefillCapacityObservation {
  status: string; since: string | null; last_sample_at: string | null
  sustained_seconds: number; required_seconds: number; utilization_percent: number; samples: number; ready: boolean
  no_capacity?: boolean
}
export interface RefillView {
  daily_schedule_status?: { next_at: string; last_at: string; last_quantity: number; last_batch_id?: string; retry_at: string; message: string }
	rules: { group_id: number; enabled: boolean; monitor_enabled: boolean; rotation_enabled: boolean; next_plan: string }[]
	rotation_cursor: number
	next_plan: string
  webhook_configured: boolean
  notification_status: { at: string | null; message: string }
  lease_sync_at: string | null
  lease_sync_message: string
  capacity_observation: RefillCapacityObservation
  forecast_observation?: RefillCapacityObservation
  config: RefillConfig
  revision: number
  token_configured: boolean
  key: { label: string; prefix: string; scope: string; expires_at: string; revoked: boolean } | null
  wallet: { balance: number; held: number; available: number } | null
  catalog: { available: number; items: { service: string; plan: string; price: number; price_version: number; enabled: boolean; terms: string }[] } | null
  catalog_at: string | null
  identity_at: string | null
  jobs: RefillJob[]
  evaluation: {
    expiry_targets?: { account_id: number; source_expires_at: number }[]
    timed_out_pending?: number
    error?: string
    checked_at: string; ready: number; healthy: number; expiring: number; pending: number
    in_use: number; capacity: number; needed: number; reason: string
    pending_capacity?: number; forecast_accounts?: number; forecast_capacity?: number; forecast_ready?: number
    accounts: { id: number; name: string; state: string; plan?: string; min_remaining_5h?: number; min_remaining_7d?: number; expires_at: string | null; source_expires_at: string | null; expiry_source: string; source_synced_at?: string; remaining_5h: number | null; remaining_7d: number | null; concurrency: number; in_use: number; forecast_loss?: boolean }[]
  }
  daily: { count: number; points: number; reserved_points: number; spent_points: number }
  next_check_at: string
  next_interval_at: string
  message: string
  import_expiry_offset_minutes: number
}
export const accountRefillAPI = {
  async download(id: string, groupID: number) {
    const { data } = await apiClient.get<string>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/delivery`, {
      params: { group_id: groupID }, timeout: 60000,
      // Keep the supplier file intact, but let the shared interceptor process
      // JSON error envelopes (including the existing export step-up flow).
      transformResponse: [(raw: string) => {
        try {
          const parsed = JSON.parse(raw)
          if (parsed && typeof parsed === 'object' && 'code' in parsed && !Array.isArray(parsed.accounts)) return parsed
        } catch { /* Validate successful responses below, including proxy errors. */ }
        return raw
      }]
    })
    // A proxy may return HTTP 200 with HTML or an error envelope. Validate the
    // file before saving, but return the original text to preserve large IDs.
    try {
      if (typeof data !== 'string') throw new Error()
      const document = JSON.parse(data)
      const account = document?.accounts?.[0]
      if (!document || Array.isArray(document) ||
          (document.type != null && document.type !== '' && document.type !== 'sub2api-data') ||
          (document.version != null && document.version !== 0 && document.version !== 1) ||
          !Array.isArray(document.proxies) || !Array.isArray(document.accounts) || document.accounts.length !== 1 ||
          account?.platform !== 'openai' || account?.type !== 'oauth' ||
          typeof account.credentials?.access_token !== 'string' || !account.credentials.access_token.trim()) throw new Error()
    } catch {
      throw new Error('Invalid SUB2API delivery JSON')
    }
    return data
  },
  async get(groupID?: number) { return (await apiClient.get<RefillView>('/admin/accounts/refill', { params: groupID ? { group_id: groupID } : {} })).data },
  async save(config: RefillConfig, api_token: string, revision: number, wecom_webhook = '') {
    return (await apiClient.put<RefillView>('/admin/accounts/refill', { config, api_token, revision, wecom_webhook }, { params: { group_id: config.group_id }, timeout: 100000 })).data
  },
  async check(groupID?: number) { return (await apiClient.post<RefillView>('/admin/accounts/refill/check', {}, { params: groupID ? { group_id: groupID } : {}, timeout: 100000 })).data },
  async manual(request: ManualRefillRequest) { return (await apiClient.post<RefillView>('/admin/accounts/refill/manual', request, { params: request.group_id ? { group_id: request.group_id } : {}, timeout: 100000 })).data },
  async dismiss(id: string, groupID?: number) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/dismiss`, { confirm_not_accepted: true }, { params: groupID ? { group_id: groupID } : {} })).data },
  async retry(id: string, groupID?: number) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/retry`, {}, { params: groupID ? { group_id: groupID } : {} })).data }
}
