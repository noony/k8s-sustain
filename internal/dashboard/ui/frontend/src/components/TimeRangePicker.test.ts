// src/components/TimeRangePicker.test.ts
import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TimeRangePicker from './TimeRangePicker.vue'

// 2026-06-10 12:00 → 2026-06-15 18:00 local, as epoch seconds. Seeding the
// picker with an absolute range opens the calendar on a deterministic month.
const FROM_TS = Math.floor(new Date(2026, 5, 10, 12, 0, 0).getTime() / 1000)
const TO_TS = Math.floor(new Date(2026, 5, 15, 18, 0, 0).getTime() / 1000)

function open(wrapper: any) {
  return wrapper.find('[data-test="picker-trigger"]').trigger('click')
}

async function openCalendar(wrapper: any) {
  await open(wrapper)
  await wrapper.find('[data-test="show-calendar"]').trigger('click')
}

describe('TimeRangePicker', () => {
  it('emits a relative range when a preset is clicked', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'relative', window: '1w' } },
    })
    await open(w)
    await w.find('[data-test="preset-1h"]').trigger('click')
    expect(w.emitted('update:modelValue')![0][0]).toEqual({ kind: 'relative', window: '1h' })
  })

  it('opens the calendar on the seeded month when the model is absolute', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    expect(w.find('[data-test="cal-month"]').text()).toBe('June 2026')
    // The seeded range endpoints are marked start/end.
    expect(w.find('[data-test="cal-day-2026-06-10"]').classes()).toContain('is-start')
    expect(w.find('[data-test="cal-day-2026-06-15"]').classes()).toContain('is-end')
  })

  it('emits an absolute range from picking two days plus times', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    await w.find('[data-test="cal-day-2026-06-19"]').trigger('click')
    await w.find('[data-test="cal-day-2026-06-20"]').trigger('click')
    await w.find('[data-test="cal-from-time"]').setValue('14:00')
    await w.find('[data-test="cal-to-time"]').setValue('09:00')
    await w.find('[data-test="cal-apply"]').trigger('click')
    const ev = w.emitted('update:modelValue')![0][0] as any
    expect(ev.kind).toBe('absolute')
    expect(ev.fromTs).toBe(Math.floor(new Date(2026, 5, 19, 14, 0, 0).getTime() / 1000))
    expect(ev.toTs).toBe(Math.floor(new Date(2026, 5, 20, 9, 0, 0).getTime() / 1000))
    expect(ev.fromTs).toBeLessThan(ev.toTs)
  })

  it('selecting days in reverse order swaps them into chronological order', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    await w.find('[data-test="cal-day-2026-06-20"]').trigger('click')
    await w.find('[data-test="cal-day-2026-06-12"]').trigger('click')
    expect(w.find('[data-test="cal-from-label"]').text()).toBe('2026-06-12')
    expect(w.find('[data-test="cal-to-label"]').text()).toBe('2026-06-20')
  })

  it('a single-day selection uses that day for both endpoints', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    await w.find('[data-test="cal-day-2026-06-19"]').trigger('click')
    await w.find('[data-test="cal-from-time"]').setValue('00:00')
    await w.find('[data-test="cal-to-time"]').setValue('23:59')
    await w.find('[data-test="cal-apply"]').trigger('click')
    const ev = w.emitted('update:modelValue')![0][0] as any
    expect(ev.kind).toBe('absolute')
    expect(ev.fromTs).toBe(Math.floor(new Date(2026, 5, 19, 0, 0, 0).getTime() / 1000))
    expect(ev.toTs).toBe(Math.floor(new Date(2026, 5, 19, 23, 59, 0).getTime() / 1000))
  })

  it('highlights today and disables future days', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 5, 21, 16, 9, 0))
    try {
      const w = mount(TimeRangePicker, {
        props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
      })
      await openCalendar(w)
      expect(w.find('[data-test="cal-day-2026-06-21"]').classes()).toContain('is-today')
      // Today is selectable; tomorrow and beyond are disabled.
      expect(w.find('[data-test="cal-day-2026-06-21"]').attributes('disabled')).toBeUndefined()
      expect(w.find('[data-test="cal-day-2026-06-22"]').attributes('disabled')).toBeDefined()
      // A future day click is a no-op (button disabled → no selection change).
      await w.find('[data-test="cal-day-2026-06-22"]').trigger('click')
      expect(w.find('[data-test="cal-from-label"]').text()).toBe('2026-06-10')
    } finally {
      vi.useRealTimers()
    }
  })

  it('navigates between months', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    await w.find('[data-test="cal-next"]').trigger('click')
    expect(w.find('[data-test="cal-month"]').text()).toBe('July 2026')
    await w.find('[data-test="cal-prev"]').trigger('click')
    await w.find('[data-test="cal-prev"]').trigger('click')
    expect(w.find('[data-test="cal-month"]').text()).toBe('May 2026')
  })

  it('clamps a future "To" (end of today) down to now', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 5, 21, 16, 9, 0))
    try {
      const w = mount(TimeRangePicker, {
        props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
      })
      await openCalendar(w)
      await w.find('[data-test="cal-day-2026-06-21"]').trigger('click')
      await w.find('[data-test="cal-from-time"]').setValue('08:00')
      await w.find('[data-test="cal-to-time"]').setValue('23:59')
      await w.find('[data-test="cal-apply"]').trigger('click')
      const ev = w.emitted('update:modelValue')![0][0] as any
      expect(ev.kind).toBe('absolute')
      expect(ev.toTs).toBe(Math.floor(Date.now() / 1000)) // clamped to now, not 23:59
      expect(ev.fromTs).toBe(Math.floor(new Date(2026, 5, 21, 8, 0, 0).getTime() / 1000))
    } finally {
      vi.useRealTimers()
    }
  })

  it('rejects from >= to on the same day (no emit)', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'absolute', fromTs: FROM_TS, toTs: TO_TS } },
    })
    await openCalendar(w)
    await w.find('[data-test="cal-day-2026-06-19"]').trigger('click')
    await w.find('[data-test="cal-from-time"]').setValue('10:00')
    await w.find('[data-test="cal-to-time"]').setValue('08:00')
    await w.find('[data-test="cal-apply"]').trigger('click')
    expect(w.emitted('update:modelValue')).toBeUndefined()
    expect(w.find('[data-test="cal-error"]').exists()).toBe(true)
  })

  it('right-aligns the popover when the trigger is in the right half of the viewport', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'relative', window: '1w' } },
      attachTo: document.body,
    })
    // jsdom defaults innerWidth to 1024; place the trigger past the midpoint.
    const root = w.find('.time-range-picker').element as HTMLElement
    vi.spyOn(root, 'getBoundingClientRect').mockReturnValue({ left: 900 } as DOMRect)
    await open(w)
    await w.vm.$nextTick()
    expect(w.find('.trp-popover').classes()).toContain('align-right')
    w.unmount()
  })

  it('left-aligns the popover when the trigger is in the left half', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'relative', window: '1w' } },
      attachTo: document.body,
    })
    const root = w.find('.time-range-picker').element as HTMLElement
    vi.spyOn(root, 'getBoundingClientRect').mockReturnValue({ left: 50 } as DOMRect)
    await open(w)
    await w.vm.$nextTick()
    expect(w.find('.trp-popover').classes()).not.toContain('align-right')
    w.unmount()
  })

  it('closes the popover on Escape', async () => {
    const w = mount(TimeRangePicker, {
      props: { modelValue: { kind: 'relative', window: '1w' } },
      attachTo: document.body,
    })
    await open(w)
    expect(w.find('[data-test="preset-1h"]').exists()).toBe(true)
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await w.vm.$nextTick()
    expect(w.find('[data-test="preset-1h"]').exists()).toBe(false)
    w.unmount()
  })
})
