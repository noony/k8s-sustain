import { ref, watch } from 'vue'

const WINDOWS = ['1h', '4h', '12h', '24h', '72h', '168h', '720h']

export function usePrometheusTime(defaultWindow = '168h') {
  const url = new URL(window.location.href)
  const hashQuery = url.hash.includes('?') ? url.hash.split('?')[1] : ''
  const params = new URLSearchParams(hashQuery)
  const initial = params.get('window') ?? defaultWindow
  const safe = WINDOWS.includes(initial) ? initial : defaultWindow
  const win = ref(safe)

  watch(win, (next) => {
    const u = new URL(window.location.href)
    const [path, q = ''] = u.hash.replace(/^#/, '').split('?')
    const ps = new URLSearchParams(q)
    ps.set('window', next)
    u.hash = '#' + path + '?' + ps.toString()
    history.replaceState(null, '', u.toString())
  })

  return { window: win }
}
