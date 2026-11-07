import { describe, expect, it } from 'vitest'
import { mapErrorCategory } from '../errorCategory'

describe('upstream safety policy categories', () => {
  it('keeps biological and cyber refusals distinct', () => {
    expect(mapErrorCategory('request', 'bio_policy')).toBe('bio')
    expect(mapErrorCategory('request', 'cyber_policy')).toBe('cyber')
    expect(mapErrorCategory('request', 'BIO_POLICY')).toBe('bio')
  })

  it('preserves ordinary failure categories', () => {
    expect(mapErrorCategory('request', 'rate_limit_error')).toBe('rate_limit')
    expect(mapErrorCategory('upstream', 'upstream_error')).toBe('upstream')
    expect(mapErrorCategory('request', 'unknown_policy')).toBe('other')
  })
})
