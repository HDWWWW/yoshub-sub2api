import type { AccountProxyMode } from '@/types'

/** Keep automatic selection separate from numeric proxy IDs used by OAuth. */
export function accountProxyPayload(mode: AccountProxyMode, id: number | null, update = false) {
  if (mode && mode !== 'specified' && mode !== 'direct') {
    return { proxy_mode: mode, proxy_id: null }
  }
  const proxyId = mode !== 'direct' && typeof id === 'number' && id > 0 ? id : (update ? 0 : null)
  return mode ? { proxy_mode: mode, proxy_id: proxyId } : { proxy_id: proxyId }
}
