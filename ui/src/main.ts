import { createPinia } from 'pinia'
import { createApp, watch } from 'vue'
import App from './App.vue'
import router, { retitle } from './router'
import { useBrandStore } from './stores/brand'
import { useSessionStore } from './stores/session'
import { usePreferencesStore } from './stores/preferences'
import { applyTheme } from './theme/theme'
import { isChunkLoadError, markStaleBuild, watchForStaleBuild } from './staleBuild'
import { vTooltip } from './components/base/tooltip'
import './style.css'

const app = createApp(App)
app.use(createPinia())
app.directive('tooltip', vTooltip)

// Instantiate early: preferences re-applies the theme the inline script set, and the
// session store registers the API client's refresh/expiry listener before any request.
applyTheme(usePreferencesStore().theme)
useSessionStore()
void useBrandStore().load()
// After Pinia is installed: the tab title follows the configured product name (from /meta, or after a settings save).
watch(() => useBrandStore().name, retitle)

// Last resort for errors no ErrorBoundary handled (event handlers, watchers, the shells):
// log only. Views report their own failures; a stale lazy chunk shows the reload banner.
app.config.errorHandler = (err, _instance, info) => {
  if (isChunkLoadError(err)) markStaleBuild()
  console.error('[ui] unhandled error:', err, info)
}
watchForStaleBuild()

app.use(router)
// Mount even if the first navigation failed (e.g. a stale lazy chunk), so the reload banner shows.
void router
  .isReady()
  .catch(() => undefined)
  .then(() => app.mount('#app'))
