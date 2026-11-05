<script setup lang="ts">
// One-time display of recovery codes: copy, download as .txt, and an explicit "I saved them"
// confirmation before the parent may dismiss them (they cannot be shown again).
import { IconCopy, IconDownload } from '@tabler/icons-vue'
import { ref } from 'vue'
import { BaseAlert, BaseButton, BaseCard, CheckboxInput } from '@/components/base'
import { saveBlob } from '@/download'
import { useBrandStore } from '@/stores/brand'
import { useSessionStore } from '@/stores/session'

const props = defineProps<{ codes: string[] }>()
const emit = defineEmits<{ done: [] }>()
const session = useSessionStore()
const saved = ref(false)
const copied = ref<'ok' | 'failed' | null>(null)

function text(): string {
  const who = session.user?.email ? ` for ${session.user.email}` : ''
  return [
    `${useBrandStore().name} recovery codes${who}`,
    `Generated ${new Date().toISOString()}`,
    'Each code signs you in once if you lose your authenticator. Keep them private.',
    '',
    ...props.codes,
    '',
  ].join('\n')
}

async function copy() {
  try {
    await navigator.clipboard.writeText(text())
    copied.value = 'ok'
  } catch {
    copied.value = 'failed'
  }
}

function download() {
  const slug = useBrandStore().name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'account'
  saveBlob(new Blob([text()], { type: 'text/plain;charset=utf-8' }), `${slug}-recovery-codes.txt`)
}
</script>

<template>
  <div class="flex flex-col gap-3">
    <BaseAlert tone="warn">
      Save these recovery codes now. They are shown only once. Each code signs you in a single time if you lose
      access to your authenticator app.
    </BaseAlert>
    <BaseCard as="ol" :padded="false" tone="subtle" class="m-0 grid list-none grid-cols-2 gap-x-6 gap-y-1.5 p-4" aria-label="Recovery codes">
      <li v-for="c in codes" :key="c" class="lg-mono text-[13.5px] tracking-wide select-all">{{ c }}</li>
    </BaseCard>
    <div class="flex flex-wrap items-center gap-2">
      <BaseButton size="sm" @click="copy">
        <template #icon><IconCopy :size="14" stroke="1.75" aria-hidden="true" /></template>
        Copy
      </BaseButton>
      <BaseButton size="sm" @click="download">
        <template #icon><IconDownload :size="14" stroke="1.75" aria-hidden="true" /></template>
        Download .txt
      </BaseButton>
      <span v-if="copied" role="status" class="text-xs" :class="copied === 'ok' ? 'text-public' : 'text-warn'">
        {{ copied === 'ok' ? 'Copied to the clipboard.' : 'Copying is blocked here; select the codes or download them.' }}
      </span>
    </div>
    <CheckboxInput v-model="saved" label="I saved these recovery codes somewhere safe" />
    <div>
      <BaseButton variant="primary" :disabled="!saved" @click="emit('done')">Done</BaseButton>
    </div>
  </div>
</template>
