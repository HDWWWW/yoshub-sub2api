import { effectScope, watchEffect, type Directive } from 'vue'
import { useAuthStore } from '@/stores/auth'

const cleanups = new WeakMap<HTMLElement, () => void>()

// Leave navigation and details buttons usable while configuration fields are
// disabled. This also covers form controls mounted later by tabs or dialogs.
export const adminReadOnlyForm: Directive<HTMLElement> = {
  mounted(el) {
    const auth = useAuthStore()
    const scope = effectScope()
    const original = new Map<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement, boolean>()
    const update = () => {
      if (!auth.isReadOnlyAdmin) return
      el.querySelectorAll<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>('input, select, textarea').forEach(control => {
        if (!original.has(control)) original.set(control, control.disabled)
        control.disabled = true
      })
    }
    const preventSubmit = (event: Event) => {
      if (auth.isReadOnlyAdmin) {
        event.preventDefault()
        event.stopImmediatePropagation()
      }
    }
    const observer = new MutationObserver(update)
    scope.run(() => watchEffect(() => {
      if (auth.isReadOnlyAdmin) update()
      else {
        original.forEach((disabled, control) => { control.disabled = disabled })
        original.clear()
      }
    }))
    observer.observe(el, { childList: true, subtree: true })
    el.addEventListener('submit', preventSubmit, true)
    cleanups.set(el, () => {
      scope.stop()
      observer.disconnect()
      el.removeEventListener('submit', preventSubmit, true)
    })
  },
  unmounted(el) {
    cleanups.get(el)?.()
    cleanups.delete(el)
  },
}
