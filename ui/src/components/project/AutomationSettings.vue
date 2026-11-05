<script setup lang="ts">
import { IconArrowRight } from '@tabler/icons-vue'
// Lifecycle automation (GET/PUT /automation, admins, If-Match).
// Every rule is off (0) by default; turning a rule off sends 0.
import { computed, ref, watch } from 'vue'
import { workflowApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Automation, AutomationInput } from '@/api/types'
import { BaseAlert, BaseBadge, BaseButton, LoadingState, SwitchInput, TextInput } from '@/components/base'
import { useQuery } from '@/composables/useQuery'

// The page's section list shows how many rules are on.
const emit = defineEmits<{ count: [n: number] }>()
const automationQ = useQuery(
  () => 'automation',
  (signal) => workflowApi.automation({ signal }),
)
const settings = computed(() => automationQ.data.value)

interface Form {
  archive: boolean
  archiveDays: string
  remind: boolean
  remindHours: string
  maxReminders: string
  cooldownHours: string
  autoResolve: boolean
  autoResolveHours: string
}
function toForm(p: Automation | undefined): Form {
  return {
    archive: !!p?.archiveAfterResolvedDays,
    archiveDays: String(p?.archiveAfterResolvedDays || 30),
    remind: !!p?.customerFollowupAfterHours,
    remindHours: String(p?.customerFollowupAfterHours || 72),
    maxReminders: String(p?.maxReminders || 2),
    cooldownHours: String(p?.reminderCooldownHours || 24),
    autoResolve: !!p?.autoResolveAfterHours,
    autoResolveHours: String(p?.autoResolveAfterHours || 72),
  }
}
const form = ref<Form>(toForm(undefined))
let keepOnNextLoad = false
watch(
  settings,
  (p) => {
    if (!p) return
    if (keepOnNextLoad) {
      keepOnNextLoad = false
      return
    }
    form.value = toForm(p)
  },
  { immediate: true, flush: 'sync' },
)
// Auto-resolve needs reminders (the requester is told before the ticket is resolved).
watch(
  () => form.value.remind,
  (on) => {
    if (!on) form.value.autoResolve = false
  },
)

const int = (v: string) => {
  const n = Number(v)
  return v.trim() && Number.isInteger(n) ? n : NaN
}
const input = computed<AutomationInput>(() => {
  const f = form.value
  return {
    archiveAfterResolvedDays: f.archive ? int(f.archiveDays) : 0,
    customerFollowupAfterHours: f.remind ? int(f.remindHours) : 0,
    maxReminders: int(f.maxReminders),
    reminderCooldownHours: int(f.cooldownHours),
    autoResolveAfterHours: f.remind && f.autoResolve ? int(f.autoResolveHours) : 0,
  }
})

// Client-side range checks mirror the server catalog; the server stays authoritative.
const local = computed<Record<string, string>>(() => {
  const i = input.value
  const out: Record<string, string> = {}
  const range = (key: keyof AutomationInput, v: number, min: number, max: number, unit: string) => {
    if (!Number.isInteger(v) || v < min || v > max) out[key] = `Enter a whole number of ${unit} from ${min} to ${max}.`
  }
  if (form.value.archive) range('archiveAfterResolvedDays', i.archiveAfterResolvedDays, 1, 3650, 'days')
  if (form.value.remind) {
    range('customerFollowupAfterHours', i.customerFollowupAfterHours, 1, 2160, 'hours')
    range('maxReminders', i.maxReminders, 1, 5, 'reminders')
    range('reminderCooldownHours', i.reminderCooldownHours, 1, 2160, 'hours')
    if (form.value.autoResolve) range('autoResolveAfterHours', i.autoResolveAfterHours, 1, 2160, 'hours')
  }
  return out
})

const dirty = computed(() => {
  const p = settings.value
  if (!p) return false
  const i = input.value
  return (
    i.archiveAfterResolvedDays !== p.archiveAfterResolvedDays ||
    i.customerFollowupAfterHours !== p.customerFollowupAfterHours ||
    i.autoResolveAfterHours !== p.autoResolveAfterHours ||
    // Reminder settings only matter while reminders are on.
    (form.value.remind && (i.maxReminders !== p.maxReminders || i.reminderCooldownHours !== p.reminderCooldownHours))
  )
})

const busy = ref(false)
const error = ref<unknown>(null)
const conflict = ref(false)
const saved = ref(false)
const fe = (f: keyof AutomationInput) => local.value[f] ?? (error.value instanceof ApiError ? error.value.field(f) : undefined)
async function save() {
  const p = settings.value
  if (!p || !dirty.value || Object.keys(local.value).length) return
  const body = { ...input.value }
  // Keep the stored reminder settings when reminders stay off (the server requires valid values).
  if (!form.value.remind) {
    body.maxReminders = p.maxReminders || 2
    body.reminderCooldownHours = p.reminderCooldownHours || 24
  }
  busy.value = true
  error.value = null
  conflict.value = false
  saved.value = false
  try {
    automationQ.set(await workflowApi.saveAutomation(body, p.version))
    saved.value = true
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      conflict.value = true
      keepOnNextLoad = true
      await automationQ.refetch()
    } else error.value = err
  } finally {
    busy.value = false
  }
}
// Saved state only: the badge in the section list must not count an unsaved draft.
watch(
  settings,
  (p) => emit('count', p ? [p.customerFollowupAfterHours, p.autoResolveAfterHours, p.archiveAfterResolvedDays].filter((n) => n > 0).length : 0),
  { immediate: true },
)
const generalError = computed(() => (error.value instanceof ApiError && error.value.fieldErrors.length ? 'Check the highlighted fields.' : error.value))
</script>

<template>
  <section id="rules" tabindex="-1" aria-labelledby="rules-h" class="lg-panel flex scroll-mt-4 flex-col gap-4 p-4 outline-none">
    <div>
      <h2 id="rules-h" class="lg-h3">Ticket rules</h2>
      <p class="m-0 mt-0.5 max-w-[58ch] text-muted">What happens to tickets waiting on the requester, and to resolved tickets. Rules run in the background about every 15 minutes; archived tickets are never touched and any status change or public reply restarts a ticket’s clock.</p>
    </div>
    <BaseAlert v-if="automationQ.error.value" :error="automationQ.error.value" />
    <LoadingState v-if="automationQ.loading.value && !settings" />
    <form v-if="settings" class="flex flex-col gap-3" novalidate @submit.prevent="save">
      <ol class="m-0 flex list-none flex-wrap items-center gap-2 p-0 text-xs text-muted" aria-label="Order of events">
        <li class="rounded-full bg-subtle px-2 py-0.5 font-medium text-text">Awaiting customer</li><li aria-hidden="true" class="flex items-center"><IconArrowRight :size="14" stroke="1.75" /></li>
        <li class="rounded-full bg-subtle px-2 py-0.5 font-medium text-text">Reminders</li><li aria-hidden="true" class="flex items-center"><IconArrowRight :size="14" stroke="1.75" /></li>
        <li class="rounded-full bg-subtle px-2 py-0.5 font-medium text-text">Resolved</li><li aria-hidden="true" class="flex items-center"><IconArrowRight :size="14" stroke="1.75" /></li>
        <li class="rounded-full bg-subtle px-2 py-0.5 font-medium text-text">Archived</li>
      </ol>

      <BaseAlert v-if="generalError" :error="generalError" />
      <BaseAlert v-if="conflict" tone="warn">These ticket rules were changed by someone else. The latest version is loaded; your choices are kept — review and save again.</BaseAlert>

      <!-- Remind -->
      <div class="flex flex-col gap-2.5 rounded-[10px] border border-border p-3">
        <div class="flex items-start gap-3">
          <SwitchInput id="rule-remind" v-model="form.remind" labelledby="rule-remind-l" describedby="rule-remind-d" class="mt-0.5" />
          <div class="flex min-w-0 flex-col">
            <span class="flex items-center gap-2"><strong id="rule-remind-l">Remind the requester</strong><BaseBadge :tone="form.remind ? 'success' : 'neutral'">{{ form.remind ? 'On' : 'Off' }}</BaseBadge></span>
            <span id="rule-remind-d" class="text-xs text-muted">When a ticket waits on them, email a short reminder (counted from entering “Awaiting customer” or the last public staff reply). No staff email or webhook is sent.</span>
          </div>
        </div>
        <div v-if="form.remind" class="flex flex-wrap items-center gap-2 pl-[52px] text-muted" role="group" aria-label="Reminder timing">
          <span>First after</span>
          <TextInput v-model="form.remindHours" type="number" size="sm" mono class="!w-[72px] text-right" aria-label="Hours until the first reminder" :invalid="!!fe('customerFollowupAfterHours')" />
          <span>hours, then every</span>
          <TextInput v-model="form.cooldownHours" type="number" size="sm" mono class="!w-[72px] text-right" aria-label="Hours between reminders" :invalid="!!fe('reminderCooldownHours')" />
          <span>hours, at most</span>
          <TextInput v-model="form.maxReminders" type="number" size="sm" mono class="!w-[72px] text-right" aria-label="Maximum number of reminders" :invalid="!!fe('maxReminders')" />
          <span>times</span>
        </div>
        <div v-if="form.remind" class="flex flex-col gap-0.5 pl-[52px]" role="alert">
          <span v-for="k in (['customerFollowupAfterHours', 'reminderCooldownHours', 'maxReminders'] as const)" :key="k" v-show="fe(k)" class="lg-field-error">{{ fe(k) }}</span>
        </div>
      </div>

      <!-- Auto-resolve -->
      <div class="flex flex-col gap-2.5 rounded-[10px] border border-border p-3">
        <div class="flex items-start gap-3">
          <SwitchInput id="rule-resolve" v-model="form.autoResolve" :disabled="!form.remind" labelledby="rule-resolve-l" describedby="rule-resolve-d" class="mt-0.5" />
          <div class="flex min-w-0 flex-col">
            <span class="flex items-center gap-2"><strong id="rule-resolve-l">Resolve unanswered tickets</strong><BaseBadge :tone="form.autoResolve ? 'success' : 'neutral'">{{ form.autoResolve ? 'On' : 'Off' }}</BaseBadge></span>
            <span id="rule-resolve-d" class="text-xs text-muted">
              {{ form.remind
                ? 'Resolves a still-waiting ticket if the requester has not replied; the last reminder tells them. They get the normal “resolved” email and can reply to reopen it.'
                : 'Needs reminders (turn them on above), so the requester is always told before a ticket is resolved.' }}
            </span>
          </div>
        </div>
        <div v-if="form.remind && form.autoResolve" class="flex flex-wrap items-center gap-2 pl-[52px] text-muted" role="group" aria-label="Auto-resolve timing">
          <span>Resolve</span>
          <TextInput v-model="form.autoResolveHours" type="number" size="sm" mono class="!w-[72px] text-right" aria-label="Hours after the last reminder" :invalid="!!fe('autoResolveAfterHours')" />
          <span>hours after the last reminder</span>
        </div>
        <span v-if="form.remind && form.autoResolve && fe('autoResolveAfterHours')" class="lg-field-error pl-[52px]" role="alert">{{ fe('autoResolveAfterHours') }}</span>
      </div>

      <!-- Archive -->
      <div class="flex flex-col gap-2.5 rounded-[10px] border border-border p-3">
        <div class="flex items-start gap-3">
          <SwitchInput id="rule-archive" v-model="form.archive" labelledby="rule-archive-l" describedby="rule-archive-d" class="mt-0.5" />
          <div class="flex min-w-0 flex-col">
            <span class="flex items-center gap-2"><strong id="rule-archive-l">Archive resolved tickets</strong><BaseBadge :tone="form.archive ? 'success' : 'neutral'">{{ form.archive ? 'On' : 'Off' }}</BaseBadge></span>
            <span id="rule-archive-d" class="text-xs text-muted">Archiving is permanent: archived tickets cannot be reopened or changed, and further work needs a linked follow-up ticket.</span>
          </div>
        </div>
        <div v-if="form.archive" class="flex flex-wrap items-center gap-2 pl-[52px] text-muted" role="group" aria-label="Archive timing">
          <span>Archive</span>
          <TextInput v-model="form.archiveDays" type="number" size="sm" mono class="!w-[72px] text-right" aria-label="Days after resolving" :invalid="!!fe('archiveAfterResolvedDays')" />
          <span>days after resolving</span>
        </div>
        <span v-if="form.archive && fe('archiveAfterResolvedDays')" class="lg-field-error pl-[52px]" role="alert">{{ fe('archiveAfterResolvedDays') }}</span>
      </div>

      <div class="flex items-center gap-3">
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!dirty || Object.keys(local).length > 0">Save</BaseButton>
        <span v-if="saved && !dirty" role="status" class="text-xs text-muted">Saved.</span>
      </div>
    </form>
  </section>
</template>
