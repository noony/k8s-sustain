import { ref, onUnmounted } from 'vue'

export function useAutoRefresh(callback: () => void, intervalMs = 30000) {
  const enabled = ref(false)
  let timer: ReturnType<typeof setInterval> | null = null

  function toggle(val: boolean) {
    enabled.value = val
    if (val) {
      stop()
      timer = setInterval(callback, intervalMs)
    } else {
      stop()
    }
  }

  function stop() {
    if (timer) {
      clearInterval(timer)
      timer = null
    }
  }

  onUnmounted(stop)

  return { enabled, toggle, stop }
}
