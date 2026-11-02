import { beforeEach, describe, expect, it, vi } from 'vitest'
import { accountRefillAPI } from '../accountRefill'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))

describe('refill delivery download transport', () => {
  beforeEach(() => get.mockReset())

  it('preserves the raw JSON file and parses error envelopes for shared auth handling', async () => {
    const raw = '{ "type":"sub2api-data", "version":1, "proxies":[], "future_id":1234567890123456789, "accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}}] }'
    get.mockResolvedValue({ data: raw })
    expect(await accountRefillAPI.download('local/order', 7)).toBe(raw)
    expect(get).toHaveBeenCalledWith('/admin/accounts/refill/jobs/local%2Forder/delivery', expect.objectContaining({ params: { group_id: 7 }, timeout: 60000 }))
    const transform = get.mock.calls[0]![1].transformResponse[0]
    expect(transform(raw)).toBe(raw)
    const error = { code: 403, reason: 'STEP_UP_REQUIRED', message: 'verification required' }
    expect(transform(JSON.stringify(error))).toEqual(error)
  })

  it.each([
    '<!doctype html><html>proxy unavailable</html>', '', '{broken', 'null', '[]', '{}',
    '{"code":0,"data":null}',
    '{"proxies":[],"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":" "}}]}',
    '{"type":"cpa","proxies":[],"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}}]}',
    '{"version":999,"proxies":[],"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}}]}',
    '{"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}}]}'
  ])('rejects an invalid successful response instead of downloading it: %s', async raw => {
    get.mockResolvedValue({ data: raw })
    await expect(accountRefillAPI.download('order-1', 7)).rejects.toThrow('JSON')
  })
})
