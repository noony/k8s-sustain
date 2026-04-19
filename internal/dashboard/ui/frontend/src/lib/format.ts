export function parseCPUQuantity(str: string): number {
  if (!str) return NaN
  if (str.endsWith('m')) return parseInt(str) / 1000
  return parseFloat(str)
}

export function parseMemoryQuantity(str: string): number {
  if (!str) return NaN
  const units: Record<string, number> = {
    Ki: 1024,
    Mi: Math.pow(1024, 2),
    Gi: Math.pow(1024, 3),
    Ti: Math.pow(1024, 4),
  }
  for (const suffix in units) {
    if (str.endsWith(suffix)) return parseInt(str) * units[suffix]
  }
  return parseFloat(str)
}

export function timeAgo(dateStr?: string): string {
  if (!dateStr) return '-'
  const d = new Date(dateStr)
  const diff = Date.now() - d.getTime()
  const hours = Math.floor(diff / 3600000)
  if (hours < 1) return 'just now'
  if (hours < 24) return hours + 'h ago'
  const days = Math.floor(hours / 24)
  if (days < 30) return days + 'd ago'
  return d.toLocaleDateString()
}

export function deltaClass(pct: number | null | undefined): string {
  if (pct == null || isNaN(pct) || pct === 0) return ''
  return pct < -5 ? 'saving' : pct > 5 ? 'increase' : 'neutral'
}

export function downloadFile(filename: string, content: string, mimeType: string) {
  const blob = new Blob([content], { type: mimeType })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}
