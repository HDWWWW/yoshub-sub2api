// Locale files contain raw strings, so the runtime-only build is insufficient.
// JIT interprets message ASTs without eval/new Function and works with our CSP.
// Share these options with Vitest so interpolation tests cover the shipped build.
export const i18nAlias = 'vue-i18n/dist/vue-i18n.esm-bundler.js'
export const i18nDefines = {
  __INTLIFY_JIT_COMPILATION__: true,
  __INTLIFY_DROP_MESSAGE_COMPILER__: false,
}
