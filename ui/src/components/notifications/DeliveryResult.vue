<script setup lang="ts">
// Inline outcome of a single delivery (test email / test webhook), updated while polling.
import { computed } from 'vue'
import type { Delivery } from '@/api/types'
import { BaseCard, RelativeTime, Spinner } from '@/components/base'
import { deliveryStatusExplanation, errorClassExplanation } from '@/deliveries'
import DeliveryStatusBadge from './DeliveryStatusBadge.vue'

const props = defineProps<{ delivery: Delivery; polling?: boolean; gaveUp?: boolean }>()
const errorHint = computed(() => errorClassExplanation(props.delivery.errorClass))
</script>

<template>
  <BaseCard as="div" size="sm" tone="subtle" role="status" aria-live="polite">
    <div class="flex flex-wrap items-center gap-2">
      <DeliveryStatusBadge :status="delivery.status" />
      <Spinner v-if="polling" :size="14" label="Checking delivery status" />
      <span class="text-xs text-muted">
        {{ delivery.attempts }} attempt{{ delivery.attempts === 1 ? '' : 's' }} · queued <RelativeTime :iso="delivery.createdAt" />
        <template v-if="delivery.nextAttemptAt"> · next try <RelativeTime :iso="delivery.nextAttemptAt" /></template>
      </span>
    </div>
    <p class="m-0 text-xs text-muted">{{ deliveryStatusExplanation(delivery.status) }}</p>
    <p v-if="delivery.errorClass || delivery.error" class="m-0 text-xs">
      <span v-if="delivery.errorClass" class="lg-mono">{{ delivery.errorClass }}</span>
      <template v-if="errorHint"> — {{ errorHint }}</template>
      <span v-if="delivery.error" class="lg-mono mt-0.5 block break-all text-muted">{{ delivery.error }}</span>
    </p>
    <p v-if="delivery.statusCode" class="m-0 text-xs">HTTP status <span class="lg-mono">{{ delivery.statusCode }}</span></p>
    <p v-if="gaveUp" class="m-0 text-xs text-muted">Stopped checking; the delivery is still in progress. Follow it in the delivery log.</p>
  </BaseCard>
</template>
