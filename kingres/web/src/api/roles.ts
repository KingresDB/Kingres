// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Role checks. Which role an operation needs comes from the contract (`x-required-role`, copied
// into operations.generated.ts), never from a hand-kept list in the UI.
import { operations, type OperationId, type RequiredRole } from './operations.generated'
import type { ClusterRoleName } from './types'

/** Hierarchy from the contract: admin includes ops, ops includes readonly, readonly includes stats. */
const RANK: Record<RequiredRole, number> = {
  none: 0,
  kingres_stats: 1,
  kingres_readonly: 2,
  kingres_ops: 3,
  kingres_admin: 4,
}

/** The highest built-in role in a list, or null when the account has none. */
export function highestRole(roles: readonly ClusterRoleName[]): ClusterRoleName | null {
  let best: ClusterRoleName | null = null
  for (const r of roles) if (best === null || RANK[r] > RANK[best]) best = r
  return best
}

/** True when an account holding `roles` may call something that needs `need`. */
export function roleSatisfies(roles: readonly ClusterRoleName[], need: RequiredRole): boolean {
  const best = highestRole(roles)
  return RANK[need] <= (best === null ? 0 : RANK[best])
}

/** True when an account holding `roles` may call the operation. */
export function canCall(roles: readonly ClusterRoleName[], operation: OperationId): boolean {
  return roleSatisfies(roles, operations[operation].role)
}

/** The role an operation needs. */
export function requiredRoleFor(operation: OperationId): RequiredRole {
  return operations[operation].role
}

/** Explanation shown next to a disabled control. */
export function denialReason(operation: OperationId): string {
  const need = operations[operation].role
  return need === 'none' ? '' : `Requires the ${need} cluster role.`
}

export type { OperationId, RequiredRole }
