<script setup lang="ts">
// Ticket tags: shown as chips; the pencil opens the tag picker, and Save (or leaving it)
// replaces the whole set with PATCH + If-Match, so a concurrent change
// is reported (and the ticket refreshed) instead of being overwritten.
// Only catalog tags can be picked: admins manage the catalog (Admin → Tags).
import { computed, useId } from 'vue'
import { tagsApi, ticketsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Ticket } from '@/api/types'
import { BaseBadge, InlineEditField, SelectInput, type SelectOption } from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'

const props = defineProps<{ ticket: Ticket; disabled?: boolean }>()
const emit = defineEmits<{ updated: [ticket: Ticket]; stale: [] }>()

const MAX = 20
const uid = useId()

const suggestions = useQuery(
  () => `tags`,
  (signal) => tagsApi.list({ signal }),
)
const tagOptions = computed<SelectOption<string>[]>(() => (suggestions.data.value?.items ?? []).map((t) => ({ value: t.tag, label: t.tag, hint: `${t.count}` })))

/** Replaces the tag set; throws the text to show beside the field. */
async function save(next: string[]) {
  if (next.length > MAX) throw `A ticket can carry at most ${MAX} tags.`
  try {
    emit('updated', await ticketsApi.update(props.ticket, { tags: [...new Set(next)].sort() }))
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      emit('stale')
      throw 'The ticket changed since you loaded it. It has been refreshed; your choice is kept — save again.'
    }
    if (err instanceof ApiError && err.is('ticketArchived')) {
      emit('stale')
      throw 'Archived tickets cannot be changed.'
    }
    if (err instanceof ApiError && err.fieldErrors.some((f) => f.code === 'unknownTag')) {
      invalidate('tags')
      throw err.fieldErrors.map((f) => f.message).join(' ')
    }
    throw err
  }
}
</script>

<template>
  <dt :id="`${uid}-h`" class="text-muted">Tags</dt>
  <dd class="m-0 min-w-0">
    <InlineEditField label="tags" mode="multi" :value="ticket.tags" :editable="!disabled" @save="save">
      <ul v-if="ticket.tags.length" class="m-0 inline-flex list-none flex-wrap gap-1.5 p-0 align-middle" aria-label="Ticket tags">
        <li v-for="t in ticket.tags" :key="t"><BaseBadge class="!text-text">{{ t }}</BaseBadge></li>
      </ul>
      <span v-else class="text-muted">No tags</span>
      <template #editor="{ editorRef, draft, setDraft, errorId }">
        <SelectInput
          :ref="editorRef"
          :model-value="draft as string[]"
          multiple
          searchable
          :options="tagOptions"
          :max-chips="MAX"
          :aria-describedby="`${uid}-hint ${errorId}`"
          :aria-labelledby="`${uid}-h`"
          label="Tags"
          placeholder="Add tags"
          search-placeholder="Find a tag"
          empty-text="No other tags. An admin adds tags in Settings, under Tags."
          size="sm"
          @update:model-value="setDraft"
        />
        <span :id="`${uid}-hint`" class="text-xs text-muted">Tags from the tag list; at most {{ MAX }}.</span>
      </template>
    </InlineEditField>
  </dd>
</template>
