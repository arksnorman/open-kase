<script setup lang="ts">
import { IconPlus } from '@tabler/icons-vue'
import { ref } from 'vue'
import { BaseButton } from '@/components/base'
import WebhooksPanel from '@/components/notifications/WebhooksPanel.vue'
import AdminPage from './AdminPage.vue'

const panel = ref<InstanceType<typeof WebhooksPanel> | null>(null)
</script>

<template>
  <AdminPage title="Webhooks" description="HTTPS notifications to external systems when tickets change.">
    <template v-if="panel?.listView" #actions>
      <BaseButton variant="primary" aria-haspopup="dialog" :disabled="!panel.canCreate" @click="panel.openCreate()">
        <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
        New webhook
      </BaseButton>
    </template>
    <WebhooksPanel ref="panel" />
  </AdminPage>
</template>
