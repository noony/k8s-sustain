import { onMounted, onUnmounted } from 'vue'

// Always-on refresh while the tab is visible. Fires `callback` every
// intervalMs; skips ticks (and fires an immediate catch-up) while hidden.
export function useAutoRefresh(callback: () => void, intervalMs = 60000): void {
  let timer: ReturnType<typeof setInterval> | null = null

  function tick() {
    if (document.visibilityState === 'visible') callback()
  }
  function start() {
    if (timer === null) timer = setInterval(tick, intervalMs)
  }
  function stop() {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }
  function onVisibility() {
    if (document.visibilityState === 'visible') {
      callback() // catch up immediately on regaining focus
      start()
    } else {
      stop()
    }
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
  })
  onUnmounted(() => {
    document.removeEventListener('visibilitychange', onVisibility)
    stop()
  })

  // Eagerly start the interval so the composable works outside a component
  // context (e.g., in unit tests where onMounted never fires).
  start()
}
