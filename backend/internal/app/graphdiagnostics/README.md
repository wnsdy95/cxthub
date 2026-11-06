# Request-scoped graph diagnostics (stage 1)

This internal, standard-library-only application support package is shared by application
and adapter layers. It accepts fixed stage, route, and cache identifiers, never SQL, parameters,
errors, repository names, positions, URLs, authentication headers, or document bodies.
Storage is bounded by the identifier sets rather than graph size or query count.

HTTP `graphRead` creates a recorder when the default structured logger enables Info.
It emits one `graph_read` record using the trusted `inbound.Correlation` context ID
set by HTTP middleware, also returned as `X-Request-ID`. Direct calls without that
context receive a fresh server-generated ID; client headers/user IDs are never used. Disabling Info
leaves the writer and request unchanged. Application calls without a recorder do not
allocate spans, consult a clock, or lock. Non-graph SQL tracing is likewise a no-op.

Stage times are inclusive, overlap when nested, and accumulate across retries. SQL
counts/times belong to the innermost active context stage, plus request totals; they
must not be summed with enclosing elapsed times. QueryTracer measures pgx Query,
QueryRow, and Exec through completion/Rows.Close, including client consumption and
draining. It does not measure pool acquisition, SendBatch, or CopyFrom. Rows are
server command-tag counts for successful SELECT/INSERT/UPDATE/DELETE, not decoded
application rows. Failed, missing, or non-row command tags increment `rows_unknown`,
not a fictional zero-row result. An empty SQL total means no traced query completed,
not proof that no database work occurred. The separate process-admission pool clears
its tracer in existing initialization and is not observed.

Cache counters describe actual lookup events: a graph miss followed by a successful
post-admission recheck records both miss and hit. Ineligible means no reusable source
generation/pinned revision; unused caches are omitted. Route counters identify the
legacy/published implementation entered, including failed attempts, not routing policy.

`build_admission` times calls to AcquireBuild, including checks/reentrant acquisition;
it is not pure queue wait. Initial HTTP admission, authorization before graphRead,
pool wait, transaction setup/cleanup, and resource release have no separate spans.
Their time may be included in enclosing request/read spans. No separate encoding,
socket-write, browser-render, or network timings are claimed.

`Server-Timing` is frozen at the first final response header, without buffering the
body. `graph_pre` is entry-to-header elapsed; `graph_sql` and named stages aggregate
measurements available then. Open spans have no duration yet and are omitted from
the header. The final log can contain additional completion/release/response time.
No Timing-Allow-Origin or cross-origin exposure policy is changed.
