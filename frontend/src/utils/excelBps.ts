export interface ExcelBPSAccountOptions {
  enabled: boolean
  models: string
  omitUnsupportedTools: boolean
  ignoreEncryptedContent: boolean
  cacheCreationAsInput: boolean
}
export function readExcelBPSOptions(extra?: Record<string, unknown> | null): ExcelBPSAccountOptions {
  return {
    enabled: extra?.openai_excel_bps === true,
    models: Array.isArray(extra?.openai_excel_bps_models) ? extra.openai_excel_bps_models.filter((x): x is string => typeof x === 'string').join('\n') : '',
    omitUnsupportedTools: extra?.openai_excel_bps_omit_unsupported_tools === true,
    ignoreEncryptedContent: extra?.openai_excel_bps_ignore_encrypted_content === true,
    cacheCreationAsInput: extra?.openai_excel_bps_cache_creation_as_input === true
  }
}
export function applyExcelBPSOptions(extra: Record<string, unknown>, options: ExcelBPSAccountOptions) {
  extra.openai_excel_bps = options.enabled
  const models = [...new Set(options.models.split(/[\n,]/).map(s => s.trim()).filter(Boolean))]
  if (models.length) extra.openai_excel_bps_models = models
  else delete extra.openai_excel_bps_models
  extra.openai_excel_bps_omit_unsupported_tools = options.omitUnsupportedTools
  extra.openai_excel_bps_ignore_encrypted_content = options.ignoreEncryptedContent
  extra.openai_excel_bps_cache_creation_as_input = options.cacheCreationAsInput
}
