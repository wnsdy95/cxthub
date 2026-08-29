import type { Ref } from './types';

export const BRANCH_LIFECYCLE_TAG_PREFIX = 'cxt/branch-state/v1/';

export interface BranchLifecycleEvent {
  branch: string;
  generation: bigint;
  state: 'active' | 'archived';
  target: string;
  ref: Ref;
}

export function parseBranchLifecycleRef(ref: Ref): BranchLifecycleEvent | null {
  if (ref.kind !== 'tag' || !ref.name.startsWith(BRANCH_LIFECYCLE_TAG_PREFIX)) return null;
  const rest = ref.name.slice(BRANCH_LIFECYCLE_TAG_PREFIX.length);
  const match = rest.match(/^([0-9]{20})\/(active|archived)\/([0-9a-f]{64})\/(.+)$/);
  if (!match) return null;
  const target = `sha256:${match[3]}`;
  if (ref.target !== target || !match[4]) return null;
  const generation = BigInt(match[1]);
  if (generation === 0n || generation.toString().padStart(20, '0') !== match[1]) return null;
  return { branch: match[4], generation, state: match[2] as 'active' | 'archived', target, ref };
}

function later(left: BranchLifecycleEvent, right: BranchLifecycleEvent): boolean {
  if (left.generation !== right.generation) return left.generation > right.generation;
  if (left.state !== right.state) return left.state === 'active';
  if (left.target !== right.target) return left.target > right.target;
  return left.ref.name > right.ref.name;
}

export function latestBranchLifecycle(refs: Ref[], branch: string): BranchLifecycleEvent | null {
  let latest: BranchLifecycleEvent | null = null;
  for (const ref of refs) {
    const event = parseBranchLifecycleRef(ref);
    if (!event || event.branch !== branch) continue;
    if (!latest || later(event, latest)) latest = event;
  }
  return latest;
}

export function branchLifecycleStates(refs: Ref[]): Map<string, BranchLifecycleEvent> {
  const states = new Map<string, BranchLifecycleEvent>();
  for (const ref of refs) {
    const event = parseBranchLifecycleRef(ref);
    if (!event) continue;
    const current = states.get(event.branch);
    if (!current || later(event, current)) states.set(event.branch, event);
  }
  return states;
}

/** Projects internal lifecycle events into the active branch list while
 * retaining the events themselves for reachability and replica convergence. */
export function projectBranchRefs(refs: Ref[]): Ref[] {
  const states = branchLifecycleStates(refs);
  const branches = new Map(refs.filter((ref) => ref.kind === 'branch').map((ref) => [ref.name, ref]));
  const out: Ref[] = [];
  for (const input of refs) {
    const ref = { ...input };
    if (ref.kind === 'branch') {
      const latest = states.get(ref.name);
      if (latest?.state === 'archived' && latest.target === ref.target) continue;
    }
    if (ref.kind === 'head' && ref.symbolic) {
      const branch = ref.symbolic.replace(/^refs\/heads\//, '');
      const latest = states.get(branch);
      const raw = branches.get(branch);
      if (latest?.state === 'archived' && (!raw || raw.target === latest.target)) {
        ref.symbolic = '';
        ref.target = latest.target;
      }
    }
    out.push(ref);
  }
  return out;
}

export function archivedBranchMarkers(refs: Ref[]): { branch: string; target: string }[] {
  const projected = projectBranchRefs(refs);
  const active = new Set(projected.filter((ref) => ref.kind === 'branch').map((ref) => ref.name));
  return [...branchLifecycleStates(refs).values()]
    .filter((event) => event.state === 'archived' && !active.has(event.branch))
    .map((event) => ({ branch: event.branch, target: event.target }))
    .sort((left, right) => left.branch.localeCompare(right.branch));
}
