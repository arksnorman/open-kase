<script setup lang="ts">
// Leading icon for an entity type in a round tinted tile (tokens --kase-tag, --kase-group, --kase-reply, --kase-hook, --kase-bot in style.css):
// tag teal · group violet · reply (canned reply) blue · webhook orange · bot (integration) cyan · hold reason (slate) · organisation emerald.
// Sizes: xs 24px (rows, pickers) · md 32px (page headers) · lg 56px (empty states). Decorative; the name is always in adjacent text.
import { IconBuildingSkyscraper, IconMessageBolt, IconPlayerPause, IconRobot, IconTag, IconUsersGroup, IconWebhook } from '@tabler/icons-vue'
import { computed } from 'vue'

export type EntityKind = 'tag' | 'group' | 'reply' | 'webhook' | 'bot' | 'holdReason' | 'organisation'

const props = withDefaults(defineProps<{ kind: EntityKind; size?: 'xs' | 'md' | 'lg' }>(), { size: 'xs' })

const glyphs = { tag: IconTag, group: IconUsersGroup, reply: IconMessageBolt, webhook: IconWebhook, bot: IconRobot, holdReason: IconPlayerPause, organisation: IconBuildingSkyscraper }
const px = computed(() => (props.size === 'lg' ? 56 : props.size === 'md' ? 32 : 24))
const icon = computed(() => (props.size === 'lg' ? 28 : props.size === 'md' ? 18 : 14))
</script>

<template>
  <span class="lg-avatar-role flex-none" :class="`lg-entity-${kind}`" :style="{ width: `${px}px`, height: `${px}px` }" aria-hidden="true">
    <component :is="glyphs[kind]" :size="icon" stroke="1.9" aria-hidden="true" />
  </span>
</template>
