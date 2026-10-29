export const DEFAULT_SAME_ACCOUNT_RETRY_COUNT = 1
export const MAX_SAME_ACCOUNT_RETRY_COUNT = 10

export function normalizeSameAccountRetryCount(value: unknown): number {
  if (value == null || value === '' || (typeof value !== 'number' && typeof value !== 'string')) {
    return DEFAULT_SAME_ACCOUNT_RETRY_COUNT
  }
  const count = Number(value)
  return Number.isFinite(count)
    ? Math.max(0, Math.min(MAX_SAME_ACCOUNT_RETRY_COUNT, Math.trunc(count)))
    : DEFAULT_SAME_ACCOUNT_RETRY_COUNT
}

export function readSameAccountRetryCount(account: {
  type: string
  credentials?: Record<string, unknown> | null
  extra?: Record<string, unknown> | null
}): number {
  const legacy = (account.type === 'apikey' || account.type === 'bedrock') && account.credentials?.pool_mode === true
    ? account.credentials.pool_mode_retry_count
    : undefined
  return normalizeSameAccountRetryCount(account.extra?.same_account_retry_count ?? legacy)
}
