<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'

type Theme = 'dark' | 'light'

const theme = ref<Theme>('dark')

function applyTheme(t: Theme) {
  document.documentElement.setAttribute('data-theme', t)
}

function load(): Theme {
  const saved = localStorage.getItem('theme') as Theme | null
  if (saved === 'light' || saved === 'dark') return saved
  if (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches)
    return 'light'
  return 'dark'
}

onMounted(() => {
  theme.value = load()
  applyTheme(theme.value)
})

watch(theme, (t) => {
  applyTheme(t)
  localStorage.setItem('theme', t)
  window.dispatchEvent(new CustomEvent('themechange', { detail: t }))
})

function toggle() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
}
</script>

<template>
  <button
    class="theme-toggle"
    type="button"
    :aria-label="theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'"
    :title="theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'"
    @click="toggle"
  >
    <svg
      v-if="theme === 'dark'"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <circle cx="12" cy="12" r="4" />
      <path
        d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"
      />
    </svg>
    <svg
      v-else
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
    </svg>
  </button>
</template>
