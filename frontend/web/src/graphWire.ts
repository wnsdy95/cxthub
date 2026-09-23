import type { GraphState } from './types';

export const graphIndexFields = [
  'snapshot_ids', 'graph_ids', 'committed_ids', 'historical_ids', 'shared_ids',
  'pushed_ids', 'unpushed_ids', 'uncommitted_ids', 'tagged_ids', 'archived_only_ids',
  'ahead_ids', 'ahead_tips',
] as const;
type IndexedField = typeof graphIndexFields[number];
type GraphWireV1 = Omit<GraphState, IndexedField | 'branch_snapshots' | 'hold' | 'previous'> & {
  encoding: 'indexed-v1';
  dictionary: string[];
  branch_snapshots: Record<string, number[]>;
  hold: (Omit<GraphState['hold'][number], 'ids'> & {ids: number[]})[];
  previous: (Omit<GraphState['previous'][number], 'snapshot_ids' | 'collapsible_ids'> & {snapshot_ids: number[]; collapsible_ids: number[]})[];
} & Record<IndexedField, number[]>;

type Context = NonNullable<GraphState['branch_contexts']>[string];
type GraphWireV2 = Omit<GraphWireV1, 'encoding' | 'branch_contexts'> & {
  encoding: 'indexed-v2';
  branch_contexts: Record<string, Omit<Context, 'snapshot_ids' | 'roots'> & {roots: number[]; snapshot_ids?: number[]; snapshot_ids_ref?: string}>;
};
export type GraphWire = GraphWireV1 | GraphWireV2;

/** Transport decoding only: never derive membership or history from local data. */
export function decodeGraphState(input: GraphWire): GraphState {
  if (!input || !['indexed-v1', 'indexed-v2'].includes(input.encoding)) throw new Error('Unsupported graph encoding');
  const {dictionary, encoding: _encoding, ...rest} = input;
  if (!Array.isArray(dictionary) || dictionary.some(id => typeof id !== 'string') || new Set(dictionary).size !== dictionary.length) {
    throw new Error('Invalid graph dictionary');
  }
  const decode = (indices: number[]): string[] => {
    if (!Array.isArray(indices)) throw new Error('Missing graph indices');
    return indices.map(index => {
      if (!Number.isSafeInteger(index) || index < 0 || index >= dictionary.length) throw new Error('Invalid graph index');
      return dictionary[index];
    });
  };
  if (!input.branch_snapshots || typeof input.branch_snapshots !== 'object' || Array.isArray(input.branch_snapshots) || !Array.isArray(input.hold) || !Array.isArray(input.previous)) {
    throw new Error('Incomplete indexed graph');
  }
  const branches = Object.fromEntries(Object.entries(input.branch_snapshots).map(([branch, ids]) => [branch, decode(ids)]));
  let contexts: GraphState['branch_contexts'];
  if (input.encoding === 'indexed-v2') {
    if (!input.branch_contexts || typeof input.branch_contexts !== 'object' || Array.isArray(input.branch_contexts)) throw new Error('Incomplete branch contexts');
    contexts = Object.fromEntries(Object.entries(input.branch_contexts).map(([branch, c]) => {
      const {snapshot_ids_ref: ref, roots, snapshot_ids, ...context} = c;
      if (ref !== undefined && (typeof ref !== 'string' || !Object.hasOwn(branches, ref) || snapshot_ids !== undefined)) throw new Error('Invalid branch timeline reference');
      return [branch, {...context, roots: decode(roots), snapshot_ids: ref === undefined ? decode(snapshot_ids ?? []) : [...branches[ref]]}];
    }));
  } else contexts = input.branch_contexts;
  return {
    ...rest,
    branch_contexts: contexts,
    ...Object.fromEntries(graphIndexFields.map(key => [key, decode(input[key])])) as Record<IndexedField, string[]>,
    branch_snapshots: branches,
    hold: input.hold.map(h => ({...h, ids: decode(h.ids)})),
    previous: input.previous.map(p => ({...p, snapshot_ids: decode(p.snapshot_ids), collapsible_ids: decode(p.collapsible_ids)})),
  };
}
