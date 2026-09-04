<!-- src/components/TimeRangePicker.vue -->
<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { PRESETS, type TimeRange } from '../lib/timerange'

const props = defineProps<{ modelValue: TimeRange }>()
const emit = defineEmits<{ 'update:modelValue': [TimeRange] }>()

const open = ref(false)
const showCalendar = ref(false)
const calError = ref('')
const rootEl = ref<HTMLElement | null>(null)
// Right-align the popover when the trigger sits in the right half of the
// viewport, so it grows leftward and stays on screen instead of overflowing.
const alignRight = ref(false)

const viewYear = ref(0)
const viewMonth = ref(0) // 0-11
const selStart = ref<Date | null>(null) // date-only (local midnight)
const selEnd = ref<Date | null>(null) // date-only (local midnight)
const fromTime = ref('00:00') // HH:MM
const toTime = ref('23:59') // HH:MM

const WEEKDAYS = ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su']
const MONTHS = [
  'January',
  'February',
  'March',
  'April',
  'May',
  'June',
  'July',
  'August',
  'September',
  'October',
  'November',
  'December',
]

function ymd(d: Date): string {
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

function dateOnly(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate())
}

function hhmm(d: Date): string {
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

function seedCalendar() {
  const r = props.modelValue
  let anchor = new Date()
  if (r.kind === 'absolute') {
    const f = new Date(r.fromTs * 1000)
    const t = new Date(r.toTs * 1000)
    selStart.value = dateOnly(f)
    selEnd.value = dateOnly(t)
    fromTime.value = hhmm(f)
    toTime.value = hhmm(t)
    anchor = f
  } else {
    selStart.value = null
    selEnd.value = null
    fromTime.value = '00:00'
    toTime.value = '23:59'
  }
  viewYear.value = anchor.getFullYear()
  viewMonth.value = anchor.getMonth()
  calError.value = ''
}

const todayYmd = computed(() => ymd(new Date()))
const todayTime = computed(() => dateOnly(new Date()).getTime())

function isFuture(d: Date): boolean {
  return d.getTime() > todayTime.value
}

const monthLabel = computed(() => `${MONTHS[viewMonth.value]} ${viewYear.value}`)

// Weeks (Monday-first) for the displayed month; leading/trailing blanks are null.
const weeks = computed<(Date | null)[][]>(() => {
  const first = new Date(viewYear.value, viewMonth.value, 1)
  const offset = (first.getDay() + 6) % 7 // Monday = 0
  const daysInMonth = new Date(viewYear.value, viewMonth.value + 1, 0).getDate()
  const cells: (Date | null)[] = []
  for (let i = 0; i < offset; i++) cells.push(null)
  for (let d = 1; d <= daysInMonth; d++) {
    cells.push(new Date(viewYear.value, viewMonth.value, d))
  }
  while (cells.length % 7 !== 0) cells.push(null)
  const out: (Date | null)[][] = []
  for (let i = 0; i < cells.length; i += 7) out.push(cells.slice(i, i + 7))
  return out
})

function dayClasses(d: Date) {
  const s = selStart.value?.getTime()
  const e = selEnd.value?.getTime()
  const t = d.getTime()
  return {
    'is-start': s === t,
    'is-end': e != null && e === t,
    'in-range': s != null && e != null && t > s && t < e,
    'is-today': ymd(d) === todayYmd.value,
    'is-future': isFuture(d),
  }
}

function clickDay(d: Date) {
  if (!selStart.value || selEnd.value) {
    selStart.value = d
    selEnd.value = null
  } else if (d.getTime() < selStart.value.getTime()) {
    selEnd.value = selStart.value
    selStart.value = d
  } else {
    selEnd.value = d
  }
  calError.value = ''
}

function prevMonth() {
  if (viewMonth.value === 0) {
    viewMonth.value = 11
    viewYear.value -= 1
  } else {
    viewMonth.value -= 1
  }
}

function nextMonth() {
  if (viewMonth.value === 11) {
    viewMonth.value = 0
    viewYear.value += 1
  } else {
    viewMonth.value += 1
  }
}

const fromLabel = computed(() => (selStart.value ? ymd(selStart.value) : '—'))
const toLabel = computed(() => {
  const end = selEnd.value ?? selStart.value
  return end ? ymd(end) : '—'
})

function combine(d: Date, hm: string): Date {
  const [h, m] = hm.split(':').map((n) => Number(n))
  return new Date(d.getFullYear(), d.getMonth(), d.getDate(), h || 0, m || 0, 0, 0)
}

function applyCalendar() {
  if (!selStart.value) {
    calError.value = 'Select a start date.'
    return
  }
  const end = selEnd.value ?? selStart.value
  const fromTs = Math.floor(combine(selStart.value, fromTime.value).getTime() / 1000)
  // Clamp a future end (e.g. picking today with the default 23:59) down to now —
  // there is no data ahead of now and the backend rejects to > now+1h.
  const nowTs = Math.floor(Date.now() / 1000)
  const toTs = Math.min(Math.floor(combine(end, toTime.value).getTime() / 1000), nowTs)
  if (fromTs >= toTs) {
    calError.value = 'From must be before To.'
    return
  }
  calError.value = ''
  emit('update:modelValue', { kind: 'absolute', fromTs, toTs })
  open.value = false
}

function onMousedown(e: MouseEvent) {
  if (open.value && !rootEl.value?.contains(e.target as Node)) {
    open.value = false
  }
}

function onKeydown(e: KeyboardEvent) {
  if (open.value && e.key === 'Escape') {
    open.value = false
  }
}

onMounted(() => {
  document.addEventListener('mousedown', onMousedown)
  document.addEventListener('keydown', onKeydown)
})

onUnmounted(() => {
  document.removeEventListener('mousedown', onMousedown)
  document.removeEventListener('keydown', onKeydown)
})

watch(showCalendar, (visible) => {
  if (visible) seedCalendar()
})

watch(open, async (isOpen) => {
  if (!isOpen) return
  await nextTick()
  const el = rootEl.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  alignRight.value = rect.left > window.innerWidth / 2
})

const tzLabel = computed(() => {
  const off = -new Date().getTimezoneOffset() // minutes east of UTC
  const sign = off >= 0 ? '+' : '-'
  const h = String(Math.floor(Math.abs(off) / 60)).padStart(2, '0')
  const m = String(Math.abs(off) % 60).padStart(2, '0')
  return `UTC${sign}${h}:${m}`
})

const label = computed(() => {
  const r = props.modelValue
  if (r.kind === 'relative') {
    return PRESETS.find((p) => p.window === r.window)?.label ?? r.window
  }
  const fmt = (ts: number) =>
    new Date(ts * 1000).toLocaleString([], {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    })
  return `${fmt(r.fromTs)} – ${fmt(r.toTs)}`
})

function pickPreset(window: string) {
  emit('update:modelValue', { kind: 'relative', window })
  open.value = false
}
</script>

<template>
  <div ref="rootEl" class="time-range-picker">
    <button data-test="picker-trigger" class="trp-trigger" @click="open = !open">
      <span class="trp-label">{{ label }}</span>
      <span class="trp-tz">{{ tzLabel }}</span>
      <span class="trp-caret">▾</span>
    </button>

    <div v-if="open" class="trp-popover" :class="{ 'align-right': alignRight }">
      <ul class="trp-preset-list">
        <li
          v-for="p in PRESETS"
          :key="p.window"
          :data-test="`preset-${p.window}`"
          class="trp-preset"
          :class="{ active: modelValue.kind === 'relative' && modelValue.window === p.window }"
          role="button"
          tabindex="0"
          @click="pickPreset(p.window)"
          @keydown.enter.prevent="pickPreset(p.window)"
          @keydown.space.prevent="pickPreset(p.window)"
        >
          <span class="trp-badge">{{ p.badge }}</span>
          <span class="trp-preset-label">{{ p.label }}</span>
        </li>
      </ul>

      <button
        data-test="show-calendar"
        class="trp-calendar-toggle"
        @click="showCalendar = !showCalendar"
      >
        Select from calendar…
      </button>

      <div v-if="showCalendar" class="trp-calendar">
        <div class="trp-cal-nav">
          <button
            data-test="cal-prev"
            type="button"
            class="trp-cal-navbtn"
            aria-label="Previous month"
            @click="prevMonth"
          >
            ‹
          </button>
          <span data-test="cal-month" class="trp-cal-month">{{ monthLabel }}</span>
          <button
            data-test="cal-next"
            type="button"
            class="trp-cal-navbtn"
            aria-label="Next month"
            @click="nextMonth"
          >
            ›
          </button>
        </div>

        <div class="trp-cal-grid">
          <span v-for="wd in WEEKDAYS" :key="wd" class="trp-cal-wd">{{ wd }}</span>
          <template v-for="(week, wi) in weeks" :key="wi">
            <template v-for="(d, di) in week" :key="di">
              <span v-if="!d" class="trp-cal-day empty"></span>
              <button
                v-else
                type="button"
                class="trp-cal-day"
                :class="dayClasses(d)"
                :disabled="isFuture(d)"
                :data-test="`cal-day-${ymd(d)}`"
                @click="clickDay(d)"
              >
                {{ d.getDate() }}
              </button>
            </template>
          </template>
        </div>

        <div class="trp-cal-times">
          <label class="trp-cal-timefield">
            <span class="trp-cal-timelabel">From</span>
            <span data-test="cal-from-label" class="trp-cal-datelabel">{{ fromLabel }}</span>
            <input data-test="cal-from-time" v-model="fromTime" type="time" class="trp-cal-time" />
          </label>
          <label class="trp-cal-timefield">
            <span class="trp-cal-timelabel">To</span>
            <span data-test="cal-to-label" class="trp-cal-datelabel">{{ toLabel }}</span>
            <input data-test="cal-to-time" v-model="toTime" type="time" class="trp-cal-time" />
          </label>
        </div>

        <p v-if="calError" data-test="cal-error" class="trp-cal-error">{{ calError }}</p>
        <button data-test="cal-apply" class="trp-apply" @click="applyCalendar">Apply</button>
      </div>
    </div>
  </div>
</template>
