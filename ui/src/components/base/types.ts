import type { Component } from 'vue'
import type { AvatarRole } from './BaseAvatar.vue'

export interface Column {
  key: string
  label: string
  /**
   * CSS width, e.g. "92px" or "18%". Omit for flexible columns (they share what is left equally).
   * A percentage is a share of the whole table: it widens the table's minimum width (DataTable).
   */
  width?: string
  class?: string
}

export interface MenuItem {
  label: string
  action: () => void
  tone?: 'default' | 'danger'
  disabled?: boolean
}

export interface Segment<T> {
  value: T
  label: string
  icon?: Component
  /** `note`: shown selected in the agent-note amber instead of the accent. */
  tone?: 'note'
}

export interface SelectOption<T> {
  value: T
  label: string
  disabled?: boolean
  /** Secondary text on the option row (and searched), e.g. a project key or an email. */
  hint?: string
  /** Options sharing a group are listed under that heading, in first-seen order. */
  group?: string
  /** Extra search terms that are not displayed. */
  keywords?: string
  /** Leading actor avatar (decorative; the label names the person). */
  avatar?: AvatarRole
}

export interface TabDef<I> {
  id: I
  label: string
  /** Optional decorative icon before the label (Tabler component). */
  icon?: Component
}
