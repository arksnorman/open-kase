import type { Component } from 'vue'
import {
  IconArchive,
  IconCircleCheck,
  IconEye,
  IconHourglass,
  IconPlayerPause,
  IconInbox,
  IconList,
  IconAlarm,
  IconUserCheck,
} from '@tabler/icons-vue'
import type { QueueCounts, QueueView } from '@/api/types'
import type { FilterKey } from '@/filters'

export interface QueueDef {
  slug: string
  view: QueueView
  label: string
  icon: Component
  count?: keyof QueueCounts
  warnCount?: boolean
  /** Empty-state text of the unfiltered queue. */
  empty?: string
  /** Filters the view fixes itself: hidden in the filter bar. */
  fixes?: FilterKey[]
}

/** Default agent views (CLAUDE.md "UI and quality"). URL slug ↔ API `view` value. */
export const QUEUES: QueueDef[] = [
  { slug: 'assigned-to-me', view: 'myWork', label: 'Assigned to me', icon: IconUserCheck, count: 'myWork', fixes: ['assignee'],
    empty: 'Nothing is assigned to you. Assign yourself a ticket from Unassigned (in Details, under Assignee).' },
  { slug: 'unassigned', view: 'unassigned', label: 'Unassigned', icon: IconInbox, count: 'unassigned', fixes: ['assignee'],
    empty: 'No unassigned tickets in your groups. New tickets wait here until someone takes them.' },
  { slug: 'watching', view: 'watching', label: 'Watching', icon: IconEye, count: 'watching' },
  { slug: 'awaiting-customer', view: 'awaitingCustomer', label: 'Awaiting customer', icon: IconHourglass, count: 'awaitingCustomer' },
  { slug: 'on-hold', view: 'onHold', label: 'On hold', icon: IconPlayerPause, count: 'onHold',
    empty: 'No tickets are on hold. A held ticket returns to Open on its own when its hold period ends.' },
  { slug: 'due-soon', view: 'dueSoon', label: 'Due soon', icon: IconAlarm, count: 'dueSoon', warnCount: true, fixes: ['sla'] },
  { slug: 'resolved', view: 'resolved', label: 'Resolved', icon: IconCircleCheck, fixes: ['status'] },
  { slug: 'archived', view: 'archived', label: 'Archived', icon: IconArchive, fixes: ['status'] },
  { slug: 'all', view: 'all', label: 'All tickets', icon: IconList },
]

export const DEFAULT_QUEUE = 'assigned-to-me'

export function queueBySlug(slug: string): QueueDef | undefined {
  return QUEUES.find((q) => q.slug === slug)
}
