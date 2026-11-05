import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { adminApi } from '@/api/endpoints'
import type { GroupRef, Project } from '@/api/types'
import { useBrandStore } from './brand'
import { useSessionStore } from './session'

/**
 * The deployment's single project (its name is the configured product name) and the signed-in user's groups
 * (their queues). Loaded once after sign-in; the server stays authoritative on every request.
 */
export const useWorkspaceStore = defineStore('workspace', () => {
  const project = shallowRef<Project | null>(null)
  const myGroups = ref<GroupRef[]>([])
  const loaded = ref(false)
  const loadError = ref<unknown>(null)

  const name = computed(() => project.value?.name ?? '')

  async function load(signal?: AbortSignal): Promise<void> {
    const session = useSessionStore()
    try {
      loadError.value = null
      const o = signal ? { signal } : {}
      const me = session.user?.id
      // Only the signed-in user's own entry of the member directory (their groups), never the whole directory.
      const [p, mine] = await Promise.all([
        adminApi.project(o),
        me ? adminApi.members({ userIds: [me], limit: 1 }, o) : Promise.resolve(null),
      ])
      project.value = p
      useBrandStore().set(p.name)
      myGroups.value = mine?.items[0]?.groups ?? []
      loaded.value = true
    } catch (err) {
      loadError.value = err
      throw err
    }
  }

  function reset(): void {
    project.value = null
    myGroups.value = []
    loaded.value = false
    loadError.value = null
  }

  return { project, myGroups, loaded, loadError, name, load, reset }
})
