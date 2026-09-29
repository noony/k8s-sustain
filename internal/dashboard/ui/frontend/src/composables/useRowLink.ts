import { useRouter, type RouteLocationRaw } from 'vue-router'

// A <tr> can't be an <a>, so clickable rows mimic the browser's own link
// handling: Cmd/Ctrl/Shift-click or middle-click opens a new tab.
export function useRowLink() {
  const router = useRouter()

  function openRow(to: RouteLocationRaw, e: MouseEvent) {
    if (e.button > 1) return
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button === 1) {
      window.open(router.resolve(to).href, '_blank')
      return
    }
    router.push(to)
  }

  return { openRow }
}
