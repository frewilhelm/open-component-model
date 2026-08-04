package workerpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	genericv1 "ocm.software/open-component-model/bindings/go/configuration/generic/v1/spec"
	"ocm.software/open-component-model/bindings/go/credentials"
	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	gpgcredentialsv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/credentials/v1alpha1"
	gpgsigningv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/signing/v1alpha1"
	"ocm.software/open-component-model/bindings/go/plugin/manager/registries/signinghandler"
	"ocm.software/open-component-model/bindings/go/repository"
	signingv1alpha1 "ocm.software/open-component-model/bindings/go/rsa/signing/v1alpha1"
	rsacredentialsv1 "ocm.software/open-component-model/bindings/go/rsa/spec/credentials/v1"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/bindings/go/signing"
	signingspec "ocm.software/open-component-model/bindings/go/signing/v1alpha1/spec"
	sigstoresigningv1alpha1 "ocm.software/open-component-model/bindings/go/sigstore/signing/v1alpha1"
	trustedrootv1alpha1 "ocm.software/open-component-model/bindings/go/sigstore/spec/credentials/trustedroot/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/verification"
)

// RequesterInfo contains information about the object requesting resolution.
type RequesterInfo struct {
	NamespacedName types.NamespacedName
}

// ErrNotSafelyDigestible is a sentinel error used to identify this error type.
var ErrNotSafelyDigestible = errors.New("not safely digestible")

// NotSafelyDigestibleError contains information about a component version that is not safely digestible.
type NotSafelyDigestibleError struct {
	Component string
	Version   string
	Err       error
}

func (e *NotSafelyDigestibleError) Error() string {
	return fmt.Sprintf("component version %s:%s is not safely digestible: %v", e.Component, e.Version, e.Err)
}

func (e *NotSafelyDigestibleError) Unwrap() error {
	return ErrNotSafelyDigestible
}

// NewNotSafelyDigestibleError creates a new NotSafelyDigestibleError with component and version information.
func NewNotSafelyDigestibleError(component, version string, err error) *NotSafelyDigestibleError {
	return &NotSafelyDigestibleError{
		Component: component,
		Version:   version,
		Err:       err,
	}
}

// ResolveOptions contains all the options the resolution service requires to perform a resolve operation.
type ResolveOptions struct {
	Component  string
	Version    string
	Repository repository.ComponentVersionRepository
	// Verifications are used to verify against component version signatures and used a cache key.
	Verifications []verification.Verification
	// Digest is used to verify the integrity of a referenced component version and is used as part of the cache key.
	Digest          *v2.Digest
	SigningRegistry *signinghandler.SigningRegistry
	// SigningConfig is the raw ocm signing config (signing.config.ocm.software) carried by the effective ocm
	// configuration. verifySignatures uses it to look up advisory verifiers for signatures present on the component
	// version but not listed in Verifications. Nil means no advisory verification runs.
	SigningConfig *genericv1.Config
	// CredentialGraph resolves typed credentials from the ocm credentials config. When a Verification
	// carries no inline key material the signature verifier's consumer identity is resolved through
	// this graph. Optional; nil means "no credential graph, use inline material or handler defaults".
	CredentialGraph credentials.Resolver
	KeyFunc         func() (string, error)
	// Requester is the information about the object requesting this resolution.
	// It will be notified when the resolution completes.
	Requester RequesterInfo
}

// Result contains the result of a resolution including any errors that might have occurred.
type Result struct {
	Value any
	Error error
}

// WorkItem represents a single work item to be processed by the worker pool.
type WorkItem struct {
	// Fn is the work function that is executed to process a work item.
	Fn workFunc
	// Opts contains the resolve options.
	Opts ResolveOptions
	// key is the calculated key that is passed in from the top to avoid
	// the error handling from the key function later.
	key string
}

// PoolOptions configures the worker pool.
type PoolOptions struct {
	// WorkerCount is the number of concurrent workers.
	WorkerCount int
	// QueueSize is the size of the work queue buffer.
	QueueSize int
	// SubscriberBufferSize is the buffer size for each subscriber's event channel.
	// A larger buffer reduces the probability of dropped events under load.
	SubscriberBufferSize int
	// Logger for the worker pool.
	Logger *logr.Logger
	// Client for Kubernetes API access.
	Client client.Reader
	// Cache for caching.
	Cache *expirable.LRU[string, *Result]
}

// WorkerPool manages a pool of workers that process work items concurrently.
type WorkerPool struct {
	PoolOptions
	workQueue     chan *WorkItem
	inProgressMu  sync.Mutex
	subscribersMu sync.RWMutex
	subscribers   []chan []RequesterInfo
	// tracks all requesters per resolution key to make sure that all objects who request this item will
	// be notified of any change.
	inProgress  map[string][]RequesterInfo
	workersDone sync.WaitGroup
}

// ErrResolutionInProgress is returned when a component version is being resolved in the background.
var ErrResolutionInProgress = fmt.Errorf("component version resolution in progress")

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(opts PoolOptions) *WorkerPool {
	if opts.WorkerCount <= 0 {
		opts.WorkerCount = 10
	}

	if opts.QueueSize <= 0 {
		opts.QueueSize = 1000
	}

	if opts.SubscriberBufferSize <= 0 {
		opts.SubscriberBufferSize = 100
	}

	return &WorkerPool{
		PoolOptions: opts,
		workQueue:   make(chan *WorkItem, opts.QueueSize),
		inProgress:  make(map[string][]RequesterInfo),
		subscribers: make([]chan []RequesterInfo, 0),
	}
}

// Subscribe creates a new event subscription channel and registers it to receive
// resolution events. Each subscriber gets its own buffered channel to avoid events being
// consumed by only one listener and controllers stealing events from other controllers.
// The channel is buffered to prevent blocking workers. If the buffer fills, events are dropped.
// The returned channel will be closed when the worker pool shuts down.
func (wp *WorkerPool) Subscribe() <-chan []RequesterInfo {
	wp.subscribersMu.Lock()
	defer wp.subscribersMu.Unlock()

	ch := make(chan []RequesterInfo, wp.SubscriberBufferSize)
	wp.subscribers = append(wp.subscribers, ch)
	return ch
}

// Start begins the worker pool.
// This method blocks until the context is canceled to implement graceful shutdown.
func (wp *WorkerPool) Start(ctx context.Context) error {
	wp.Logger.Info("starting worker pool", "workers", wp.WorkerCount, "queueSize", wp.QueueSize, "subscriberBufferSize", wp.SubscriberBufferSize)

	for i := range wp.WorkerCount {
		wp.workersDone.Add(1)
		go wp.worker(ctx, i)
	}

	// wait for context cancellation
	<-ctx.Done()
	wp.Logger.Info("worker pool shutting down, draining queue")

	// wait for all workers to finish
	done := make(chan struct{})
	go func() {
		wp.workersDone.Wait()

		// now it's safe to close the channels
		close(wp.workQueue)

		wp.subscribersMu.Lock()
		for _, ch := range wp.subscribers {
			close(ch)
		}
		wp.subscribersMu.Unlock()

		close(done)
	}()

	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	select {
	case <-done:
		wp.Logger.Info("worker pool shutdown complete")
		return nil
	case <-timeout.C:
		return fmt.Errorf("timed out waiting for worker pool to shutdown")
	}
}

// GetComponentVersion retrieves a component version using the worker pool and cache.
func (wp *WorkerPool) GetComponentVersion(ctx context.Context, opts ResolveOptions) (*descriptor.Descriptor, error) {
	return resolveWorkRequest[*descriptor.Descriptor](ctx, wp, opts, wp.getComponentVersion)
}

// resolveWorkRequest is an abstraction in front of the worker queue and resolution logic. It is meant to be called by
// small purpose functions, like the GetComponentVersion function above, that wish to use the worker-pool to cache results.
// For example, another function could be GetLocalResource that caches the blob object.
func resolveWorkRequest[T any](ctx context.Context, wp *WorkerPool, opts ResolveOptions, fn workFunc) (result T, _ error) {
	wp.inProgressMu.Lock()
	defer wp.inProgressMu.Unlock()

	key, err := opts.KeyFunc()
	if err != nil {
		return result, fmt.Errorf("failed to generate cache key: %w", err)
	}

	// Check cache BEFORE checking in-progress, otherwise we get into a scenario where
	// cache has been populated but in-progress has not yet been cleared and an error
	// is returned even though the value exists.
	// This is a slim chance, but not zero.
	// handleWorkItem -> Cache.Add
	// resolveWorkRequest -> locks InProgress so handleWorkItem cannot lock to delete the key
	// If it would check InProgress before we check the cache it would return the error even though the item
	// is already in the cache.
	// With this, it returns, releases in-progress mutex, defer in handleWorkItem continues and removes the
	// InProgress key.
	if cached, ok := wp.Cache.Get(key); ok {
		CacheHitCounterTotal.WithLabelValues(opts.Component, opts.Version, verificationState(opts.Verifications, opts.Digest)).Inc()
		// In case of an error of type ErrNotSafelyDigestible we return the cached error and value because we want
		// to pass through the information that this component version is not safely digestible to the controller
		// but still use the value.
		if errors.Is(cached.Error, ErrNotSafelyDigestible) {
			res, ok := cached.Value.(T)
			if !ok {
				return result, fmt.Errorf("unable to assert cache value for key %s into requested type, was: %T", key, cached.Value)
			}

			return res, cached.Error
		}

		if cached.Error != nil {
			// we remove error results from the cache, so the controller can immediately retry.
			wp.Cache.Remove(key)
			return result, cached.Error
		}

		res, ok := cached.Value.(T)
		if !ok {
			return result, fmt.Errorf("unable to assert cache value for key %s into requested type, was: %T", key, cached.Value)
		}

		return res, nil
	}

	CacheMissCounterTotal.WithLabelValues(opts.Component, opts.Version, verificationState(opts.Verifications, opts.Digest)).Inc()

	// check if already/still in progress
	if requesters, exists := wp.inProgress[key]; exists {
		// add this requester to the list if not already present (deduplicate)
		alreadyRequested := false
		for _, r := range requesters {
			if r.NamespacedName == opts.Requester.NamespacedName {
				alreadyRequested = true
				break
			}
		}
		if !alreadyRequested {
			wp.inProgress[key] = append(requesters, opts.Requester)
			wp.Logger.V(1).Info("resolution still in progress, added requester",
				"component", opts.Component,
				"version", opts.Version,
				"requester", opts.Requester.NamespacedName)
		} else {
			wp.Logger.V(1).Info("resolution still in progress, requester already tracked",
				"component", opts.Component,
				"version", opts.Version,
				"requester", opts.Requester.NamespacedName)
		}
		return result, ErrResolutionInProgress
	}

	// check for context cancellation before enqueuing
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	default:
	}

	workItem := &WorkItem{
		Fn:   fn,
		Opts: opts,
		key:  key,
	}

	select {
	case wp.workQueue <- workItem:
		// first requester
		wp.inProgress[key] = []RequesterInfo{opts.Requester}
		InProgressGauge.Set(float64(len(wp.inProgress)))
		QueueSizeGauge.Set(float64(len(wp.workQueue)))
		wp.Logger.V(1).Info("enqueued request", "component", opts.Component, "requester", opts.Requester.NamespacedName)

		return result, ErrResolutionInProgress
	default:
		if len(wp.workQueue) == wp.QueueSize {
			return result, fmt.Errorf("work queue is full; cannot resolve requests for %s", opts.Component)
		}

		return result, fmt.Errorf("cannot enqueue request for %s", opts.Component)
	}
}

// worker is the main worker loop that processes work items and updates the cache directly.
func (wp *WorkerPool) worker(ctx context.Context, id int) {
	defer wp.workersDone.Done()
	logger := wp.Logger.WithValues("worker", id)
	defer logger.V(1).Info("worker stopped")

	for {
		select {
		case <-ctx.Done():
			logger.V(1).Info("worker stopped due to context cancellation")
			return
		case item := <-wp.workQueue:
			QueueSizeGauge.Set(float64(len(wp.workQueue)))
			wp.handleWorkItem(ctx, &logger, item)
		}
	}
}

// workFunc is the signature for functions that process work items.
type workFunc func(ctx context.Context, item ResolveOptions) (any, error)

func (wp *WorkerPool) handleWorkItem(ctx context.Context, logger *logr.Logger, item *WorkItem) {
	logger.V(1).Info("processing work item", "key", item.key)

	start := time.Now()
	result, err := item.Fn(ctx, item.Opts)
	duration := time.Since(start).Seconds()

	// Track metrics
	ResolutionDurationHistogram.WithLabelValues(item.Opts.Component, item.Opts.Version, verificationState(item.Opts.Verifications, item.Opts.Digest)).Observe(duration)

	if err != nil {
		logger.Error(err, "failed to process work item",
			"component", item.Opts.Component,
			"version", item.Opts.Version,
			"duration", duration)
	} else {
		logger.V(1).Info("processed work item",
			"component", item.Opts.Component,
			"version", item.Opts.Version,
			"duration", duration)
	}

	// get all requesters AFTER resolution completes but BEFORE cleanup
	// ensures we capture all requesters that were added during the resolution and the wait for it to be finished
	requesters := wp.setResult(item.key, result, err)

	// notify all subscribers of an event happening.
	// Uses buffered channels with non-blocking send to avoid worker goroutine overhead.
	wp.subscribersMu.RLock()
	subscribers := slices.Clone(wp.subscribers)
	wp.subscribersMu.RUnlock()

	for _, ch := range subscribers {
		select {
		case <-ctx.Done():
			logger.V(1).Info("context canceled, skipping event broadcast",
				"component", item.Opts.Component,
				"version", item.Opts.Version)
			return
		case ch <- requesters:
			logger.V(1).Info("sent resolution event to subscriber",
				"component", item.Opts.Component,
				"version", item.Opts.Version,
				"requesterCount", len(requesters))
		default:
			logger.Info("dropped resolution event, subscriber buffer full",
				"component", item.Opts.Component,
				"version", item.Opts.Version)
			EventChannelDropsTotal.WithLabelValues(item.Opts.Component, item.Opts.Version, verificationState(item.Opts.Verifications, item.Opts.Digest)).Inc()
		}
	}
}

func (wp *WorkerPool) setResult(key string, result any, err error) []RequesterInfo {
	wp.inProgressMu.Lock()
	defer wp.inProgressMu.Unlock()

	wp.Cache.Add(key, &Result{
		Value: result,
		Error: err,
	})

	requesters := slices.Clone(wp.inProgress[key])
	delete(wp.inProgress, key)
	InProgressGauge.Set(float64(len(wp.inProgress)))
	return requesters
}

// getComponentVersion performs the actual component version resolution. If verifications or a digest from a component
// reference from a parent component are provided, it performs the necessary integrity and signature verification.
func (wp *WorkerPool) getComponentVersion(ctx context.Context, opts ResolveOptions) (any, error) {
	logger := log.FromContext(ctx)

	desc, err := opts.Repository.GetComponentVersion(ctx, opts.Component, opts.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to get component version %s:%s: %w", opts.Component, opts.Version, err)
	}

	if opts.Digest != nil && len(opts.Verifications) > 0 {
		return nil, fmt.Errorf(
			"invalid resolve options for %s:%s: digest and verifications are mutually exclusive",
			opts.Component, opts.Version,
		)
	}

	switch {
	case opts.Digest != nil:
		return compareDigest(ctx, desc, opts.Digest)
	case len(opts.Verifications) > 0:
		// If verifications are requested, we need to verify that the component version is safely digestible.
		// Anything that comes after this will, in case of an error, always be skipped until cache TTL expires
		if err := signing.IsSafelyDigestible(&desc.Component); err != nil {
			return desc, fmt.Errorf("%w: %w", ErrNotSafelyDigestible, err)
		}

		if opts.SigningRegistry == nil {
			return nil, fmt.Errorf("signing registry is required when verifications are configured")
		}

		return verifySignatures(ctx, desc, opts.Verifications, opts.SigningConfig, opts.CredentialGraph, opts.SigningRegistry)
	default:
		logger.Info("no digest or verifications provided, skipping integrity and signature verification",
			"component", opts.Component, "version", opts.Version)
		return desc, nil
	}
}

// verifySignatures runs signature verification in two passes.
//
// Pass 1 (enforcing): every entry in verifications is verified against the matching signature on the component
// version. A missing signature or a failed verification is a terminal error and blocks the resolution.
//
// Pass 2 (advisory): every signature on the component version that pass 1 did not already handle is looked up in the
// ocm signing config (signing.config.ocm.software). If an entry with a Verifier applies (per-signature or global),
// its handler is invoked. Failures are logged at Error level and swallowed: advisory verifiers exist to surface trust
// data configured via ocmconfig without turning every unrelated component into a Ready=false. Advisory verification is
// observability, not a security control - only entries in Component.spec.verify gate the deployment.
//
// Credential resolution order per signature:
//  1. Inline key material on the Verification (SecretRef / Value) - always wins when present.
//  2. Credentials returned by credGraph for the handler's consumer identity - used when no inline
//     material is configured and a credential graph is available.
//  3. Nil - passed through to the handler, which either accepts defaults (public-good Sigstore) or
//     fails with its own "missing credentials" error (RSA / GPG).
func verifySignatures(ctx context.Context, desc *descriptor.Descriptor, verifications []verification.Verification, signingConfig *genericv1.Config, credGraph credentials.Resolver, signingRegistry *signinghandler.SigningRegistry) (*descriptor.Descriptor, error) {
	logger := log.FromContext(ctx)
	logger.Info("verifying signature", "component", desc.Component.Name, "version", desc.Component.Version)

	enforced := make(map[string]struct{}, len(verifications))
	for _, v := range verifications {
		var descSig *descriptor.Signature
		for i := range desc.Signatures {
			if desc.Signatures[i].Name == v.Signature {
				descSig = &desc.Signatures[i]
				break
			}
		}

		if descSig == nil {
			return nil, fmt.Errorf("signature %s not found in component %s", v.Signature, desc.Component.Name)
		}
		enforced[descSig.Name] = struct{}{}

		if err := signing.VerifyDigestMatchesDescriptor(ctx, desc, *descSig, slog.New(logr.ToSlogHandler(logger))); err != nil {
			return nil, fmt.Errorf("digest verification failed for signature %q: %w", descSig.Name, err)
		}

		var verifierSpec runtime.Typed
		if v.VerifierSpec != nil {
			verifierSpec = v.VerifierSpec
		} else {
			verifierSpec = &signingv1alpha1.Config{}
		}

		signingHandler, err := signingRegistry.GetPlugin(ctx, verifierSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to get signing handler plugin for verifier %q: %w", verifierSpec.GetType(), err)
		}

		creds, err := resolveVerifierCredentials(ctx, signingHandler, verifierSpec, *descSig, v.PublicKey, credGraph, logger)
		if err != nil {
			return nil, fmt.Errorf("could not resolve credentials for signature %q: %w", descSig.Name, err)
		}

		if err := signingHandler.Verify(ctx, *descSig, verifierSpec, creds); err != nil {
			return nil, fmt.Errorf("signature verification failed for signature %s: %w", v.Signature, err)
		}

		logger.Info("verified signature", "component", desc.Component.Name, "version", desc.Component.Version, "signature", v.Signature)
	}

	if signingConfig == nil {
		return desc, nil
	}

	for i := range desc.Signatures {
		descSig := &desc.Signatures[i]
		if _, done := enforced[descSig.Name]; done {
			continue
		}
		cfg, err := signingspec.LookupConfigForSignature(signingConfig, descSig.Name)
		if err != nil {
			logger.Error(err, "advisory signature verification skipped: failed to look up signing config",
				"signature", descSig.Name, "component", desc.Component.Name)
			continue
		}
		if cfg == nil || cfg.Verifier == nil {
			continue
		}
		if err := runAdvisoryVerification(ctx, desc, *descSig, cfg.Verifier, credGraph, signingRegistry, logger); err != nil {
			logger.Error(err, "advisory signature verification failed; verifier sourced from ocmconfig, not enforced",
				"signature", descSig.Name, "component", desc.Component.Name, "verifier", cfg.Verifier.GetType())
		}

		logger.Info("verified advisory signature", "component", desc.Component.Name, "version", desc.Component.Version, "signature", descSig.Name)
	}

	return desc, nil
}

// runAdvisoryVerification runs a single advisory signature verification. Errors are returned so the caller can log them
// with structured fields; the caller must not propagate them as reconcile failures.
func runAdvisoryVerification(ctx context.Context, desc *descriptor.Descriptor, descSig descriptor.Signature, verifierSpec runtime.Typed, credGraph credentials.Resolver, signingRegistry *signinghandler.SigningRegistry, logger logr.Logger) error {
	if err := signing.VerifyDigestMatchesDescriptor(ctx, desc, descSig, slog.New(logr.ToSlogHandler(logger))); err != nil {
		return fmt.Errorf("digest verification failed: %w", err)
	}
	handler, err := signingRegistry.GetPlugin(ctx, verifierSpec)
	if err != nil {
		return fmt.Errorf("failed to get signing handler plugin for verifier %q: %w", verifierSpec.GetType(), err)
	}
	creds, err := resolveVerifierCredentials(ctx, handler, verifierSpec, descSig, nil, credGraph, logger)
	if err != nil {
		return fmt.Errorf("could not resolve credentials: %w", err)
	}
	return handler.Verify(ctx, descSig, verifierSpec, creds)
}

// resolveVerifierCredentials picks the credential material handed to the signing handler for a single signature. It
// prefers inline material declared on the Verification CR, falling back to the ocm credential graph when the handler
// exposes a consumer identity, and finally to nil (the handler decides).
func resolveVerifierCredentials(
	ctx context.Context,
	handler signing.Handler,
	verifierSpec runtime.Typed,
	sig descriptor.Signature,
	inlineKey []byte,
	credGraph credentials.Resolver,
	logger logr.Logger,
) (runtime.Typed, error) {
	if len(inlineKey) > 0 {
		return credentialsForVerifier(verifierSpec.GetType(), inlineKey)
	}

	if credGraph == nil {
		return nil, nil
	}

	consumerID, err := handler.GetVerifyingCredentialConsumerIdentity(ctx, sig, verifierSpec)
	if err != nil {
		// The handler is unable to describe an identity for this signature. Fall back to nil credentials
		// and let it use its defaults or fail with its own error.
		logger.V(1).Info("handler could not derive verifying credential consumer identity",
			"signature", sig.Name, "error", err.Error())
		return nil, nil
	}

	resolved, err := credGraph.Resolve(ctx, consumerID)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			logger.V(1).Info("no credentials found in credential graph for signature verification",
				"signature", sig.Name, "identity", consumerID)
			return nil, nil
		}
		return nil, fmt.Errorf("credential graph lookup failed: %w", err)
	}
	return resolved, nil
}

// credentialsForVerifier wraps the raw key material carried on the Verification CR into the typed credential expected
// by the resolved signing handler. The v1alpha1 Component API exposes a single opaque blob per signature, so the
// meaning of that blob is derived from the verifier config type:
//
//   - RSASigningConfiguration            -> PEM public key / certificate chain (required)
//   - GPGSigningConfiguration            -> ASCII-armored OpenPGP public key   (required)
//   - SigstoreVerificationConfiguration  -> optional trusted-root JSON, only
//     needed for privateInfrastructure: true; empty means "use the default
//     public-good Sigstore TUF root".
//
// A nil return is the explicit signal "no material was configured". Handlers that require material (RSA, GPG) will
// surface a descriptive error on Verify; handlers that tolerate absence (public-good Sigstore) will proceed with
// their defaults.
func credentialsForVerifier(verifierType runtime.Type, publicKey []byte) (runtime.Typed, error) {
	if len(publicKey) == 0 {
		return nil, nil
	}
	switch verifierType.Name {
	case signingv1alpha1.ConfigType:
		return &rsacredentialsv1.RSACredentials{
			Type:         rsacredentialsv1.VersionedType,
			PublicKeyPEM: string(publicKey),
		}, nil
	case gpgsigningv1alpha1.ConfigType:
		return &gpgcredentialsv1alpha1.GPGCredentials{
			Type:         runtime.NewVersionedType(gpgcredentialsv1alpha1.GPGCredentialsType, gpgcredentialsv1alpha1.Version),
			PublicKeyPGP: string(publicKey),
		}, nil
	case sigstoresigningv1alpha1.VerifyConfigType:
		return &trustedrootv1alpha1.TrustedRoot{
			Type:            trustedrootv1alpha1.VersionedType,
			TrustedRootJSON: string(publicKey),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported verifier configuration type %q", verifierType)
	}
}

// compareDigest performs integrity verification using the provided digest against a fresh calculated digest of
// the passed descriptor.
func compareDigest(ctx context.Context, desc *descriptor.Descriptor, digest *v2.Digest) (*descriptor.Descriptor, error) {
	logger := log.FromContext(ctx)

	logger.Info("verifying integrity with provided digest",
		"component", desc.Component.Name, "version", desc.Component.Version)

	digestDesc, err := signing.GenerateDigest(ctx, desc, slog.New(logr.ToSlogHandler(logger)),
		digest.NormalisationAlgorithm, digest.HashAlgorithm)
	if err != nil {
		return nil, fmt.Errorf("failed to generate digest for component version %s:%s: %w",
			desc.Component.Name, desc.Component.Version, err)
	}

	if digestDesc.Value != digest.Value {
		return nil, fmt.Errorf("digest mismatch (%s/%s) for component version %s:%s: expected %s, got %s",
			digest.NormalisationAlgorithm, digest.HashAlgorithm, desc.Component.Name, desc.Component.Version,
			digest.Value, digestDesc.Value)
	}

	return desc, nil
}

func verificationState(verifications []verification.Verification, digest *v2.Digest) string {
	hasVerifications := len(verifications) != 0
	hasDigest := digest != nil

	switch {
	case hasVerifications && hasDigest:
		return "unknown"
	case hasVerifications || hasDigest:
		return "verified"
	default:
		return "unverified"
	}
}
