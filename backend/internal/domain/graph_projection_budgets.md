# Graph projection output budgets

| Contract | Aggregate output bytes | Structural entries | Persisted parts |
| --- | ---: | ---: | ---: |
| `graph-v1`, rules 1 | 4 MiB | 20,000 | 20,000 |
| `graph-view-v2`, rules 2 | 32 MiB | 1,000,000 | 20,000 |
| On Hold / pending | 4 MiB | Existing pending accounting, unchanged | 20,000 |

Graph structural entries count every slice element and map entry recursively,
including repeated membership IDs. They are not unique snapshot counts or SQL
part counts. Header descriptor discovery retains the persisted-part bound;
the exact field manifest remains mandatory.

The v2 byte budget applies to pre-encoding measurement, canonical encoded parts
including keys and per-part overhead, and PostgreSQL JSONB textual storage size.
PostgreSQL formatting can make stored size larger than compact JSON. Passing
one measurement does not waive the others. Encoding, decoding, candidate
publication/read, qualification, serving promotion/read and manifest discovery
use the corresponding graph budget. Pending storage keeps its own defaults.

This changes admission capacity, not the v2 schema, canonical values, hashes or
qualification rules. Existing small v2 bundles retain identical encodings and
digests. Older binaries still reject v2 bundles above their previous bounds:
upgrade all candidate/qualification workers and readers before publishing or
enabling larger bundles. Graph-v1 and pending limits remain unchanged.

These are finite logical-output limits, not an RSS or latency guarantee. Decoded
objects, schema validation, canonical re-encoding, JSONB conversion and concurrent
requests require additional memory. Source/input limits, request deadlines and
resource admission are unchanged. Compression and graph windows are not added;
reads still load and decode the complete bundle. Production-size measurement and
qualified readiness remain prerequisites for rollout.
