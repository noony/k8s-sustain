import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import DurationCombobox from './DurationCombobox.vue'

describe('DurationCombobox', () => {
  it('exposes ARIA combobox wiring when open', async () => {
    const w = mount(DurationCombobox, { props: { modelValue: '' } })
    const input = w.get('input')
    await input.trigger('focus')

    const listbox = w.get('[role="listbox"]')
    const listboxId = listbox.attributes('id')
    expect(listboxId).toBeTruthy()
    expect(input.attributes('aria-controls')).toBe(listboxId)
    expect(input.attributes('aria-expanded')).toBe('true')

    const options = w.findAll('[role="option"]')
    expect(options.length).toBeGreaterThan(0)
    // Options must be non-button so the button role doesn't override role=option.
    options.forEach((opt) => {
      expect(opt.element.tagName.toLowerCase()).not.toBe('button')
      expect(opt.attributes('id')).toBeTruthy()
    })
  })

  it('tracks aria-activedescendant on arrow navigation', async () => {
    const w = mount(DurationCombobox, { props: { modelValue: '' } })
    const input = w.get('input')
    await input.trigger('focus')
    await input.trigger('keydown', { key: 'ArrowDown' })

    const activeId = input.attributes('aria-activedescendant')
    expect(activeId).toBeTruthy()
    const active = w.get(`#${activeId}`)
    expect(active.attributes('role')).toBe('option')
  })

  it('shows the rephrased empty-state when no matches', async () => {
    const w = mount(DurationCombobox, { props: { modelValue: '' } })
    const input = w.get('input')
    await input.setValue('zzznomatch')
    await input.trigger('focus')
    expect(w.text()).toContain('No matches — value will be used as-is.')
    expect(w.text()).not.toContain('press Enter or click outside')
  })
})
