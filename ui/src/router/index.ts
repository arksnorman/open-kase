import { createRouter, createWebHistory, type RouteLocationNormalizedLoaded, type RouteLocationRaw } from 'vue-router'
import AdminShell from '@/layouts/AdminShell.vue'
import AppShell from '@/layouts/AppShell.vue'
import { DEFAULT_QUEUE } from '@/queues'
import { useSessionStore } from '@/stores/session'
import { useWorkspaceStore } from '@/stores/workspace'
import { useBrandStore } from '@/stores/brand'
import { isChunkLoadError, markStaleBuild } from '@/staleBuild'

declare module 'vue-router' {
  interface RouteMeta {
    public?: boolean
    /** Administrators only (convenience; the API enforces it on every request). */
    admin?: boolean
    title?: string
  }
}

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/sign-in', name: 'sign-in', component: () => import('@/views/SignInView.vue'), meta: { public: true, title: 'Sign in' } },
    { path: '/forgot-password', name: 'forgot-password', component: () => import('@/views/auth/ForgotPasswordView.vue'), meta: { public: true, title: 'Reset your password' } },
    { path: '/reset-password', name: 'reset-password', component: () => import('@/views/auth/ResetPasswordView.vue'), meta: { public: true, title: 'Choose a new password' } },
    { path: '/accept-invitation', name: 'accept-invitation', component: () => import('@/views/auth/AcceptInvitationView.vue'), meta: { public: true, title: 'Accept invitation' } },
    {
      path: '/account',
      component: () => import('@/layouts/AccountShell.vue'),
      children: [
        { path: '', name: 'account', redirect: { name: 'account-security' } },
        { path: 'security', name: 'account-security', component: () => import('@/views/account/SecurityView.vue'), meta: { title: 'Security' } },
      ],
    },
    {
      path: '/',
      component: AppShell,
      children: [
        { path: '', name: 'home', redirect: { name: 'queue', params: { view: DEFAULT_QUEUE } } },
        { path: 'queue/:view', name: 'queue', component: () => import('@/views/QueueView.vue'), meta: { title: 'Queue' } },
        { path: 'tickets/new', name: 'ticket-new', component: () => import('@/views/NewTicketView.vue'), meta: { title: 'New ticket' } },
        { path: 'tickets/:ticketNumber', name: 'ticket', component: () => import('@/views/TicketView.vue'), meta: { title: 'Ticket' } },
      ],
    },
    {
      path: '/admin',
      component: AdminShell,
      meta: { admin: true },
      children: [
        { path: '', name: 'admin', redirect: { name: 'admin-users' } },
        { path: 'users', name: 'admin-users', component: () => import('@/views/admin/UsersAdmin.vue'), meta: { title: 'Users' } },
        { path: 'users/:id', name: 'admin-user', component: () => import('@/views/admin/UserAdminView.vue'), meta: { title: 'User' } },
        { path: 'groups', name: 'admin-groups', component: () => import('@/views/admin/GroupsAdmin.vue'), meta: { title: 'Groups' } },
        { path: 'sla', name: 'admin-sla', component: () => import('@/views/admin/SlaAdmin.vue'), meta: { title: 'SLA' } },
        { path: 'canned-replies', name: 'admin-macros', component: () => import('@/views/admin/MacrosAdmin.vue'), meta: { title: 'Canned replies' } },
        { path: 'tags', name: 'admin-tags', component: () => import('@/views/admin/TagsAdmin.vue'), meta: { title: 'Tags' } },
        { path: 'hold-reasons', name: 'admin-hold-reasons', component: () => import('@/views/admin/HoldReasonsAdmin.vue'), meta: { title: 'Hold reasons' } },
        { path: 'organisations', name: 'admin-organisations', component: () => import('@/views/admin/OrganisationsAdmin.vue'), meta: { title: 'Organisations' } },
        { path: 'integrations', name: 'admin-integrations', component: () => import('@/views/admin/IntegrationsAdmin.vue'), meta: { title: 'Integrations' } },
        { path: 'webhooks', name: 'admin-webhooks', component: () => import('@/views/admin/WebhooksAdmin.vue'), meta: { title: 'Webhooks' } },
        { path: 'logs', name: 'admin-logs', component: () => import('@/views/admin/LogsAdmin.vue'), meta: { title: 'Logs' } },
        { path: 'reports', name: 'admin-reports', component: () => import('@/views/admin/ReportsAdmin.vue'), meta: { title: 'Reports' } },
        { path: 'configuration', name: 'admin-settings', component: () => import('@/views/admin/SettingsAdmin.vue'), meta: { title: 'Configuration' } },
      ],
    },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: () => import('@/views/NotFoundView.vue'), meta: { title: 'Not found' } },
  ],
})

/** Landing target for a signed-in user: the default queue. */
function homeTarget(): RouteLocationRaw {
  return { name: 'queue', params: { view: DEFAULT_QUEUE } }
}

router.beforeEach(async (to) => {
  const session = useSessionStore()
  const workspace = useWorkspaceStore()
  await session.bootstrap()

  if (to.meta.public) {
    return session.isAuthenticated && to.name === 'sign-in' ? homeTarget() : true
  }
  if (!session.isAuthenticated) {
    return to.name === 'home' ? { name: 'sign-in' } : { name: 'sign-in', query: { redirect: to.fullPath } }
  }
  // Admin MFA policy: everything but enrolment is refused by the API until MFA is confirmed.
  if (session.mfaEnrolmentRequired) return to.name === 'account-security' ? true : { name: 'account-security' }
  if (!workspace.loaded) {
    try {
      await workspace.load()
    } catch {
      // Keep navigation possible; pages show their own load errors with "Try again".
    }
  }
  if (to.matched.some((r) => r.meta.admin) && !session.isAdmin) return homeTarget()
  return true
})

/** A more precise page name set by the routed view for one path (see setViewTitle). */
let viewTitle: { path: string; title: string } | null = null

function applyTitle(to: RouteLocationNormalizedLoaded) {
  const own = viewTitle && viewTitle.path === to.path ? viewTitle.title : null
  const ref = typeof to.params.ticketNumber === 'string' && to.params.ticketNumber ? `#${to.params.ticketNumber.replace(/^#/, '')}` : null
  const parts = [own ?? to.meta.title, ref, useBrandStore().name]
  document.title = parts.filter((p) => typeof p === 'string' && p).join(' · ')
}

/**
 * Lets the routed view name the page more precisely than its route's title: the queue page
 * names its queue or group ("Assigned to me", "Delivery"), not just "Queue". Keyed by path, so
 * a title left behind by a view that is gone never applies to another page.
 */
export function setViewTitle(path: string, title: string | null): void {
  if (title) viewTitle = { path, title }
  else if (viewTitle?.path === path) viewTitle = null
  applyTitle(router.currentRoute.value)
}

router.afterEach((to) => applyTitle(to))
/** Re-applies the tab title for the current page (the configured product name arrived or changed). */
export function retitle(): void {
  applyTitle(router.currentRoute.value)
}

// A lazy route whose file is gone after a deploy: the navigation is abandoned (the current page
// stays usable) and the shells show "Reload to continue", which then opens the intended page.
router.onError((error, to) => {
  if (!isChunkLoadError(error)) return
  markStaleBuild(to ? router.resolve(to).href : undefined)
})

export default router
