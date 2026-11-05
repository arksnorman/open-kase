<script setup lang="ts">
// Logs: the audit log (default) and the email/webhook delivery log in one place.
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { deliveriesApi } from '@/api/endpoints'
import { BaseTabs, type TabDef } from '@/components/base'
import AuditLog from '@/components/logs/AuditLog.vue'
import DeliveryLog from '@/components/notifications/DeliveryLog.vue'
import AdminPage from './AdminPage.vue'

type Tab = 'audit' | 'deliveries'
const tabs: TabDef<Tab>[] = [
  { id: 'audit', label: 'Audit log' },
  { id: 'deliveries', label: 'Deliveries' },
]
const route = useRoute()
const router = useRouter()
const tab = computed<Tab>({
  get: () => (route.query.tab === 'deliveries' ? 'deliveries' : 'audit'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v === 'audit' ? undefined : v } }),
})
</script>

<template>
  <AdminPage
    title="Logs"
    description="Who did what (audit log), and every email and webhook notification with its delivery state. Delivery is at least once; failed entries can be retried."
  >
    <BaseTabs v-model="tab" :tabs="tabs" label="Log type" variant="compact">
      <template #audit>
        <div class="mt-4"><AuditLog /></div>
      </template>
      <template #deliveries>
        <div class="mt-4">
          <DeliveryLog
            label="Notification deliveries"
            show-channel
            :fetch-page="(q, signal) => deliveriesApi.list(q, { signal })"
            :retry="(d) => deliveriesApi.retry(d.id)"
          />
        </div>
      </template>
    </BaseTabs>
  </AdminPage>
</template>
