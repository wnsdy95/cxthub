package nativecodex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// WindowBinding is an invocation-local binding to native's configured packing
// policy, not an account entitlement or a claim that inference will accept it.
// Only StartWindowBound or StartWindowPolicy can create a usable binding. Private config/catalog
// contents and paths are never returned, serialized, or included in errors.
type WindowBinding struct {
	session     *Session
	thread      Thread
	window      ModelWindow
	configHash  string
	catalogHash string
	catalogPath string
	scope       string
}

func (WindowBinding) String() string             { return "native Codex window binding (private configuration)" }
func (b WindowBinding) GoString() string         { return b.String() }
func (b WindowBinding) ModelWindow() ModelWindow { return b.window }
func (b WindowBinding) RuntimeScope() string     { return b.scope }

// StartWindowBound uses a discovery process to read native's effective static
// catalog selection. It then starts a fresh execution process with unchanged
// options/environment. Config and catalog bytes are captured before execution
// startup: the native manager retains its startup catalog even if config/read
// later sees a changed file. No discovery thread or model turn is created.
//
// Refreshable catalogs need an owned-runtime descriptor API; debug/cache reads
// cannot prove their binding. Revalidation detects observed drift, not atomic
// filesystem isolation (including an ABA edit entirely between observations).
func StartWindowBound(ctx context.Context, opts Options, threadOpts ThreadOptions) (*Session, Thread, WindowBinding, error) {
	opts, config, configHash, err := discoverWindowConfig(ctx, opts, threadOpts)
	if err != nil {
		return nil, Thread{}, WindowBinding{}, err
	}
	return startWindowBound(ctx, opts, threadOpts, config, configHash)
}

func startWindowBound(ctx context.Context, opts Options, threadOpts ThreadOptions, config map[string]json.RawMessage, configHash string) (*Session, Thread, WindowBinding, error) {
	empty := WindowBinding{}
	if dynamicWindowCatalog(config) {
		return nil, Thread{}, empty, windowBindingError("active dynamic catalog is not exposed by this native version")
	}
	var path string
	if json.Unmarshal(config["model_catalog_json"], &path) != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return nil, Thread{}, empty, windowBindingError("static catalog requires an absolute path")
	}
	catalog, hash, err := readWindowCatalog(path)
	if err != nil {
		return nil, Thread{}, empty, err
	}
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
		return nil, Thread{}, empty, windowBindingError("unsupported native version")
	}
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err = s.enter(ctx); err != nil {
		return nil, Thread{}, empty, err
	}
	defer func() { <-s.gate }()
	before, startedConfigHash, err := s.windowConfig(ctx, path)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	if startedConfigHash != configHash {
		return nil, Thread{}, empty, windowBindingError("configuration changed during startup")
	}
	thread, err := s.startThread(ctx, threadOpts)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	// This versioned source proof covers the native OpenAI-compatible manager.
	// Other provider implementations can ignore static catalogs entirely.
	if thread.ModelProvider != "openai" {
		return nil, Thread{}, empty, windowBindingError("provider catalog semantics unsupported")
	}
	window, err := resolveBoundModelWindow(thread, catalog, before)
	if err != nil {
		return nil, Thread{}, empty, err
	}
	b := WindowBinding{session: s, thread: thread, window: window, configHash: configHash, catalogHash: hash, catalogPath: path}
	// Thread identity deliberately partitions feedback by invocation. Native
	// credentials never need to be copied or independently fingerprinted by CXT.
	scope, _ := json.Marshal(struct {
		Host            string
		Thread          Thread
		Config, Catalog string
		Window          ModelWindow
	}{s.host, thread, configHash, hash, window})
	b.scope = windowHash(scope)
	if err = b.validateLocked(ctx); err != nil {
		return nil, Thread{}, empty, err
	}
	success = true
	return s, thread, b, nil
}

func (b WindowBinding) Validate(ctx context.Context, thread Thread) error {
	if b.session == nil || thread != b.thread || b.scope == "" {
		return windowBindingError("unbound runtime")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.session.enter(ctx); err != nil {
		return err
	}
	defer func() { <-b.session.gate }()
	return b.validateLocked(ctx)
}
func (b WindowBinding) validateLocked(ctx context.Context) error {
	s := b.session
	if err := s.active(); err != nil {
		return err
	}
	if s.thread != b.thread || !s.started || b.scope == "" {
		return windowBindingError("runtime changed")
	}
	_, configHash, err := s.windowConfig(ctx, b.catalogPath)
	if err != nil {
		return err
	}
	_, catalogHash, err := readWindowCatalog(b.catalogPath)
	if err != nil {
		return err
	}
	if configHash != b.configHash || catalogHash != b.catalogHash {
		return windowBindingError("configuration changed")
	}
	return ctx.Err()
}

func windowBindingError(reason string) error {
	return fmt.Errorf("%w: native window binding: %s", ErrState, reason)
}
func windowHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:])
}

func readWindowCatalog(path string) ([]byte, string, error) {
	f, err := openWindowCatalog(path)
	if err != nil {
		return nil, "", windowBindingError("catalog unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMessageBytes {
		return nil, "", windowBindingError("invalid catalog file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxMessageBytes+1))
	if err != nil || len(raw) > maxMessageBytes || len(raw) == 0 {
		return nil, "", windowBindingError("invalid catalog content")
	}
	return raw, windowHash(raw), nil
}

func (s *Session) windowConfig(ctx context.Context, path string) (map[string]json.RawMessage, string, error) {
	config, hash, err := s.effectiveWindowConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	if dynamicWindowCatalog(config) {
		return nil, "", windowBindingError("active dynamic catalog is not exposed by this native version")
	}
	if path != "" && !stringEquals(config["model_catalog_json"], path) {
		return nil, "", windowBindingError("effective catalog mismatch")
	}
	return config, hash, nil
}

func (s *Session) effectiveWindowConfig(ctx context.Context) (map[string]json.RawMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.rpc.call(ctx, "config/read", map[string]any{"cwd": s.cwd, "includeLayers": false})
	if err != nil {
		return nil, "", err
	}
	root, ok := identityObject(raw, "config")
	config, valid := windowObject(root["config"], "model_catalog_json", "model_provider")
	if !ok || !valid {
		return nil, "", windowBindingError("effective catalog mismatch")
	}
	// Fingerprint a detached, version-normalized copy. The resolver receives the
	// original effective configuration, including all TUI and unknown fields.
	hash, err := windowConfigFingerprint(root["config"])
	if err != nil {
		return nil, "", err
	}
	return config, hash, nil
}

func resolveBoundModelWindow(thread Thread, catalog []byte, config map[string]json.RawMessage) (ModelWindow, error) {
	selected, err := windowCatalogModel(thread, catalog)
	if err != nil {
		return ModelWindow{}, err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return ModelWindow{}, windowBindingError("invalid configuration")
	}
	return ResolveModelWindow(thread, thread.ModelProvider, selected, raw)
}

func windowCatalogModel(thread Thread, catalog []byte) (json.RawMessage, error) {
	root, ok := identityObject(catalog, "models")
	var models []json.RawMessage
	if !ok || json.Unmarshal(root["models"], &models) != nil || len(models) == 0 || len(models) > 10000 {
		return nil, windowBindingError("invalid catalog models")
	}
	seen := map[string]bool{}
	var selected json.RawMessage
	for _, raw := range models {
		item, ok := identityObject(raw, "slug")
		var slug string
		if !ok || json.Unmarshal(item["slug"], &slug) != nil || !windowIdentity(slug) || seen[slug] {
			return nil, windowBindingError("ambiguous catalog model")
		}
		seen[slug] = true
		if slug == thread.Model {
			selected = raw
		}
	}
	if selected == nil {
		return nil, windowBindingError("exact catalog model unavailable")
	}
	return selected, nil
}

// The shared manager is constructed from startup config. An explicit thread
// provider override cannot substitute a different manager's catalog semantics.
func windowStartupProvider(config map[string]json.RawMessage) error {
	for key := range config {
		if key != "model_provider" && strings.EqualFold(key, "model_provider") {
			return windowBindingError("ambiguous startup provider")
		}
	}
	provider := "openai" // pinned native 0.157.1 default
	if raw, present := config["model_provider"]; present && string(bytes.TrimSpace(raw)) != "null" {
		if json.Unmarshal(raw, &provider) != nil {
			return windowBindingError("invalid startup provider")
		}
	}
	if provider != "openai" {
		return windowBindingError("startup provider catalog semantics unsupported")
	}
	return nil
}
