import { reactive } from 'vue'

interface SortState {
  key: string
  dir: number
}

const states: Record<string, SortState> = reactive({})

export function useSorting(tableId: string) {
  function sort(key: string) {
    const state = states[tableId] || { key: '', dir: 1 }
    state.dir = state.key === key ? state.dir * -1 : 1
    state.key = key
    states[tableId] = state
  }

  function sortArrow(key: string): string {
    const state = states[tableId]
    if (!state || state.key !== key) return ''
    return state.dir === 1 ? ' \u25B2' : ' \u25BC'
  }

  function applySorting<T extends Record<string, any>>(data: T[]): T[] {
    const state = states[tableId]
    if (!state || !state.key) return data
    return [...data].sort((a, b) => {
      let va = a[state.key]
      let vb = b[state.key]
      if (va == null) va = ''
      if (vb == null) vb = ''
      if (typeof va === 'string' && typeof vb === 'string') return state.dir * va.localeCompare(vb)
      return state.dir * ((parseFloat(va) || 0) - (parseFloat(vb) || 0))
    })
  }

  return { sort, sortArrow, applySorting }
}
