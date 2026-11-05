<script setup lang="ts">
// A message's file that an administrator removed with a redaction: muted, not a button,
// and without the file name (the API no longer returns it).
import { IconFileOff } from '@tabler/icons-vue'
</script>

<template>
  <span class="lg-attach border-dashed text-muted" role="note" aria-label="Attachment removed by an administrator">
    <IconFileOff :size="16" stroke="1.75" aria-hidden="true" />
    <span class="text-xs">Attachment removed</span>
  </span>
</template>
