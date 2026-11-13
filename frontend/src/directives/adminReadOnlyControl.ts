import { effectScope, watchEffect, type Directive } from 'vue'
import { useAuthStore } from '@/stores/auth'

const cleanups = new WeakMap<HTMLElement, () => void>()
export const adminReadOnlyControl: Directive<HTMLElement> = {
  beforeMount(el) {
    const auth = useAuthStore()
    const scope = effectScope()
    scope.run(() => watchEffect(() => {
      el.inert = auth.isReadOnlyAdmin
      if (auth.isReadOnlyAdmin) el.setAttribute('aria-disabled', 'true')
      else el.removeAttribute('aria-disabled')
    }))
    cleanups.set(el, () => scope.stop())
  },
  unmounted(el) {
    cleanups.get(el)?.()
    cleanups.delete(el)
  },
}
