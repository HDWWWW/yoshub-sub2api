import { describe, expect, it } from 'vitest'
import { formatNotificationTime } from '../beijingTime'
import { formatSupplierTime } from '../supplierTime'

describe('Beijing notification timestamps', () => {
  it.each(['2026-11-24T23:20:39Z', '2026-11-25T07:20:39+08:00', '2026-11-24T15:20:39-08:00'])('shows the same next-day event for %s', (instant) => {
    expect(formatNotificationTime(instant)).toBe('2026-11-25 07:20:39 (UTC+08:00)')
    expect(formatSupplierTime(instant)).toBe('2026-11-25 07:20:39')
  })

  it('uses 00 for midnight and a placeholder for missing or invalid times', () => {
    expect(formatNotificationTime('2026-12-31T16:00:00Z')).toBe('2027-01-01 00:00:00 (UTC+08:00)')
    expect(formatNotificationTime(null)).toBe('—')
    expect(formatNotificationTime('0001-01-01T00:00:00Z')).toBe('—')
    expect(formatNotificationTime('invalid')).toBe('—')
  })
})
