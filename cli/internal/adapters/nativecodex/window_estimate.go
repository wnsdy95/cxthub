package nativecodex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// WindowPolicy exposes invocation-local packing evidence. Callers must retain
// the concrete type: only WindowBinding binds native's static catalog policy;
// WindowEstimate is unverified catalog evidence for a separate reserve policy.
type WindowPolicy interface {
	ModelWindow() ModelWindow
	RuntimeScope() string
	Validate(context.Context, Thread) error
}

var (
	_ WindowPolicy = WindowBinding{}
	_ WindowPolicy = WindowEstimate{}
)

// WindowEstimate is a read-only observation of the actual native home's model
// cache and effective configuration for an acknowledged thread. It is NEVER a
// WindowBinding: native can retain, merge, refresh or ignore this cache. Its
// opaque auth identity is fingerprinted, not independently authenticated; no
// credentials are read. Neither runtime adoption nor entitlement is attested.
// Only StartWindowEstimated or StartWindowPolicy creates a usable estimate.
type WindowEstimate struct {
	session        *Session
	thread         Thread
	window         ModelWindow
	configHash     string
	catalogHash    string
	catalogPath    string
	catalogVersion string
	observedAt     string
	scope          string
}

func (WindowEstimate) String() string             { return "native Codex window estimate (private configuration)" }
func (e WindowEstimate) GoString() string         { return e.String() }
func (e WindowEstimate) ModelWindow() ModelWindow { return e.window }
func (e WindowEstimate) RuntimeScope() string     { return e.scope }
func (WindowEstimate) Source() string             { return "native_model_cache" }

// ObservedAt is the catalog's fetched_at timestamp, not the time CXT read it.
func (e WindowEstimate) ObservedAt() string { return e.observedAt }

// CatalogHash fingerprints the selected entry and opaque cache identity.
// Compatible writers, timestamp/ETag refreshes and other models do not invalidate it.
func (e WindowEstimate) CatalogHash() string { return e.catalogHash }

// CatalogClientVersion identifies the cache writer, independently of the pinned
// executing host. A reviewed foreign writer is still only estimate evidence.
func (e WindowEstimate) CatalogClientVersion() string { return e.catalogVersion }
func (WindowEstimate) Estimated() bool                { return true }
func (WindowEstimate) Evidence() string {
	return "native model cache estimate; runtime adoption and provider acceptance unverified"
}

// StartWindowPolicy dispatches from acknowledged effective config, never from
// a failed static binding. An explicit but invalid static catalog stays an error.
// The returned ModelWindow does not apply CXT's 80% input/reserve policy.
func StartWindowPolicy(ctx context.Context, opts Options, threadOpts ThreadOptions) (*Session, Thread, WindowPolicy, error) {
	opts, config, hash, err := discoverWindowConfig(ctx, opts, threadOpts)
	if err != nil {
		return nil, Thread{}, nil, err
	}
	if dynamicWindowCatalog(config) {
		s, thread, estimate, err := startWindowEstimated(ctx, opts, threadOpts, hash)
		if err != nil {
			return nil, Thread{}, nil, err
		}
		return s, thread, estimate, nil
	}
	s, thread, binding, err := startWindowBound(ctx, opts, threadOpts, config, hash)
	if err != nil {
		return nil, Thread{}, nil, err
	}
	return s, thread, binding, nil
}

// StartWindowEstimated requires a dynamic catalog and exact native-acknowledged
// model/provider. Missing, malformed, stale or nonmatching evidence is unknown
// (an error and zero estimate), never a bundled/default model or invented window.
// Startup issues no inference or injection and writes no config/auth/catalog.
// The stock process itself can perform its normal catalog refresh at startup.
func StartWindowEstimated(ctx context.Context, opts Options, threadOpts ThreadOptions) (*Session, Thread, WindowEstimate, error) {
	opts, config, hash, err := discoverWindowConfig(ctx, opts, threadOpts)
	if err != nil {
		return nil, Thread{}, WindowEstimate{}, err
	}
	if !dynamicWindowCatalog(config) {
		return nil, Thread{}, WindowEstimate{}, windowEstimateError("effective catalog is static")
	}
	return startWindowEstimated(ctx, opts, threadOpts, hash)
}

func discoverWindowConfig(ctx context.Context, opts Options, threadOpts ThreadOptions) (Options, map[string]json.RawMessage, string, error) {
	opts.ConfigArgs = append([]string(nil), opts.ConfigArgs...)
	if opts.Env != nil {
		opts.Env = append([]string{}, opts.Env...)
	} else {
		opts.Env = os.Environ()
	}
	if threadOpts.ModelProvider != "" && threadOpts.ModelProvider != "openai" {
		return opts, nil, "", windowBindingError("thread provider catalog semantics unsupported")
	}
	discovery, err := Start(ctx, opts)
	if err != nil {
		return opts, nil, "", err
	}
	if !SupportedHostIdentity(discovery.HostIdentity()) {
		_ = discovery.Close()
		return opts, nil, "", windowBindingError("unsupported native version")
	}
	config, hash, err := discovery.effectiveWindowConfig(ctx)
	closeErr := discovery.Close()
	if err != nil {
		return opts, nil, "", err
	}
	if closeErr != nil {
		return opts, nil, "", closeErr
	}
	if err = windowStartupProvider(config); err != nil {
		return opts, nil, "", err
	}
	return opts, config, hash, nil
}

func dynamicWindowCatalog(config map[string]json.RawMessage) bool {
	raw, present := config["model_catalog_json"]
	return !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func startWindowEstimated(ctx context.Context, opts Options, threadOpts ThreadOptions, configHash string) (*Session, Thread, WindowEstimate, error) {
	empty := WindowEstimate{}
	s, err := Start(ctx, opts)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	success := false
	defer func() {
		if !success {
			_ = s.Close()
		}
	}()
	if !SupportedHostIdentity(s.HostIdentity()) {
		return nil, Thread{}, empty, windowEstimateError("unsupported native version")
	}
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err = s.enter(ctx); err != nil {
		return nil, Thread{}, empty, err
	}
	defer func() { <-s.gate }()
	config, startedHash, err := s.effectiveWindowConfig(ctx)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	if startedHash != configHash || !dynamicWindowCatalog(config) {
		return nil, Thread{}, empty, windowEstimateError("configuration changed during startup")
	}
	path, err := windowEstimateCachePath(opts.Env, s.cwd)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	thread, err := s.startThread(ctx, threadOpts)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	if thread.ModelProvider != "openai" {
		return nil, Thread{}, empty, windowEstimateError("provider catalog semantics unsupported")
	}
	// thread/start may refresh the cache. Observe it afterward; do not pretend
	// a disk snapshot proves which metadata the manager retained for this thread.
	catalog, hash, observedAt, err := readWindowEstimateCache(path, thread, time.Now())
	if err != nil {
		return nil, Thread{}, empty, err
	}
	window, err := resolveBoundModelWindow(thread, catalog, config)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	e := WindowEstimate{session: s, thread: thread, window: window, configHash: configHash, catalogHash: hash, catalogPath: path, observedAt: observedAt}
	// The cache reader has already validated this required field and its shape.
	cacheFields, _ := rpcObject(catalog)
	_ = json.Unmarshal(cacheFields["client_version"], &e.catalogVersion)
	// The private random socket directory distinguishes invocations even if a
	// host reuses a thread ID. Only its fingerprint escapes the adapter.
	scope, _ := json.Marshal(struct {
		Evidence, Host, Invocation string
		Thread                     Thread
		Config, Catalog            string
		Window                     ModelWindow
	}{e.Source(), s.host, s.socketPath, thread, configHash, hash, window})
	e.scope = windowHash(scope)
	if err = e.validateLocked(ctx); err != nil {
		return nil, Thread{}, empty, err
	}
	success = true
	return s, thread, e, nil
}

// Validate rechecks liveness, thread identity, effective config, exact cache
// selected entry/provenance and freshness immediately before injection. It detects observed drift,
// not atomic filesystem isolation or unexposed in-memory/auth changes in native.
func (e WindowEstimate) Validate(ctx context.Context, thread Thread) error {
	if e.session == nil || thread != e.thread || e.scope == "" {
		return windowEstimateError("unbound runtime")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.session.enter(ctx); err != nil {
		return err
	}
	defer func() { <-e.session.gate }()
	return e.validateLocked(ctx)
}

func (e WindowEstimate) validateLocked(ctx context.Context) error {
	s := e.session
	if err := s.active(); err != nil {
		return err
	}
	if s.thread != e.thread || !s.started || e.scope == "" {
		return windowEstimateError("runtime changed")
	}
	config, hash, err := s.effectiveWindowConfig(ctx)
	if err != nil {
		return err
	}
	if !dynamicWindowCatalog(config) || hash != e.configHash {
		return windowEstimateError("configuration changed")
	}
	_, catalogHash, _, err := readWindowEstimateCache(e.catalogPath, e.thread, time.Now())
	if err != nil {
		return err
	}
	if catalogHash != e.catalogHash {
		return windowEstimateError("catalog changed")
	}
	return ctx.Err()
}

func windowEstimateError(reason string) error {
	return fmt.Errorf("%w: native catalog estimate: %s", ErrModelWindow, reason)
}

// Source: openai/codex 36650394c5b38c2990ccf2a3457165ca3e9d9726,
// models-manager/src/{cache,manager,lib}.rs. FileModelsCache uses fetched_at,
// whole client_version, opaque identity and models, with a 300-second TTL.
// The opaque identity cannot be matched to live auth through stock config/read;
// requiring and hashing it prevents unscoped evidence and detects disk drift.
// rust-v0.162.0 (c1382380de69521303b416720a52f42d51af6248),
// models-manager/src/cache.rs and protocol/src/openai_models.rs
// retain this envelope and the resolver's window-field semantics. Accept that
// reviewed desktop writer as an estimate even on a 0.157.1 runtime; native's own
// exact-version adoption check is deliberately NOT claimed here. Other versions
// remain unknown until reviewed. Compatible writer changes preserve identical evidence.
const windowEstimateCacheTTL = 5 * time.Minute

func readWindowEstimateCache(path string, thread Thread, now time.Time) ([]byte, string, string, error) {
	raw, _, err := readWindowCatalog(path)
	if err != nil {
		return nil, "", "", windowEstimateError("catalog unavailable")
	}
	root, ok := windowObject(raw, "fetched_at", "client_version", "identity", "etag", "models")
	if !ok {
		return nil, "", "", windowEstimateError("invalid cache envelope")
	}
	// Reject duplicate keys at every depth rather than accepting a JSON parser's
	// last-wins interpretation, including within model metadata extensions.
	if _, err := windowConfigValue(raw, 0); err != nil {
		return nil, "", "", windowEstimateError("invalid cache content")
	}
	var version, identity, timestamp string
	if json.Unmarshal(root["client_version"], &version) != nil || (version != ModelWindowNativeVersion && version != "0.162.0") ||
		json.Unmarshal(root["identity"], &identity) != nil || !windowIdentity(identity) ||
		json.Unmarshal(root["fetched_at"], &timestamp) != nil {
		return nil, "", "", windowEstimateError("cache metadata unavailable or mismatched")
	}
	if etag, present := root["etag"]; present && !bytes.Equal(bytes.TrimSpace(etag), []byte("null")) {
		var value string
		if json.Unmarshal(etag, &value) != nil {
			return nil, "", "", windowEstimateError("invalid cache metadata")
		}
	}
	fetched, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || fetched.After(now) || now.Sub(fetched) > windowEstimateCacheTTL {
		return nil, "", "", windowEstimateError("cache timestamp invalid or expired")
	}
	selected, err := windowCatalogModel(thread, raw)
	if err != nil {
		return nil, "", "", windowEstimateError("exact cache model unavailable or ambiguous")
	}
	model, err := windowConfigValue(selected, 0)
	if err != nil {
		return nil, "", "", windowEstimateError("invalid cache model")
	}
	// Reviewed writers have the same selected-model semantics. Preserve the
	// original writer/timestamp as observation metadata, but do not invalidate
	// an idle CLI when a desktop refresh rewrites identical model evidence.
	evidence, err := json.Marshal(struct {
		Source, Identity string
		Model            any
	}{"native_model_cache", identity, model})
	if err != nil {
		return nil, "", "", windowEstimateError("invalid cache evidence")
	}
	return raw, windowHash(evidence), fetched.UTC().Format(time.RFC3339Nano), nil
}

// Follow utils/home-dir/src/lib.rs at the same pinned commit using the frozen
// child environment, never this adapter's ambient CODEX_HOME. Explicit relative
// paths resolve against native's canonical cwd. Missing HOME is unknown rather
// than guessing a different user's fallback directory. No directories are made.
func windowEstimateCachePath(env []string, cwd string) (string, error) {
	var codexHome, userHome string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			switch key {
			case "CODEX_HOME":
				codexHome = value
			case "HOME":
				userHome = value
			}
		}
	}
	if !utf8.ValidString(codexHome) || !utf8.ValidString(userHome) {
		return "", windowEstimateError("native home unavailable")
	}
	if codexHome == "" {
		if !filepath.IsAbs(userHome) {
			return "", windowEstimateError("native home unavailable")
		}
		codexHome = filepath.Join(userHome, ".codex")
	} else if !filepath.IsAbs(codexHome) {
		if !filepath.IsAbs(cwd) {
			return "", windowEstimateError("native home unavailable")
		}
		// Do not clean away a symlink/.. component before canonicalization;
		// native resolves it through the filesystem, not lexically.
		codexHome = cwd + string(os.PathSeparator) + codexHome
	}
	resolved, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		return "", windowEstimateError("native home unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", windowEstimateError("native home unavailable")
	}
	return filepath.Join(resolved, "models_cache.json"), nil
}
