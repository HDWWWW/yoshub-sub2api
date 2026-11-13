import { effectScope, watchEffect, type Directive, type EffectScope } from 'vue'
import { useAuthStore } from '@/stores/auth'

const scopes = new WeakMap<HTMLElement, EffectScope>()

// Presentation only. The server independently authorizes every request.
export const adminWrite: Directive<HTMLElement> = {
  beforeMount(el) {
    const auth = useAuthStore()
    const scope = effectScope()
    const originalDisplay = el.style.display
    scope.run(() => watchEffect(() => {
      if (auth.isReadOnlyAdmin) {
        el.style.setProperty('display', 'none', 'important')
        el.setAttribute('aria-hidden', 'true')
      } else {
        el.style.display = originalDisplay
        el.removeAttribute('aria-hidden')
      }
    }))
    scopes.set(el, scope)
  },
  updated(el) {
    if (useAuthStore().isReadOnlyAdmin) el.style.setProperty('display', 'none', 'important')
  },
  unmounted(el) {
    scopes.get(el)?.stop()
    scopes.delete(el)
  },
}
