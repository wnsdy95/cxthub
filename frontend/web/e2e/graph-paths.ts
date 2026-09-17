import { expect, type Page } from '@playwright/test';

/** Follow actual SVG endpoints in screen coordinates, including divider rows.
 * Node counts or lane numbers alone cannot detect physical gaps in a path. */
export async function expectRenderedGraphPath(page: Page, from: string, to: string, lifecycle = false) {
  const connected = await page.locator('.graph').evaluate((graph, { from, to, lifecycle }) => {
    const key = (x: number, y: number) => `${x.toFixed(2)}:${y.toFixed(2)}`;
    const center = (id: string) => {
      const row = [...graph.querySelectorAll<HTMLElement>('[data-graph-id]')].find(r => r.dataset.graphId === id);
      const node = row?.querySelector<SVGGraphicsElement>('.branch-event-node, circle');
      if (!node) return '';
      const box = node.getBBox(), matrix = node.getScreenCTM();
      if (!matrix) return '';
      const p = new DOMPoint(box.x + box.width / 2, box.y + box.height / 2).matrixTransform(matrix);
      return key(p.x, p.y);
    };
    const edges = new Map<string, Array<{ target: string; lifecycle: boolean }>>();
    for (const segment of graph.querySelectorAll<SVGGeometryElement>('svg line, svg path')) {
      const matrix = segment.getScreenCTM();
      if (!matrix) continue;
      const p = segment.getPointAtLength(0).matrixTransform(matrix);
      const q = segment.getPointAtLength(segment.getTotalLength()).matrixTransform(matrix);
      const start = key(p.x, p.y), end = key(q.x, q.y);
      if (q.y <= p.y) continue;
      edges.set(start, [...(edges.get(start) ?? []), { target: end, lifecycle: segment.dataset.graphEdge === 'lifecycle' }]);
    }
    const start = center(from), end = center(to);
    if (!start || !end) return false;
    const stack = [{ target: start, lifecycle: false }], seen = new Set<string>();
    while (stack.length) {
      const p = stack.pop()!;
      if (p.target === end && (!lifecycle || p.lifecycle)) return true;
      const id = `${p.target}:${p.lifecycle}`;
      if (seen.has(id)) continue;
      seen.add(id);
      for (const edge of edges.get(p.target) ?? []) stack.push({ target: edge.target, lifecycle: p.lifecycle || edge.lifecycle });
    }
    return false;
  }, { from, to, lifecycle });
  expect(connected, `Rendered path ${from} → ${to}${lifecycle ? ' through a lifecycle edge' : ''}`).toBe(true);
}
