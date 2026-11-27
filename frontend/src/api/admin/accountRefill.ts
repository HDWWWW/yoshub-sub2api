import { apiClient } from '../client'

export type RefillProvider = '1208' | 'muze' | '558669'
export interface RefillCatalog {
  available: number
  reserved?: number
  items: { service: string; plan: string; price: number; price_version: number; enabled: boolean; reason?: string; terms: string }[]
}
export interface Supplier558669View {
  token_configured: boolean
  catalog: RefillCatalog | null
  checked_at: string | null
  retry_at: string
  daily: { count: number; points: number; reserved_points: number; spent_points: number }
}

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
  health_duration_minutes?: number
  model_mapping: Record<string, string>
}
export interface RefillConfig {
  primary_provider?: RefillProvider
  supplier_558669?: { max_unit_price: number; daily_max_points: number } | null
  muze_fallback?: { provider?: '' | RefillProvider; enabled: boolean; failure_threshold: number; timeout_minutes: number } | null
  daily_schedule?: { enabled: boolean; time: string; quantity: number; plan?: string; day_mode?: '' | 'china_workday' }
  daily_schedules?: { id: string; enabled: boolean; time: string; quantity: number; plan: string; day_mode?: '' | 'china_workday' }[] | null
	rotation_enabled: boolean
	rotation: { provider?: '' | RefillProvider; plan: string; count: number }[]
  notifications: { channel?: '' | 'wecom' | 'qq'; qq_app_id?: string; qq_target_type?: 'group' | 'c2c'; qq_target_openid?: string; refill_started: boolean; refill_needed?: boolean; low_balance: boolean; low_balance_threshold: number; muze_remaining?: { enabled: boolean; threshold: number; products: string[] } | null }
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
  provider?: 'muze' | 'fendudu' | '558669'
  entitlement_status?: string
  delivery_version?: number
  applied_delivery_version?: number
  billing_status?: string
  supplier_email?: string
  muze_product_id?: string
  supplier_refund_phase?: string
  submission_started_at?: string
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
  supplier_expires_at?: number | null
  error?: string
  account_id?: number
  attempts: number
  next_attempt_at: string
}
export interface ManualRefillRequest {
  provider?: 'muze' | '558669'
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
export interface MuzeProduct {
  product: string
  label: string
  plan: string
  mode: string
  closed: boolean
  expired: boolean
  stock: number
  remaining: number
  available_now: number
  blocked_reason: string
}
export interface MuzeCatalog {
  token_configured: boolean
  connection_id: string
  products: MuzeProduct[] | null
  checked_at: string | null
}
export interface RefillView {
  supplier_558669?: Supplier558669View
  muze_fallback_status?: { consecutive_failures: number; last_at: string | null; message: string }
  workday_calendar?: { years: number[]; synced_at: string; checked_at: string; message: string }
  muze?: MuzeCatalog
  daily_schedule_status?: { next_at: string; last_at: string; last_quantity: number; last_batch_id?: string; retry_at: string; message: string }
  daily_schedule_statuses?: Record<string, { next_at: string; last_at: string; last_quantity: number; message: string }>
	rules: { group_id: number; enabled: boolean; monitor_enabled: boolean; rotation_enabled: boolean; next_provider?: string; next_plan: string }[]
	rotation_cursor: number
	next_provider?: string
	next_plan: string
  webhook_configured: boolean
  qq_secret_configured?: boolean
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
  catalog: RefillCatalog | null
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
  async catalog558669(groupID: number) {
    return (await apiClient.get<Supplier558669View>('/admin/accounts/refill/558669/catalog', { params: { group_id: groupID }, timeout: 40000 })).data
  },
  async configure558669(groupID: number, api_token: string, revision: number) {
    return (await apiClient.put<RefillView>('/admin/accounts/refill/558669', { api_token, revision }, { params: { group_id: groupID }, timeout: 70000 })).data
  },
  async getImportThreshold() {
    return (await apiClient.get<{ percent: number }>('/admin/accounts/refill/import-threshold')).data.percent
  },
  async setImportThreshold(percent: number) {
    return (await apiClient.put<{ percent: number }>('/admin/accounts/refill/import-threshold', { percent })).data.percent
  },
  async muzeProducts(groupID: number) {
    return (await apiClient.get<MuzeCatalog>('/admin/accounts/refill/muze/products', { params: { group_id: groupID }, timeout: 40000 })).data
  },
  async configureMuze(groupID: number, api_token: string, connection_id: string, revision: number) {
    return (await apiClient.put<RefillView>('/admin/accounts/refill/muze', { api_token, connection_id, revision }, { params: { group_id: groupID }, timeout: 70000 })).data
  },
  async reconcileMuze(id: string, groupID: number, supplier_id: string) {
    return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/muze-reconcile`, { supplier_id }, { params: { group_id: groupID }, timeout: 40000 })).data
  },
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
  async save(config: RefillConfig, api_token: string, revision: number, wecom_webhook = '', qq_client_secret = '') {
    return (await apiClient.put<RefillView>('/admin/accounts/refill', { config, api_token, revision, wecom_webhook, ...(qq_client_secret ? { qq_client_secret } : {}) }, { params: { group_id: config.group_id }, timeout: 100000 })).data
  },
  async testNotification(groupID: number, revision: number) {
    return (await apiClient.post<RefillView>('/admin/accounts/refill/notifications/test', { revision }, { params: { group_id: groupID }, timeout: 20000 })).data
  },
  async check(groupID?: number) { return (await apiClient.post<RefillView>('/admin/accounts/refill/check', {}, { params: groupID ? { group_id: groupID } : {}, timeout: 100000 })).data },
  async manual(request: ManualRefillRequest) { return (await apiClient.post<RefillView>('/admin/accounts/refill/manual', request, { params: request.group_id ? { group_id: request.group_id } : {}, timeout: 100000 })).data },
  async dismiss(id: string, groupID?: number) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/dismiss`, { confirm_not_accepted: true }, { params: groupID ? { group_id: groupID } : {} })).data },
  async retry(id: string, groupID?: number) { return (await apiClient.post<RefillView>(`/admin/accounts/refill/jobs/${encodeURIComponent(id)}/retry`, {}, { params: groupID ? { group_id: groupID } : {} })).data }
}
