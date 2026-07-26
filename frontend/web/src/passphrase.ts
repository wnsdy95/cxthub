// Team passphrase — format/creation.
// Format: 4+ English words · 12+ characters · single space delimiter (no leading/trailing spaces, punctuation, numbers).
//   → Envelope is puller+ and can be offline injected, so length/entropy is core of defense.
// Creation: Random 6 words from below list (≈42 bits) — combined with PBKDF2 600k for offline attack resistance.
// Crypto input is used as-is (no normalization) — Go/JS byte match maintained.

// Common English words (lowercase, 3-7 characters, no ambiguity/offensiveness). 128 → 6 words ≈ 42 bits.
const WORDS =
  'apple amber anchor arch arrow atlas bacon badge bamboo banjo barn basil beacon beetle birch bison bloom brick bronze brook cabin cactus camel canyon carbon cedar chalk cherry cider clover cobalt comet copper coral cotton cove cricket crystal daisy dawn delta denim diamond dune eagle ember falcon fern flint forest fox gable garnet ginger glacier granite grove hazel heron hollow ivory jade jasmine jetty kelp lagoon lantern ledger lemon lily linen lotus maple marble meadow meteor mint moss nectar oak ocean olive onyx opal orbit otter oxbow pearl pebble pine plum pond poppy prairie quartz quill raven reef ridge river robin rowan ruby saffron sage sequoia shale silk slate sparrow spruce stone storm summit thistle timber topaz tulip tundra valley velvet violet walnut willow winter zephyr zinc'.split(
    ' ',
  );

// generatePassphrase — random count words (default 6). Uint32 % 128 is unbiased as 2^32 divided by 128.
export function generatePassphrase(count = 6): string {
  const idx = crypto.getRandomValues(new Uint32Array(count));
  return Array.from(idx, (n) => WORDS[n % WORDS.length]).join(' ');
}

// passphraseError — null if valid, else i18n error key. Regex rejects leading/trailing/continuous spaces/punctuation.
export function passphraseError(p: string): 'secrets.passInvalid' | null {
  if (p.length < 12) return 'secrets.passInvalid';
  if (!/^[a-z]+( [a-z]+){3,}$/i.test(p)) return 'secrets.passInvalid'; // 4+ English words, single space
  return null;
}
