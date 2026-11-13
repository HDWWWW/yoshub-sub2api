export const hiddenReadOnlyAdminPaths = new Set([
  '/admin/happy-first-token',
  '/keys',
  '/settings/mcp',
  '/oauth/mcp/authorize',
])

export function readOnlyAdminPageAllowed(path: string): boolean {
  return !hiddenReadOnlyAdminPaths.has(path) && !path.startsWith('/custom/')
}

// Kept in sync with the server's exact read-only POST allowlist.
const readOnlyPostPaths = new Set([
  '/admin/dashboard/users-usage',
  '/admin/dashboard/api-keys-usage',
  '/admin/accounts/usage/batch',
  '/admin/accounts/today-stats/batch',
  '/admin/accounts/lifetime-stats/batch',
  '/admin/user-attributes/batch',
])

export function isAdminMutation(method: string, path: string): boolean {
  const normalized = path.split('?')[0]!.replace(/^\/api\/v1/, '')
  if (!normalized.startsWith('/admin/')) return false
  const verb = method.toUpperCase()
  return verb !== 'GET' && !(verb === 'POST' && readOnlyPostPaths.has(normalized))
}
