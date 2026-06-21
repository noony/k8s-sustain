// src/composables/useTimeRange.ts
import { ref, watch, type Ref } from 'vue'
import { decodeRange, encodeRange, type TimeRange } from '../lib/timerange'

export function useTimeRange(): { range: Ref<TimeRange> } {
  const range = ref<TimeRange>(decodeRange(new URLSearchParams(window.location.search)))

  watch(
    range,
    (next) => {
      const u = new URL(window.location.href)
      // clear previous range params, then set the new encoding
      for (const k of ['window', 'from_ts', 'to_ts']) u.searchParams.delete(k)
      for (const [k, v] of Object.entries(encodeRange(next, Date.now()))) {
        u.searchParams.set(k, v)
      }
      history.replaceState(null, '', u.toString())
    },
    { deep: true },
  )

  return { range }
}
