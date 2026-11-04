const CODEX_FINGERPRINT_MODES = [
  { value: 'off', labelKey: 'admin.accounts.openai.codexFingerprintOff' },
  { value: 'device', labelKey: 'admin.accounts.openai.codexFingerprintDevice' },
  { value: 'session', labelKey: 'admin.accounts.openai.codexFingerprintSession' },
  { value: 'full', labelKey: 'admin.accounts.openai.codexFingerprintFull' }
] as const

export type CodexFingerprintMode = typeof CODEX_FINGERPRINT_MODES[number]['value']

export function getCodexFingerprintModeOptions(t: (key: string) => string) {
  return CODEX_FINGERPRINT_MODES.map(({ value, labelKey }) => ({ value, label: t(labelKey) }))
}
