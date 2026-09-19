export interface PreviousProgressGroup {
  key: string;
  branch: string;
  before: string;
  after: string;
  createdAt: string;
  snapshotIds: Set<string>;
  collapsibleIds: Set<string>;
}

/** Expanding either of two overlapping groups also reveals their shared path. */
export function hiddenProgressIds(
  groups: PreviousProgressGroup[], expandedKeys: ReadonlySet<string>,
): Set<string> {
  const hidden = new Set(groups.flatMap((group) => [...group.collapsibleIds]));
  for (const group of groups) {
    if (expandedKeys.has(group.key)) for (const id of group.collapsibleIds) hidden.delete(id);
  }
  return hidden;
}
