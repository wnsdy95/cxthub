// Public storage pricing contract. The unit price and Free/Pro allowance
// mirror GitHub Git LFS storage as verified on 2026-09-02. cxthub deliberately
// excludes bandwidth and seat pricing: stored context is the only meter.
export const STORAGE_PRICING = Object.freeze({
  includedGiB: 10,
  overageUsdPerGiBMonth: 0.07,
  verifiedAt: '2026-09-02',
  sourceDocs: 'https://docs.github.com/en/billing/concepts/product-billing/git-lfs',
  sourceCalculator: 'https://github.com/pricing/calculator',
});

export function normalizedAverageStorageGiB(value: number): number {
  return Number.isFinite(value) && value > 0 ? value : 0;
}

export function billableStorageGiB(averageStorageGiB: number): number {
  return Math.max(0, normalizedAverageStorageGiB(averageStorageGiB) - STORAGE_PRICING.includedGiB);
}

export function estimateMonthlyStorageUsd(averageStorageGiB: number): number {
  return billableStorageGiB(averageStorageGiB) * STORAGE_PRICING.overageUsdPerGiBMonth;
}
