<script setup lang="ts">
// Actor marker. Without `role`: initials in a square tile (the signed-in user). With `role`: an icon in
// a tinted circle, named by `title`/`aria-label`:
//   staff (users) headset on indigo · requester (contacts) person on sky · bot (integration tokens) robot on cyan ·
//   system (system actors) cog on grey · webhook (endpoints) on orange · email (CC addresses).
// Sizes (role mode): 2xs 16px inside badges and chips, xs 20px tables and pickers, sm 28px thread headers, md 32px page headers.
// `admin` adds a shield badge (and "Administrator" in the name). `decorative` hides the marker from
// assistive technology where the role is obvious or named in adjacent text (assignee, watcher lists).
import { IconHeadset, IconMail, IconRobot, IconSettings, IconShieldFilled, IconUser, IconWebhook } from '@tabler/icons-vue'
import { computed } from 'vue'
import { initials } from '@/format'

export type AvatarRole = 'staff' | 'requester' | 'bot' | 'system' | 'webhook' | 'email'

const props = withDefaults(defineProps<{ name: string; size?: 'md' | 'sm' | 'xs' | '2xs'; role?: AvatarRole; decorative?: boolean; admin?: boolean }>(), { size: 'sm' })

const px = computed(() => (props.size === 'md' ? 32 : props.size === 'sm' ? 28 : props.size === 'xs' ? 20 : 16))
const icon = computed(() => (props.size === 'md' ? 18 : props.size === 'sm' ? 16 : props.size === 'xs' ? 12 : 10))
const initialsPx = computed(() => (props.size === 'md' ? 30 : props.size === 'sm' ? 26 : 20))
const labels: Record<AvatarRole, string> = { staff: 'Staff', requester: 'Requester', bot: 'Integration token', system: 'System', webhook: 'Webhook', email: 'Email address' }
const roleLabel = computed(() => (props.role ? labels[props.role] + (props.admin ? ', Administrator' : '') : ''))
const glyph = computed(() => {
  switch (props.role) {
    case 'requester':
      return IconUser
    case 'bot':
      return IconRobot
    case 'system':
      return IconSettings
    case 'webhook':
      return IconWebhook
    case 'email':
      return IconMail
    default:
      return IconHeadset
  }
})
const tone = computed(() =>
  props.role === 'requester' || props.role === 'email'
    ? 'lg-avatar-requester'
    : props.role === 'bot'
      ? 'lg-avatar-bot'
      : props.role === 'webhook'
        ? 'lg-avatar-webhook'
        : props.role === 'system'
          ? 'lg-avatar-neutral'
          : 'lg-avatar-staff',
)
const badge = computed(() => (props.size === 'xs' || props.size === '2xs' ? 9 : 11))
</script>

<template>
  <span
    v-if="role"
    :role="decorative ? undefined : 'img'"
    :aria-hidden="decorative ? 'true' : undefined"
    class="lg-avatar-role relative"
    :class="tone"
    :style="{ width: `${px}px`, height: `${px}px` }"
    :aria-label="decorative ? undefined : roleLabel"
    v-tooltip="roleLabel"
  >
    <component :is="glyph" :size="icon" stroke="1.9" aria-hidden="true" />
    <span v-if="admin" class="lg-avatar-badge" :style="{ width: `${badge + 3}px`, height: `${badge + 3}px` }" aria-hidden="true">
      <IconShieldFilled :size="badge" aria-hidden="true" />
    </span>
  </span>
  <span v-else aria-hidden="true" :style="{ width: `${initialsPx}px`, height: `${initialsPx}px` }" :class="size === 'md' ? 'lg-avatar' : size === 'xs' ? 'lg-avatar-xs' : 'lg-avatar-sm'">
    {{ initials(name) }}
  </span>
</template>
