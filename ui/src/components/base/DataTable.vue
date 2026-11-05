<script setup lang="ts" generic="R">
import EntityIcon, { type EntityKind } from '@/components/base/EntityIcon.vue'
// Dense data table (36px rows). Cells render via `#cell-<key>` scoped slots; default is the
// row's property value. Optional `rowLink` makes the whole row open a route on click while
// keeping real links/buttons inside cells keyboard-accessible.
//
// States, in order: `error` replaces the rows with a failure row and "Try again" (emits
// `retry`; load-more is hidden), first load shows a loading row, and an empty result shows
// either `filteredText` with "Clear filters" (when `filtered`: the list was narrowed by filters,
// search; emits `clear-filters`) or the normal `emptyText` / `#empty` slot.
// `hasMore` renders the "Load more" footer (emits `load-more`).
import { IconFilterOff } from '@tabler/icons-vue'
import { computed, ref, type Component } from 'vue'
import { useRouter, type RouteLocationRaw } from 'vue-router'
import BaseButton from './BaseButton.vue'
import ErrorState from './ErrorState.vue'
import LoadingState from './LoadingState.vue'
import { useOverflowX } from './overflow'
import type { Column } from './types'

const props = defineProps<{
  columns: Column[]
  rows: R[]
  rowKey: (row: R) => string
  label: string
  rowLink?: (row: R) => RouteLocationRaw
  emptyText?: string
  /** Large muted icon above the empty text. */
  emptyIcon?: Component
  /** Coloured entity tile in the empty state instead of `emptyIcon`. */
  emptyKind?: EntityKind
  loading?: boolean
  /** Below this width the table scrolls horizontally instead of squeezing columns. */
  minWidth?: string
  /** A failed load (ApiError / Error / text). Replaces the rows until `retry` succeeds. */
  error?: unknown
  errorTitle?: string
  /** The list is narrowed by filters or search (changes the empty state). */
  filtered?: boolean
  filteredText?: string
  hasMore?: boolean
  loadingMore?: boolean
}>()
defineEmits<{ retry: []; 'clear-filters': []; 'load-more': [] }>()
const router = useRouter()

function onRowClick(row: R, e: MouseEvent) {
  if (!props.rowLink) return
  if ((e.target as HTMLElement).closest('a, button, input, select, textarea')) return
  if (e.metaKey || e.ctrlKey || e.shiftKey || window.getSelection()?.toString()) return
  void router.push(props.rowLink(row))
}
function cellValue(row: R, key: string): unknown {
  return (row as Record<string, unknown>)[key]
}
// A table wider than its box scrolls sideways; keyboard users can then focus the box to scroll it.
const scrollBox = ref<HTMLElement | null>(null)
const overflowing = useOverflowX(scrollBox)

/**
 * The narrowest width at which every flexible column still gets 140 px, so a narrow screen
 * scrolls the table instead of squeezing a column to nothing (its text then overlapped the
 * next column). A percentage is a share of the whole table, so columns without a width only
 * get what the fixed and percentage columns leave. An explicit `minWidth` is a floor.
 */
const MIN_FLEX = 140
const tableMinWidth = computed(() => {
  let px = 0
  let share = 0
  let auto = 0
  let forShares = 0
  for (const c of props.columns) {
    const fixed = c.width?.match(/^(\d+(?:\.\d+)?)px$/)
    const pct = c.width?.match(/^(\d+(?:\.\d+)?)%$/)
    if (fixed) px += Number(fixed[1])
    else if (pct && Number(pct[1]) > 0) {
      share += Number(pct[1]) / 100
      forShares = Math.max(forShares, MIN_FLEX / (Number(pct[1]) / 100))
    } else auto++
  }
  const rest = 1 - share
  const min = Math.ceil(rest > 0.05 ? Math.max((px + MIN_FLEX * auto) / rest, forShares) : px + MIN_FLEX * (auto + 1))
  return props.minWidth ? `max(${props.minWidth}, ${min}px)` : `${min}px`
})
</script>

<template>
  <div class="flex flex-col gap-2.5">
    <div
      ref="scrollBox"
      class="lg-panel relative overflow-x-auto"
      :tabindex="overflowing ? 0 : undefined"
      :role="overflowing ? 'region' : undefined"
      :aria-label="overflowing ? `${label} (scrolls sideways)` : undefined"
    >
      <table class="lg-table table-fixed" :style="{ minWidth: tableMinWidth }" :aria-label="label" :aria-busy="loading || undefined">
        <colgroup>
          <col v-for="c in columns" :key="c.key" :style="c.width ? { width: c.width } : undefined" />
        </colgroup>
        <thead>
          <tr>
            <th v-for="c in columns" :key="c.key" scope="col" class="first:pl-5 last:pr-5">{{ c.label }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="error">
            <td :colspan="columns.length" class="!h-auto px-5">
              <ErrorState :error="error" :title="errorTitle" :retrying="loading" @retry="$emit('retry')" />
            </td>
          </tr>
          <template v-else>
            <tr
              v-for="row in rows"
              :key="rowKey(row)"
              :class="rowLink ? 'lg-hov cursor-pointer' : ''"
              @click="onRowClick(row, $event)"
            >
              <td v-for="c in columns" :key="c.key" class="first:pl-5 last:pr-5" :class="c.class">
                <slot :name="`cell-${c.key}`" :row="row">{{ cellValue(row, c.key) ?? '—' }}</slot>
              </td>
            </tr>
            <tr v-if="!rows.length && loading">
              <td :colspan="columns.length" class="pl-5"><LoadingState /></td>
            </tr>
            <tr v-else-if="!rows.length && filtered">
              <td :colspan="columns.length" class="!h-auto px-5">
                <div class="flex items-center gap-3 py-3">
                  <span class="flex-1 text-muted">{{ filteredText ?? 'No matching results' }}</span>
                  <BaseButton size="sm" @click="$emit('clear-filters')">
                    <template #icon><IconFilterOff :size="14" stroke="1.75" aria-hidden="true" /></template>
                    Clear filters
                  </BaseButton>
                </div>
              </td>
            </tr>
            <tr v-else-if="!rows.length">
              <td :colspan="columns.length" :class="$slots.empty ? '!h-auto !p-0' : emptyIcon || emptyKind ? '!h-auto px-5 text-muted' : 'pl-5 text-muted'">
                <slot name="empty">
                  <div v-if="emptyIcon || emptyKind" class="flex flex-col items-center gap-2 py-8 text-center">
                    <EntityIcon v-if="emptyKind" :kind="emptyKind" size="lg" />
                    <component :is="emptyIcon" v-else :size="32" stroke="1.5" aria-hidden="true" />
                    <span>{{ emptyText ?? 'Nothing to show.' }}</span>
                  </div>
                  <template v-else>{{ emptyText ?? 'Nothing to show.' }}</template>
                </slot>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
    <div v-if="hasMore && !error" class="flex justify-end">
      <BaseButton size="sm" :loading="loadingMore" @click="$emit('load-more')">Load more</BaseButton>
    </div>
  </div>
</template>
