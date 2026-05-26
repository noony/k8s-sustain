<script setup lang="ts">
import { ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import ThemeToggle from './components/ThemeToggle.vue'

const route = useRoute()
const mobileNavOpen = ref(false)

function isActive(page: string): boolean {
  const path = route.path
  if (page === 'overview') return path === '/overview'
  if (page === 'policies') return path.startsWith('/policies')
  if (page === 'workloads') return path.startsWith('/workloads')
  if (page === 'simulator') return path.startsWith('/simulator')
  return false
}

watch(
  () => route.path,
  () => {
    mobileNavOpen.value = false
  },
)
</script>

<template>
  <div class="app">
    <header class="mobile-topbar">
      <button
        class="menu-btn"
        type="button"
        aria-label="Open navigation"
        :aria-expanded="mobileNavOpen"
        @click="mobileNavOpen = !mobileNavOpen"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M3 6h18M3 12h18M3 18h18" stroke-linecap="round" />
        </svg>
      </button>
      <h1><span class="logo">S</span> k8s-sustain</h1>
      <div class="spacer"></div>
      <ThemeToggle />
    </header>
    <div
      class="nav-backdrop"
      :class="{ open: mobileNavOpen }"
      @click="mobileNavOpen = false"
      aria-hidden="true"
    ></div>
    <nav class="sidebar" :class="{ open: mobileNavOpen }" aria-label="Primary">
      <div class="sidebar-header">
        <div class="row-between">
          <h1><span class="logo">S</span> k8s-sustain</h1>
          <ThemeToggle />
        </div>
        <div class="subtitle">Resource Right-Sizing Dashboard</div>
      </div>
      <div class="nav-section">Overview</div>
      <router-link to="/overview" class="nav-item" :class="{ active: isActive('overview') }">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path
            d="M3 12l2-2m0 0l7-7 7 7M5 10v10a1 1 0 001 1h3m10-11l2 2m-2-2v10a1 1 0 01-1 1h-3m-4 0a1 1 0 01-1-1v-4a1 1 0 011-1h2a1 1 0 011 1v4a1 1 0 01-1 1"
          />
        </svg>
        Overview
      </router-link>
      <router-link to="/policies" class="nav-item" :class="{ active: isActive('policies') }">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path
            d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2"
          />
        </svg>
        Policies
      </router-link>
      <router-link to="/workloads" class="nav-item" :class="{ active: isActive('workloads') }">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <rect x="2" y="3" width="20" height="14" rx="2" />
          <path d="M8 21h8M12 17v4" />
        </svg>
        Workloads
      </router-link>
      <div class="nav-section">Tools</div>
      <router-link to="/simulator" class="nav-item" :class="{ active: isActive('simulator') }">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path
            d="M12 6V4m0 2a2 2 0 100 4m0-4a2 2 0 110 4m-6 8a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4m6 6v10m6-2a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4"
          />
        </svg>
        Simulator
      </router-link>
    </nav>
    <main class="main">
      <router-view />
    </main>
  </div>
</template>
