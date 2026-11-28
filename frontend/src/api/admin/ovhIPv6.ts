import { apiClient } from '../client'
import type { Proxy } from '@/types'

export type OvhPool = 'primary' | 'backup'
export interface Retirement { id: number; pool: OvhPool; address: string; retired_at: string }

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

export interface OvhIPv6StorageStats {
  sampled_at: string
  issued: string | null
  prefix: string
  control_available: boolean
  control_error?: string
  files: {
    configured: boolean
    available: boolean
    error?: string
    items: Array<{
      name: 'counter.json' | 'counter.guard.json' | 'leases.json'
      bytes: number | null
      disk_bytes: number | null
      error?: string
    }>
    counter_bytes: number | null
    counter_disk_bytes: number | null
    total_bytes: number | null
    total_disk_bytes: number | null
  }
  database: {
    available: boolean
    error?: string
    rows: number | null
    reserved: number | null
    bound: number | null
    retiring: number | null
    table_bytes: number | null
    index_bytes: number | null
    total_bytes: number | null
  }
}

const allocationOperationKeys = new Map<string, string>()

function allocationStorageKey(name: string): string {
  return `sub2api:admin:ovh-ipv6-allocate:${encodeURIComponent(name)}`
}

function storedAllocationKey(name: string): string | null {
  try {
    return globalThis.sessionStorage?.getItem(allocationStorageKey(name)) ?? null
  } catch {
    return null
  }
}

function storeAllocationKey(name: string, key: string | null): void {
  try {
    if (key) globalThis.sessionStorage?.setItem(allocationStorageKey(name), key)
    else globalThis.sessionStorage?.removeItem(allocationStorageKey(name))
  } catch {
    // Preserve same-page retry protection when browser storage is unavailable.
  }
}

export const ovhIPv6API = {
  async allocate(name?: string, pool: OvhPool = 'primary'): Promise<Proxy> {
    const normalizedName = name?.trim() || ''
    const operation = pool === 'primary' ? normalizedName : JSON.stringify([pool, normalizedName])
    let key = allocationOperationKeys.get(operation) ?? storedAllocationKey(operation)
    if (!key) {
      const requestID = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
      key = `ovh-ipv6-allocate-${requestID}`
    }
    allocationOperationKeys.set(operation, key)
    storeAllocationKey(operation, key)
    const { data } = await apiClient.post<Proxy>('/admin/proxies/ovh-ipv6/allocate', { name: normalizedName || undefined, ...(pool === 'backup' ? { pool } : {}) }, {
      headers: { 'Idempotency-Key': key }
    })
    allocationOperationKeys.delete(operation)
    storeAllocationKey(operation, null)
    return data
  },
  async get(signal?: AbortSignal): Promise<OvhIPv6Status> {
    return (await apiClient.get<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { signal })).data
  },
  async getBackup(signal?: AbortSignal): Promise<OvhIPv6Status> {
    return (await apiClient.get<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { signal, params: { pool: 'backup' } })).data
  },
  async setBackup(enabled: boolean): Promise<OvhIPv6Status> {
    return (await apiClient.put<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { enabled }, { params: { pool: 'backup' } })).data
  },
  async retirements(before = 0): Promise<Retirement[]> {
    return (await apiClient.get<Retirement[]>('/admin/proxies/ovh-ipv6/retirements', { params: { before } })).data
  },
  async getStorage(signal?: AbortSignal): Promise<OvhIPv6StorageStats> {
    return (await apiClient.get<OvhIPv6StorageStats>('/admin/proxies/ovh-ipv6/storage', { signal })).data
  },
  async setEnabled(enabled: boolean): Promise<OvhIPv6Status> {
    return (await apiClient.put<OvhIPv6Status>('/admin/proxies/ovh-ipv6', { enabled })).data
  }
}
