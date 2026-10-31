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
  expiry_mode: string
  fixed_expires_at: number | null
  model_mapping: Record<string, string>
}
export interface RefillConfig {
  enabled: boolean
  monitor_enabled: boolean
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
  target_concurrency_percent: number
  concurrency_sustain_seconds: number
  import_expiry_grace_minutes: number
  check_seconds: number
  cooldown_seconds: number
  daily_max_count: number
  daily_max_points: number
  max_unit_price: number
  import_concurrency: number
  window_enabled: boolean
  window_start: string
  window_end: string
  delivery: RefillDeliveryConfig
}
export interface RefillJob {
  id: string
  supplier_id?: string
  status: string
  supplier_status?: string
  reason: string
  plan: string
  price: number
  created_at: string
  updated_at: string
  finished_at?: string
  expires_at?: number
  error?: string
  account_id?: number
  attempts: number
  next_attempt_at: string
}
export interface RefillView {
  lease_sync_at: string | null
  lease_sync_message: string
  capacity_observation: {
    status: string; since: string | null; last_sample_at: string | null
    sustained_seconds: number; required_seconds: number; utilization_percent: number; samples: number; ready: boolean
  }
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
    checked_at: string; ready: number; healthy: number; expiring: number; pending: number
    in_use: number; capacity: number; needed: number; reason: string
    accounts: { id: number; name: string; state: string; expires_at: string | null; source_expires_at: string | null; expiry_source: string; source_synced_at?: string; remaining_5h: number | null; remaining_7d: number | null; concurrency: number; in_use: number }[]
  }
  daily: { count: number; points: number; reserved_points: number; spent_points: number }
  next_check_at: string
  next_interval_at: string
  message: string
  import_expiry_offset_minutes: number
}
export const accountRefillAPI = {
  async get() { return (await apiClient.get<RefillView>('/admin/accounts/refill')).data },
  async save(config: RefillConfig, api_token: string, revision: number) {
    return (await apiClient.put<RefillView>('/admin/accounts/refill', { config, api_token, revision }, { timeout: 100000 })).data
  },
  async check() { return (await apiClient.post<RefillView>('/admin/accounts/refill/check', {}, { timeout: 100000 })).data },
  async dismiss(id: string) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/dismiss`, { confirm_not_accepted: true })).data },
  async retry(id: string) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/retry`)).data }
}
