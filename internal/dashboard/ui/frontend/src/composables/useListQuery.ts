import { computed, onUnmounted, ref, watch, type Ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

interface PagedData {
  items: unknown[]
  total: number
  pageSize: number
}

export interface ListQueryOptions<K extends string> {
  filterKeys: readonly K[]
  defaultSort: string
  pageSize?: number
}

// Keeps a paginated list's filters, sort and page in the URL so Back from a
// row restores the list as it was, and a filtered view can be shared.
export function useListQuery<K extends string>(opts: ListQueryOptions<K>) {
  const route = useRoute()
  const router = useRouter()
  const listPath = route.path
  const pageSize = opts.pageSize ?? 50

  function queryValue(key: string): string {
    const v = route.query[key]
    return typeof v === 'string' ? v : ''
  }

  function currentQuery(): Record<string, string> {
    const query: Record<string, string> = {}
    for (const [k, v] of Object.entries(route.query)) {
      if (typeof v === 'string' && v) query[k] = v
    }
    // useTimeRange writes window/from_ts/to_ts with history.replaceState, which
    // the router never sees; read the real URL so a list change keeps them.
    if (window.location.pathname === route.path) {
      for (const [k, v] of new URLSearchParams(window.location.search)) {
        if (v) query[k] = v
      }
    }
    return query
  }

  // replace, not push: typing in a search box must not flood history.
  function updateQuery(patch: Record<string, string | undefined>) {
    if (route.path !== listPath) return
    const query = currentQuery()
    for (const [k, v] of Object.entries(patch)) {
      if (v) query[k] = v
      else delete query[k]
    }
    router.replace({ path: route.path, query, hash: route.hash })
  }

  function filterRef(key: K): Ref<string> {
    return computed({
      get: () => queryValue(key),
      set: (v: string) => updateQuery({ [key]: v || undefined, page: undefined }),
    })
  }

  const page = computed({
    get: () => Math.max(1, parseInt(queryValue('page'), 10) || 1),
    set: (n: number) => updateQuery({ page: n > 1 ? String(n) : undefined }),
  })

  const sortParam = computed(() => queryValue('sort') || opts.defaultSort)
  const sortKey = computed(() => sortParam.value.replace(/^-/, ''))
  const sortDesc = computed(() => sortParam.value.startsWith('-'))

  function sort(key: string) {
    const next = sortKey.value === key && !sortDesc.value ? '-' + key : key
    updateQuery({ sort: next === opts.defaultSort ? undefined : next, page: undefined })
  }

  function sortArrow(key: string): string {
    if (sortKey.value !== key) return ''
    return sortDesc.value ? ' ▼' : ' ▲'
  }

  const searchInput = ref(queryValue('search'))
  let searchTimer: ReturnType<typeof setTimeout> | null = null

  function cancelSearch() {
    if (searchTimer) clearTimeout(searchTimer)
    searchTimer = null
  }

  watch(
    () => queryValue('search'),
    (v) => {
      cancelSearch()
      searchInput.value = v
    },
  )

  function onSearch(val: string) {
    searchInput.value = val
    cancelSearch()
    searchTimer = setTimeout(() => {
      searchTimer = null
      updateQuery({ search: val || undefined, page: undefined })
    }, 300)
  }

  onUnmounted(cancelSearch)

  const hasFilters = computed(
    () => searchInput.value !== '' || opts.filterKeys.some((k) => queryValue(k)),
  )

  function clearFilters() {
    cancelSearch()
    searchInput.value = ''
    updateQuery({
      ...Object.fromEntries(opts.filterKeys.map((k) => [k, undefined])),
      page: undefined,
    })
  }

  const apiQuery = computed(() => {
    const qs = new URLSearchParams({ page: String(page.value), pageSize: String(pageSize) })
    for (const key of opts.filterKeys) {
      const v = queryValue(key)
      if (v) qs.set(key, v)
    }
    qs.set('sort', sortParam.value)
    return qs.toString()
  })

  // Leaving the view empties the query before unmount; don't refetch then.
  function onQueryChange(cb: () => void) {
    watch(apiQuery, () => {
      if (route.path === listPath) cb()
    })
  }

  function totalPagesOf(data: PagedData | null | undefined): number {
    if (!data) return 1
    return Math.max(1, Math.ceil(data.total / (data.pageSize || pageSize)))
  }

  // A bookmarked or restored URL can point past the end once the list shrinks;
  // the API then returns no rows, which would read as "No matches".
  function clampPage(data: PagedData | null | undefined) {
    if (!data || data.items.length > 0 || data.total === 0) return
    const last = totalPagesOf(data)
    if (page.value > last) page.value = last
  }

  return {
    filterRef,
    page,
    sort,
    sortArrow,
    searchInput,
    onSearch,
    hasFilters,
    clearFilters,
    apiQuery,
    onQueryChange,
    totalPagesOf,
    clampPage,
  }
}
