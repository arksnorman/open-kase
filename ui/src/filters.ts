// Queue filters ↔ URL query ↔ API list parameters. The URL is the single
// source of truth, so a filtered list can be bookmarked and the back button works.
import type { LocationQuery, LocationQueryRaw } from 'vue-router'
import type { TicketFilterParams } from '@/api/endpoints'

/** SLA filter values (API `due`). */
export const SLA_FILTERS = ['dueSoon', 'breached', 'met', 'missed', 'running'] as const
export type SlaFilter = (typeof SLA_FILTERS)[number]

export interface QueueFilters {
  priorities: number[]
  statusIds: string[]
  tags: string[]
  requestTypeIds: string[]
  organisationIds: string[]
  /** '' = anyone; 'me' | 'unassigned' | user id. */
  assignee: string
  due: '' | SlaFilter
}

/** Filters a queue may fix by itself (and so hides). */
export type FilterKey = 'status' | 'assignee' | 'sla'

/** URL query keys used by the filter bar (besides `q` and `group`). */
const KEYS = ['priority', 'status', 'tag', 'type', 'organisation', 'assignee', 'due'] as const

function list(q: LocationQuery, key: string): string[] {
  const v = q[key]
  const raw = Array.isArray(v) ? v.join(',') : typeof v === 'string' ? v : ''
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .slice(0, 20)
}

export function emptyFilters(): QueueFilters {
  return { priorities: [], statusIds: [], tags: [], requestTypeIds: [], organisationIds: [], assignee: '', due: '' }
}

export function filtersFromQuery(q: LocationQuery): QueueFilters {
  const due = list(q, 'due')[0]
  return {
    priorities: list(q, 'priority')
      .map(Number)
      .filter((n) => Number.isInteger(n) && n >= 1 && n <= 4),
    statusIds: list(q, 'status'),
    tags: list(q, 'tag'),
    requestTypeIds: list(q, 'type'),
    organisationIds: list(q, 'organisation'),
    assignee: list(q, 'assignee')[0] ?? '',
    due: (SLA_FILTERS as readonly string[]).includes(due ?? '') ? (due as SlaFilter) : '',
  }
}

/** Filters a queue fixes itself are dropped (a URL may still carry them). */
export function withoutFixed(f: QueueFilters, fixed: readonly FilterKey[]): QueueFilters {
  const out = { ...f }
  if (fixed.includes('status')) out.statusIds = []
  if (fixed.includes('assignee')) out.assignee = ''
  if (fixed.includes('sla')) out.due = ''
  return out
}

/** Replace the filter keys of `base` with `f` (empty filters are removed). */
export function filtersToQuery(f: QueueFilters, base: LocationQuery): LocationQueryRaw {
  const out: LocationQueryRaw = { ...base }
  for (const k of KEYS) delete out[k]
  const join = (a: (string | number)[]) => (a.length ? a.join(',') : undefined)
  Object.assign(out, {
    priority: join([...f.priorities].sort()),
    status: join(f.statusIds),
    tag: join(f.tags),
    type: join(f.requestTypeIds),
    organisation: join(f.organisationIds),
    assignee: f.assignee || undefined,
    due: f.due || undefined,
  })
  for (const k of KEYS) if (out[k] === undefined) delete out[k]
  return out
}

export function hasFilters(f: QueueFilters): boolean {
  return (
    !!f.priorities.length ||
    !!f.statusIds.length ||
    !!f.tags.length ||
    !!f.requestTypeIds.length ||
    !!f.organisationIds.length ||
    !!f.assignee ||
    !!f.due
  )
}

export function filtersToParams(f: QueueFilters): TicketFilterParams {
  const p: TicketFilterParams = {}
  if (f.priorities.length) p.priority = f.priorities.join(',')
  if (f.statusIds.length) p.statusId = f.statusIds.join(',')
  if (f.tags.length) p.tag = f.tags.join(',')
  if (f.requestTypeIds.length) p.requestTypeId = f.requestTypeIds.join(',')
  if (f.organisationIds.length) p.organisationId = f.organisationIds.join(',')
  if (f.assignee) p.assignee = f.assignee
  if (f.due) p.due = f.due
  return p
}
