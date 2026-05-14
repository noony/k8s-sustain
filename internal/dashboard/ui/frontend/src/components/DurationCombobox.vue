<script setup lang="ts">
import { ref, computed, watch, nextTick, onBeforeUnmount, useId } from 'vue'
import { timeRangeOptions } from '../lib/api'

const props = withDefaults(
  defineProps<{
    modelValue: string
    placeholder?: string
    minWidth?: string
  }>(),
  {
    placeholder: 'e.g. 168h',
    minWidth: '100%',
  },
)

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const uid = useId()
const listboxId = `${uid}-list`
const optionId = (i: number) => `${uid}-opt-${i}`

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

const suggestions = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return timeRangeOptions
  return timeRangeOptions.filter(
    (o) => o.window.toLowerCase().includes(q) || o.label.toLowerCase().includes(q),
  )
})

const activeDescendant = computed(() =>
  activeIdx.value >= 0 && activeIdx.value < suggestions.value.length
    ? optionId(activeIdx.value)
    : undefined,
)

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
  emit('update:modelValue', query.value)
}

function onFocus() {
  open.value = true
}

function toggleFromCaret() {
  if (open.value) {
    open.value = false
    activeIdx.value = -1
  } else {
    open.value = true
    inputEl.value?.focus()
  }
}

async function onKey(e: KeyboardEvent) {
  const total = suggestions.value.length
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
    if (activeIdx.value >= 0 && activeIdx.value < total) {
      commit(suggestions.value[activeIdx.value].window)
    } else {
      commit(query.value)
    }
  } else if (e.key === 'Escape') {
    open.value = false
    activeIdx.value = -1
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
  if (item) item.scrollIntoView?.({ block: 'nearest' })
}

function onDocClick(e: MouseEvent) {
  if (!root.value) return
  if (!root.value.contains(e.target as Node)) {
    open.value = false
    activeIdx.value = -1
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
        :aria-controls="listboxId"
        :aria-activedescendant="activeDescendant"
        aria-autocomplete="list"
        @input="onInput"
        @focus="onFocus"
        @keydown="onKey"
      />
      <svg
        class="combobox-caret combobox-caret-clickable"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        aria-hidden="true"
        @mousedown.prevent="toggleFromCaret"
      >
        <path d="M6 9l6 6 6-6" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </div>
    <div v-if="open" :id="listboxId" ref="listEl" class="combobox-list" role="listbox">
      <div
        v-for="(opt, i) in suggestions"
        :key="opt.window"
        :id="optionId(i)"
        class="combobox-option duration-option"
        role="option"
        tabindex="-1"
        :data-active="activeIdx === i"
        :aria-selected="modelValue === opt.window"
        @mousedown.prevent="commit(opt.window)"
        @mouseenter="activeIdx = i"
      >
        <span class="duration-label">{{ opt.label }}</span>
        <span class="duration-value text-dim">{{ opt.window }}</span>
        <svg
          v-if="modelValue === opt.window"
          class="combobox-check"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
        >
          <path d="M5 12l5 5L20 7" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </div>
      <div v-if="suggestions.length === 0 && query" class="combobox-empty">
        No matches — value will be used as-is.
      </div>
    </div>
  </div>
</template>
