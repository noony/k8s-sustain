<script setup lang="ts">
import { ref, computed, watch, nextTick, onBeforeUnmount } from 'vue'

const props = withDefaults(
  defineProps<{
    modelValue: string
    options: string[]
    placeholder?: string
    allLabel?: string
    minWidth?: string
    freeText?: boolean
    showAll?: boolean
  }>(),
  {
    placeholder: 'Filter…',
    allLabel: 'All',
    minWidth: '180px',
    freeText: false,
    showAll: true,
  },
)

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const query = ref(props.modelValue)
const open = ref(false)
const activeIdx = ref(-1)
const root = ref<HTMLDivElement | null>(null)
const listEl = ref<HTMLDivElement | null>(null)
const inputEl = ref<HTMLInputElement | null>(null)

watch(
  () => props.modelValue,
  (v) => {
    if (v !== query.value) query.value = v
  },
)

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  const sorted = [...props.options].sort()
  if (!q) return sorted
  return sorted.filter((o) => o.toLowerCase().includes(q))
})

function commit(value: string) {
  query.value = value
  emit('update:modelValue', value)
  open.value = false
  activeIdx.value = -1
}

function onInput(e: Event) {
  query.value = (e.target as HTMLInputElement).value
  open.value = true
  activeIdx.value = -1
  if (props.freeText) emit('update:modelValue', query.value)
}

function onFocus() {
  open.value = true
}

async function onKey(e: KeyboardEvent) {
  const showAllExtra = props.showAll ? 1 : 0
  const total = filtered.value.length + showAllExtra
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    open.value = true
    if (total === 0) return
    activeIdx.value = (activeIdx.value + 1) % total
    scrollActiveIntoView()
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    open.value = true
    if (total === 0) return
    activeIdx.value = (activeIdx.value - 1 + total) % total
    scrollActiveIntoView()
  } else if (e.key === 'Enter') {
    if (!open.value) return
    e.preventDefault()
    if (props.showAll && activeIdx.value === 0) {
      commit('')
    } else if (activeIdx.value >= showAllExtra && activeIdx.value < total) {
      commit(filtered.value[activeIdx.value - showAllExtra])
    } else if (filtered.value.length === 1) {
      commit(filtered.value[0])
    } else {
      commit(query.value)
    }
  } else if (e.key === 'Escape') {
    open.value = false
    activeIdx.value = -1
    if (!props.freeText) query.value = props.modelValue
    inputEl.value?.blur()
  } else if (e.key === 'Tab') {
    open.value = false
  }
  await nextTick()
}

async function scrollActiveIntoView() {
  await nextTick()
  const list = listEl.value
  if (!list) return
  const item = list.querySelector<HTMLElement>('[data-active="true"]')
  if (item) item.scrollIntoView({ block: 'nearest' })
}

function clear() {
  commit('')
  inputEl.value?.focus()
}

function onDocClick(e: MouseEvent) {
  if (!root.value) return
  if (!root.value.contains(e.target as Node)) {
    open.value = false
    activeIdx.value = -1
    // In picker mode, restore committed value if user typed without selecting.
    // In free-text mode, the input value is always the source of truth.
    if (!props.freeText && query.value !== props.modelValue) {
      query.value = props.modelValue
    }
  }
}

watch(open, (v) => {
  if (v) document.addEventListener('mousedown', onDocClick)
  else document.removeEventListener('mousedown', onDocClick)
})

onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocClick)
})
</script>

<template>
  <div ref="root" class="combobox" :style="{ minWidth }">
    <div class="combobox-input-wrap">
      <input
        ref="inputEl"
        type="text"
        role="combobox"
        autocomplete="off"
        spellcheck="false"
        :value="query"
        :placeholder="placeholder"
        :aria-expanded="open"
        aria-autocomplete="list"
        @input="onInput"
        @focus="onFocus"
        @keydown="onKey"
      />
      <button
        v-if="query"
        type="button"
        class="combobox-clear"
        aria-label="Clear"
        tabindex="-1"
        @mousedown.prevent="clear"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M18 6L6 18M6 6l12 12" stroke-linecap="round" />
        </svg>
      </button>
      <svg
        class="combobox-caret"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        aria-hidden="true"
      >
        <path d="M6 9l6 6 6-6" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </div>
    <div v-if="open" ref="listEl" class="combobox-list" role="listbox">
      <button
        v-if="showAll"
        type="button"
        class="combobox-option"
        role="option"
        :data-active="activeIdx === 0"
        :aria-selected="modelValue === ''"
        @mousedown.prevent="commit('')"
        @mouseenter="activeIdx = 0"
      >
        <span class="text-dim">{{ allLabel }}</span>
      </button>
      <button
        v-for="(opt, i) in filtered"
        :key="opt"
        type="button"
        class="combobox-option"
        role="option"
        :data-active="activeIdx === i + (showAll ? 1 : 0)"
        :aria-selected="modelValue === opt"
        @mousedown.prevent="commit(opt)"
        @mouseenter="activeIdx = i + (showAll ? 1 : 0)"
      >
        <span>{{ opt }}</span>
        <svg
          v-if="modelValue === opt"
          class="combobox-check"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
        >
          <path d="M5 12l5 5L20 7" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </button>
      <div v-if="filtered.length === 0 && query" class="combobox-empty">
        {{ freeText ? 'Press Enter to use this value' : 'No matches' }}
      </div>
    </div>
  </div>
</template>
