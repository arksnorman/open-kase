// Single source of product metadata. Branding is provisional; change it here only.
export const product = {
  name: 'open-kase',
  version: '0.2.0',
  apiBase: '/api/v1',
} as const

export type Product = typeof product
