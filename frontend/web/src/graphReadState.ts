import { validateGraphState } from './graphState';
import type { GraphProjectionStatus, GraphReadGeneration, GraphState, PendingView, RepositoryRevision, RepositoryView } from './types';

function sourceEpoch(value: unknown): { incarnation: string; sequence: bigint } | null {
  if (typeof value !== 'string' || value.length > 56 ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:(0|[1-9][0-9]*)$/.test(value) ||
      value.trim() !== value) return null;
  const sequence = BigInt(value.slice(37));
  return sequence <= 9223372036854775807n ? { incarnation: value.slice(0, 36), sequence } : null;
}

function hash(value: unknown): value is string {
  return typeof value === 'string' && value.length === 71 && /^sha256:[0-9a-f]{64}$/.test(value);
}

export function parseGraphProjection(value: unknown): GraphProjectionStatus | null {
  const status = value as GraphProjectionStatus | null;
  if (!status || typeof status !== 'object' || Array.isArray(status) || status.rules !== 2 ||
      !['disabled', 'needs_attention', 'catching_up', 'ready'].includes(status.state) ||
      typeof status.complete !== 'boolean' || typeof status.current !== 'boolean') return null;
  const source = sourceEpoch(status.source_epoch), applied = sourceEpoch(status.applied_epoch);
  if (!source || (status.applied_epoch !== undefined && (!applied || (source.incarnation === applied.incarnation && applied.sequence > source.sequence)))) return null;
  if (status.complete) {
    if (!applied || source.incarnation !== applied.incarnation || !hash(status.generation) || status.current !== (status.source_epoch === status.applied_epoch)) return null;
  } else if (status.current || 'generation' in status) return null;
  if (status.state === 'ready' && (!status.complete || !status.current)) return null;
  return status;
}

export function parseGraphReadGeneration(value: unknown): GraphReadGeneration | null {
  const generation = value as GraphReadGeneration | null;
  if (!generation || typeof generation !== 'object' || Array.isArray(generation) || generation.rules !== 2 ||
      generation.complete !== true || typeof generation.current !== 'boolean' || !hash(generation.generation)) return null;
  const source = sourceEpoch(generation.source_epoch), applied = sourceEpoch(generation.applied_epoch);
  return source && applied && source.incarnation === applied.incarnation && applied.sequence <= source.sequence &&
    generation.current === (generation.source_epoch === generation.applied_epoch) ? generation : null;
}

export function sameGraphGeneration(current: GraphReadGeneration | undefined, wanted: GraphReadGeneration | undefined): boolean {
  return current?.generation === wanted?.generation && current?.applied_epoch === wanted?.applied_epoch &&
    current?.rules === wanted?.rules && current?.complete === wanted?.complete;
}

export function validateGraphPosition(graph: GraphState): GraphState {
  validateGraphState(graph, graph.revision);
  if (graph.read_generation !== undefined && !parseGraphReadGeneration(graph.read_generation)) {
    throw new Error('Invalid historical graph read generation');
  }
  return graph;
}

export function graphPositionMatches(graph: GraphState | undefined, revision: RepositoryRevision | undefined, generation?: GraphReadGeneration): boolean {
  if (!graph || !revision) return false;
  try {
    validateGraphPosition(graph);
    validateGraphState(graph, revision);
    return (!generation || Boolean(parseGraphReadGeneration(generation))) && sameGraphGeneration(graph.read_generation, generation);
  } catch { return false; }
}

export function graphRefreshTarget(status: GraphProjectionStatus | undefined): string | undefined {
  if (!status || status.state === 'disabled') return undefined;
  return `${status.source_epoch.slice(0, 36)}:${status.rules}:${status.complete ? `${status.generation}:${status.applied_epoch}` : 'pending'}`;
}

export function graphGenerationMatches(view: RepositoryView | undefined, status: GraphProjectionStatus): boolean {
  if (!view) return false;
  try { validateGraphRead(view, status); } catch { return false; }
  const generation = parseGraphReadGeneration(view?.read_generation);
  return Boolean(generation && status.complete && generation.generation === status.generation &&
    generation.applied_epoch === status.applied_epoch && generation.rules === status.rules &&
    generation.source_epoch.slice(0, 36) === status.source_epoch.slice(0, 36));
}

export function validateGraphRead<View extends RepositoryView | PendingView>(view: View, status?: GraphProjectionStatus): View {
  validateGraphState(view.graph, view.revision);
  const enabled = status && status.state !== 'disabled';
  if (!view.read_generation && !view.graph.read_generation && !enabled) return view;
  const generation = parseGraphReadGeneration(view.read_generation), graph = parseGraphReadGeneration(view.graph.read_generation);
  if (!generation || !graph || !sameGraphGeneration(generation, graph) || generation.source_epoch !== graph.source_epoch ||
      generation.current !== graph.current) throw new Error('Inconsistent graph read generation');
  if (enabled) {
    const source = sourceEpoch(status.source_epoch), applied = sourceEpoch(generation.applied_epoch)!;
    if (!parseGraphProjection(status) || !source || source.incarnation !== applied.incarnation || applied.sequence > source.sequence ||
        (generation.generation === status.generation && generation.applied_epoch !== status.applied_epoch) ||
        (generation.applied_epoch === status.applied_epoch && generation.generation !== status.generation && status.complete)) {
      throw new Error('Graph read generation does not match the published source');
    }
  }
  return view;
}

export function graphReadState(view: RepositoryView | undefined, status?: GraphProjectionStatus, error?: unknown, noticeError?: unknown) {
  const failure = error && typeof error === 'object' ? error as { status?: number; code?: string } : undefined;
  if (noticeError || (failure?.status && [401, 403].includes(failure.status))) return { state: 'denied', showGraph: false } as const;
  if (status?.state === 'disabled' || (!status && !view?.read_generation && !view?.graph.read_generation)) {
    return { state: 'legacy', showGraph: Boolean(view) } as const;
  }
  let showGraph = false;
  try { showGraph = Boolean(view && (!status || status.complete) && validateGraphRead(view, status)); } catch { showGraph = false; }
  if (status?.state === 'needs_attention' || failure?.code === 'graph_projection_failed') {
    return { state: 'failed', showGraph } as const;
  }
  if (!showGraph) return { state: 'pending', showGraph: false } as const;
  if (!status) return { state: view!.read_generation!.current ? 'ready' : 'lagging', showGraph } as const;
  return { state: status.state === 'ready' && status.current && graphGenerationMatches(view, status) ? 'ready' : 'lagging', showGraph } as const;
}
