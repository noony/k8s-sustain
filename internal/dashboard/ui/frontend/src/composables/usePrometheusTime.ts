import { ref, watch } from 'vue'

const WINDOWS = ['1h', '4h', '12h', '24h', '72h', '168h', '720h']

export function usePrometheusTime(defaultWindow = '168h') {
  const url = new URL(window.location.href)
  const initial = url.searchParams.get('window') ?? defaultWindow
  const safe = WINDOWS.includes(initial) ? initial : defaultWindow
  const win = ref(safe)

  watch(win, (next) => {
    const u = new URL(window.location.href)
    u.searchParams.set('window', next)
    history.replaceState(null, '', u.toString())
  })

  return { window: win }
}
