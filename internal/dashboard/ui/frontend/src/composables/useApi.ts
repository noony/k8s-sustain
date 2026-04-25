import { ref } from 'vue'

export function useApi<T>(fetcher: () => Promise<T>) {
  const data = ref<T | null>(null) as { value: T | null }
  const loading = ref(false)
  const error = ref('')
  let requestId = 0

  async function run() {
    const id = ++requestId
    loading.value = true
    try {
      const result = await fetcher()
      if (id !== requestId) return
      data.value = result
      error.value = ''
    } catch (e: any) {
      if (id !== requestId) return
      error.value = e?.message || String(e)
    } finally {
      if (id === requestId) loading.value = false
    }
  }

  return { data, loading, error, run }
}
