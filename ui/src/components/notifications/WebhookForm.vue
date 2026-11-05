<script setup lang="ts">
// Settings of a webhook endpoint: URL, events, authentication/headers/TLS. Used whole on the webhook
// page (Settings tab) and one `section` at a time by the New webhook dialog; all sections stay mounted
// so nothing typed is lost between steps. The parent owns the buttons and reads `input`.
// `includeAgentNotes` is offered to installation admins only; the agent-note event needs that grant.
import { computed, ref, watch } from 'vue'
import { ApiError } from '@/api/errors'
import type { Webhook, WebhookEventType, WebhookInput } from '@/api/types'
import { BaseAlert, BaseBadge, BaseCard, CheckboxInput, FormField, TextInput } from '@/components/base'
import { eventName } from '@/webhookLabels'
import WebhookRequestFields from './WebhookRequestFields.vue'

const props = withDefaults(
  defineProps<{
    eventTypes: WebhookEventType[]
    /** Admin: may grant agent notes. */
    canGrantNotes: boolean
    initial?: Webhook | null
    error?: unknown
    /** Which part to show; `all` shows every section. */
    section?: 'all' | 'endpoint' | 'events' | 'security'
  }>(),
  { section: 'all' },
)

const name = ref('')
const url = ref('')
const selected = ref<string[]>([])
const includeNotes = ref(false)
const request = ref<InstanceType<typeof WebhookRequestFields> | null>(null)

function reset() {
  const w = props.initial
  name.value = w?.name ?? ''
  url.value = w?.url ?? ''
  selected.value = [...(w?.eventTypes ?? [])]
  includeNotes.value = w?.includeAgentNotes ?? false
  request.value?.reset()
}
reset()
// Reload only for another webhook or a newer version, never over unsaved edits (a stats refresh or
// Pause/Resume replaces `initial` while someone is typing).
watch(
  () => [props.initial?.id, props.initial?.version],
  () => {
    if (!dirty.value) reset()
  },
)

const noteTypes = computed(() => new Set(props.eventTypes.filter((t) => t.requiresAgentNotes).map((t) => t.name)))
watch(includeNotes, (on) => {
  if (!on) selected.value = selected.value.filter((n) => !noteTypes.value.has(n))
})

function toggle(name: string, on: boolean) {
  selected.value = on ? [...new Set([...selected.value, name])] : selected.value.filter((n) => n !== name)
}

const fe = (f: string) => (props.error instanceof ApiError ? props.error.field(f) : undefined)
const generalError = computed(() => (props.error && !(props.error instanceof ApiError && props.error.fieldErrors.length) ? props.error : null))

const order = (n: string) => {
  const i = props.eventTypes.findIndex((t) => t.name === n)
  return i < 0 ? 999 : i
}

/** The members to send: everything on create, only the changes on update. */
const input = computed<WebhookInput>(() => {
  const w = props.initial
  const out: WebhookInput = {}
  const trimmedName = name.value.trim()
  const trimmed = url.value.trim()
  const types = props.eventTypes.map((t) => t.name).filter((n) => selected.value.includes(n))
  if (!w || trimmedName !== w.name) out.name = trimmedName
  if (!w || trimmed !== w.url) out.url = trimmed
  if (!w || types.join() !== [...w.eventTypes].sort((a, b) => order(a) - order(b)).join()) out.eventTypes = types
  if (props.canGrantNotes && (w ? includeNotes.value !== w.includeAgentNotes : includeNotes.value)) out.includeAgentNotes = includeNotes.value
  Object.assign(out, request.value?.toInput() ?? {})
  return out
})
const dirty = computed(() => Object.keys(input.value).length > 0)
const endpointOk = computed(() => !!name.value.trim() && !!url.value.trim())
const eventsOk = computed(() => selected.value.length > 0)
defineExpose({ input, dirty, endpointOk, eventsOk, reset })

const cards = computed(() => props.section === 'all')
const EVENT_HINTS: Record<string, string> = {
  'ticket.updated': 'Priority, group, tags, org fields…',
  'ticket.message.agentNote': 'Staff-only content. Admin grant required.',
}
const show = (s: 'endpoint' | 'events' | 'security') => props.section === 'all' || props.section === s
</script>

<template>
  <div class="flex flex-col" :class="cards ? 'gap-4' : 'gap-5'">
    <BaseAlert v-if="generalError" :error="generalError" />

    <component :is="cards ? BaseCard : 'section'" v-show="show('endpoint')" v-bind="cards ? { title: 'Endpoint' } : {}" class="flex flex-col gap-3" aria-label="Endpoint">
      <p v-if="cards" class="m-0 -mt-2 text-xs text-muted">HTTPS. Private network addresses work.</p>
      <FormField label="Name" :error="fe('name')" required>
        <TextInput v-model="name" maxlength="80" autocomplete="off" placeholder="CRM sync" />
      </FormField>
      <FormField label="Endpoint URL" :hint="cards ? undefined : 'HTTPS. Private network addresses work.'" :error="fe('url')" required>
        <TextInput v-model="url" type="url" mono autocomplete="off" placeholder="https://example.com/hooks/open-kase" />
      </FormField>
    </component>

    <component :is="cards ? BaseCard : 'section'" v-show="show('events')" v-bind="cards ? { title: 'Events' } : {}" class="flex flex-col gap-3" aria-label="Events">
      <fieldset class="m-0 flex flex-col gap-2 border-0 p-0">
        <legend :class="cards ? 'lg-label sr-only' : 'lg-label mb-1 p-0'">Events</legend>
        <p class="m-0 text-xs text-muted" :class="cards ? '-mt-2' : ''">Choose what this system hears about.</p>
        <ul class="m-0 grid list-none gap-x-4 gap-y-2.5 p-0 sm:grid-cols-2">
          <li v-for="t in eventTypes" :key="t.name" class="flex flex-col gap-1.5">
            <CheckboxInput
              :id="`evt-${section}-${t.name}`"
              :label="eventName(t.name)"
              :model-value="selected.includes(t.name)"
              :disabled="t.requiresAgentNotes && !includeNotes"
              @update:model-value="toggle(t.name, $event)"
            >
              <template #hint>
                <span class="lg-mono text-[11px]">{{ t.name }}</span>
                <template v-if="EVENT_HINTS[t.name]"><br />{{ EVENT_HINTS[t.name] }}</template>
              </template>
            </CheckboxInput>
            <div v-if="t.requiresAgentNotes" class="ml-6 flex flex-col gap-1.5">
              <CheckboxInput v-if="canGrantNotes" v-model="includeNotes" id="wh-include-notes" label="Include agent notes" hint="Administrators only." />
              <span v-else-if="initial?.includeAgentNotes"><BaseBadge tone="warn">Agent notes included</BaseBadge></span>
              <p v-if="includeNotes" class="m-0 text-xs text-muted">Receives private staff note bodies. Only for trusted systems.</p>
              <span v-if="fe('includeAgentNotes')" class="lg-field-error">{{ fe('includeAgentNotes') }}</span>
            </div>
          </li>
        </ul>
        <span v-if="fe('eventTypes')" class="lg-field-error">{{ fe('eventTypes') }}</span>
      </fieldset>
    </component>

    <WebhookRequestFields v-show="show('security')" ref="request" :initial="initial" :url="url" :error="error" :cards="cards" />
  </div>
</template>
