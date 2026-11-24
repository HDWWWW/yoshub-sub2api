// A fixed display zone keeps the date and time independent of the browser timezone.
const beijingDateTime = new Intl.DateTimeFormat('sv-SE', {
  timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23'
})

export function formatBeijingTime(value: string | Date | null | undefined): string {
  if (!value) return '—'
  const date = typeof value === 'string' ? new Date(value) : value
  return Number.isFinite(date.getTime()) ? beijingDateTime.format(date) : '—'
}

export function formatNotificationTime(value: string | Date | null | undefined): string {
  if (typeof value === 'string' && value.startsWith('0001-01-01T00:00:00Z')) return '—'
  const time = formatBeijingTime(value)
  return time === '—' ? time : `${time} (UTC+08:00)`
}
