import type { MergePatch } from './api'

/** Every key path into T, from one key down to its leaves. */
export type KeyPath<T> = T extends readonly unknown[]
  ? never
  : T extends object
    ? {
        [K in keyof T & string]-?: readonly [K] | readonly [K, ...KeyPath<NonNullable<T[K]>>]
      }[keyof T & string]
    : never

/** What T holds at path P. */
export type ValueAt<T, P> = P extends readonly [infer K, ...infer Rest]
  ? K extends keyof NonNullable<T>
    ? ValueAt<NonNullable<T>[K], Rest>
    : never
  : T

// A generic KeyPath<T> is too deep to iterate, so the walks see plain keys.
const keysOf = (path: unknown) => path as readonly string[]

export function valueAt<T, const P extends KeyPath<T>>(
  obj: T | null | undefined,
  path: P,
): ValueAt<T, P> | undefined {
  let o: unknown = obj
  for (const k of keysOf(path)) {
    o = o != null && typeof o === 'object' ? (o as Record<string, unknown>)[k] : undefined
  }
  return o as ValueAt<T, P> | undefined
}

/** Whether the patch names path: a value or null, which removes the base's. */
export function patches<T, const P extends KeyPath<T>>(
  patch: MergePatch<T> | null | undefined,
  path: P,
): boolean {
  return valueAt(patch as T | undefined, path) !== undefined
}

export function setPatch<T, const P extends KeyPath<T>>(
  patch: MergePatch<T>,
  path: P,
  value: ValueAt<MergePatch<T>, P>,
): void {
  const keys = keysOf(path)
  let o = patch as Record<string, unknown>
  for (const k of keys.slice(0, -1)) {
    if (o[k] == null || typeof o[k] !== 'object') o[k] = {}
    o = o[k] as Record<string, unknown>
  }
  o[keys[keys.length - 1]] = value
}
