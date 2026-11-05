<script setup lang="ts">
// Renders an otpauth:// URI as a QR code on a canvas, locally (the `qrcode` package; nothing is
// sent anywhere). Always dark-on-white with a quiet zone so phone cameras read it in dark mode.
import QRCode from 'qrcode'
import { onMounted, ref, watch } from 'vue'

const props = withDefaults(defineProps<{ uri: string; size?: number }>(), { size: 192 })
const canvas = ref<HTMLCanvasElement | null>(null)
const failed = ref(false)

/** Reads the theme-invariant --kase-qr-* tokens (style.css); canvas drawing needs concrete colors. */
function token(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}

async function draw() {
  if (!canvas.value) return
  try {
    await QRCode.toCanvas(canvas.value, props.uri, {
      errorCorrectionLevel: 'M',
      margin: 3,
      width: props.size,
      color: { dark: token('--kase-qr-dark'), light: token('--kase-qr-light') },
    })
    failed.value = false
  } catch {
    failed.value = true
  }
}
onMounted(draw)
watch(() => props.uri, draw)
</script>

<template>
  <div class="flex-none rounded-md border border-border bg-qr-light p-1" :style="{ width: `${size + 10}px` }">
    <canvas v-show="!failed" ref="canvas" role="img" aria-label="QR code for your authenticator app. If you cannot scan it, enter the setup key shown next to it." class="block" />
    <p v-if="failed" class="m-0 p-3 text-xs text-qr-dark">The QR code could not be drawn. Enter the setup key manually.</p>
  </div>
</template>
