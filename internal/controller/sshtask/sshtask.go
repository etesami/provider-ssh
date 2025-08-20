package sshtask

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/feature"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	klog "sigs.k8s.io/controller-runtime/pkg/log"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	ssh "golang.org/x/crypto/ssh"

	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/statemetrics"

	apiv1a1 "github.com/etesami/provider-ssh/apis/v1alpha1"
	"github.com/etesami/provider-ssh/internal/features"
)

const (
	errNotSSHTask   = "managed resource is not a SSHTask custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNoSSHConn    = "no SSH connection available"
	errCreateConn   = "cannot create new SSH connection"
	errNotReachable  = "cannot reach to remote resource"
	errGetConnData  = "cannot get connection data"
	errExtractCreds  = "cannot extract credentials"

	errNoProbeScript  = "no probeScript provided"
	errNoEnsureScript = "no ensureScript provided"

	errRunEnsureScript = "error on running ensure script"
	errRunProbeScript = "error on running probe script"
	errParseProbe     = "error on parsing probe output"
	errNotCompliant   = "error on compliance check"
)

var (
	connectionCache = sync.Map{}
)

// Setup adds a controller that reconciles SSHTask managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(apiv1a1.SSHTaskGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apiv1a1.StoreConfigGroupVersionKind))
	}

	opts := []managed.ReconcilerOption{
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apiv1a1.ProviderConfigUsage{}),
			newServiceFn: newSSHClient}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...),
		managed.WithManagementPolicies(),
	}

	if o.Features.Enabled(feature.EnableAlphaChangeLogs) {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}

	if o.MetricOptions != nil {
		opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
	}

	if o.MetricOptions != nil && o.MetricOptions.MRStateMetrics != nil {
		stateMetricsRecorder := statemetrics.NewMRStateRecorder(
			mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &apiv1a1.SSHTaskList{}, o.MetricOptions.PollStateMetricInterval,
		)
		if err := mgr.Add(stateMetricsRecorder); err != nil {
			return errors.Wrap(err, "cannot register MR state metrics recorder for kind apisv1alpha1.SSHTaskList")
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(apiv1a1.SSHTaskGroupVersionKind), opts...)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&apiv1a1.SSHTask{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(creds *config) (*ssh.Client, error)
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	logger := klog.FromContext(ctx).WithName("[CONNECT]")
	logger.Info("Connecting...")
	cr, ok := mg.(*apiv1a1.SSHTask)
	if !ok {
		return nil, errors.New(errNotSSHTask)
	}

	// Skip if being deleted - we still return an external to allow cleanup paths to run.
	if cr.GetDeletionTimestamp() != nil {
		logger.Info("Resource is being deleted. Skip establishing new connection.")
		return &external{}, nil
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionFalse, errTrackPCUsage, err.Error())
		cr.SetConditions(xpv1.ReconcileError(err))
		return nil, errors.Wrap(err, errTrackPCUsage)
	}


	pc := &apiv1a1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionFalse, errGetPC, err.Error())
		cr.SetConditions(xpv1.ReconcileError(err))
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionFalse, errGetCreds, err.Error())
		cr.SetConditions(xpv1.ReconcileError(err))
		return nil, errors.Wrap(err, errGetCreds)
	}

	connectionData, err := getConnectionConfigFromSecret(data)
	if err != nil { // no connection data found
		cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionFalse, errExtractCreds, err.Error())
		cr.SetConditions(xpv1.Unavailable())
		return &external{}, nil
	}

	remoteHost := fmt.Sprintf("%s:%s", connectionData.RemoteHostIP, connectionData.RemoteHostPort)
	logger.Info(fmt.Sprintf("Connecting to remote host [%s]...", remoteHost))
	if val, exists := connectionCache.Load(remoteHost); exists {
		logger.Info(fmt.Sprintf("Connection [%s] exists in cache.", remoteHost))
		ok := testConnection(val.(*ssh.Client), 10)
		if ok {
			logger.Info(fmt.Sprintf("Connection [%s] is valid.", remoteHost))
			cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionTrue, "", "Connection is valid")
			cr.SetCondition(xpv1.TypeSynced, corev1.ConditionTrue, "", "Connection is valid")
			return &external{ssh: val.(*ssh.Client), cfg: connectionData}, nil
		}
		// invalid - drop it
		val.(*ssh.Client).Close()
		connectionCache.Delete(remoteHost)
		logger.Info(fmt.Sprintf("Connection [%s] is invalid. Removing from cache.", remoteHost))
	}

	svc, err := c.newServiceFn(connectionData)
	if err != nil {
		cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionFalse, errCreateConn, err.Error())
		cr.SetConditions(xpv1.ReconcileError(err))
		return &external{}, nil
	}

	connectionCache.Store(remoteHost, svc)
	logger.Info(fmt.Sprintf("Connection [okay]: [%s] stored in cache.", remoteHost))
	cr.SetCondition(apiv1a1.TypeConnected, corev1.ConditionTrue, "", "Connection is valid")
	cr.SetCondition(xpv1.TypeSynced, corev1.ConditionTrue, "", "Connection is valid")

	return &external{ssh: svc, cfg: connectionData}, nil
}

// external implements managed.ExternalClient and executes scripts over SSH.
type external struct {
	ssh *ssh.Client
	// cfg is the connection configuration used to open ssh,
	// used only to populate status.atProvider.endpoint.
	cfg *config
}

// probePayload is the JSON contract printed by probeScript.
// Example:
// {
//   "facts": {... arbitrary JSON ...},
//   "compliant": true,
//   "drift": {... optional ...}
// }
type probePayload struct {
	Facts     json.RawMessage       `json:"facts"`
	Compliant bool                  `json:"compliant"`
	Drift     map[string]any        `json:"drift,omitempty"`
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*apiv1a1.SSHTask)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotSSHTask)
	}
	logger := klog.FromContext(ctx).WithName("[OBSERVE]")

	if cr.GetDeletionTimestamp() != nil {
		logger.Info("Resource is being deleted. Skip Observe.")
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	// If not connected yet, report as observed but don't attempt apply.
	if c.ssh == nil {
		logger.Info("No SSH connection available during Observe; will retry on next poll.")
		cr.SetConditions(xpv1.ReconcileError(errors.New(errNoSSHConn)))
		// Keep current conditions; do not trigger Update.
		return managed.ExternalObservation{}, nil
	}

	// Defaults
	execSpec := defaultExecution(cr.Spec.ForProvider.Execution)
	capture := defaultCapture(cr.Spec.ForProvider.ArtifactPolicy)
	populateEndpoint(cr, c.cfg)

	now := metav1Now()

	// Decide whether we need to (re)run probeScript
	needProbe := isObserveStale(cr)
	if cr.Spec.ForProvider.Scripts.ProbeScript == nil {
		// No probeScript means we cannot determine compliance/facts -> treat as not up-to-date so Update runs ensure.
		logger.Info("No probeScript provided; treating resource as not up-to-date.")
		cr.SetConditions(xpv1.ReconcileSuccess())
		cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errNoProbeScript, "No probeScript provided")
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	if !needProbe {
		// Probe considered fresh and keep current Ready condition.
		cr.SetConditions(xpv1.Available(), xpv1.ReconcileSuccess())
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}

	var probeOut, probeErr string
	var probeExit int
	var probeDur time.Duration
	var runErr error

	logger.Info("Running probeScript...")
	probeExit, probeOut, probeErr, probeDur, runErr = runScript2(ctx, c.ssh, cr.Spec.ForProvider.Scripts.ProbeScript.Inline, execSpec, capture)
	cr.Status.AtProvider.LastCheckTime = &now
	cr.Status.AtProvider.LastRun = &apiv1a1.LastRunStatus{
		Time:       &now,
		ExitCode:   int32Ptr(int32(probeExit)),
		RetryCount: int32Ptr(0),
	}
	cr.Status.AtProvider.Artifacts = selectArtifacts(capture, probeOut, probeErr)
	logger.Info(fmt.Sprintf("probeScript completed: exit=%d, dur=%s, out=%dB, err=%dB", probeExit, probeDur, len(probeOut), len(probeErr)))

	if runErr != nil || probeExit != 0 {
		logger.Info(fmt.Sprintf("%s: exit=%d err=%v", errRunProbeScript, probeExit, runErr))
		// We cannot trust stale facts; mark not ready but don't error hard to avoid hot loop.
		cr.SetConditions(xpv1.ReconcileSuccess())
		cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errRunProbeScript, runErr.Error())
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false, // trigger Update, which may try ensure (but Update will re-probe first too)
		}, nil
	}

	// Parse payload
	var payload probePayload
	if err := json.Unmarshal([]byte(probeOut), &payload); err != nil {
		logger.Info(fmt.Sprintf("failed to parse probe JSON: %v", err))
		cr.SetConditions(xpv1.ReconcileSuccess())
		cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errParseProbe, "Probe output is not valid JSON")
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, nil
	}

	digest := computeDigest(payload.Facts)
	if cr.Status.AtProvider.Observed == nil {
		cr.Status.AtProvider.Observed = &apiv1a1.ObservedStatus{}
	}
	cr.Status.AtProvider.Observed.Raw = &apiextensionsv1.JSON{Raw: payload.Facts}
	cr.Status.AtProvider.Observed.Digest = &digest
	cr.Status.AtProvider.Observed.ObservedAt = &now

	// Optional: map fields from facts into Observed.Fields if mapping configured.
	mapObservedFields(cr, payload.Facts)

	// Set Ready depending on compliance
	if payload.Compliant {
		cr.SetConditions(xpv1.Available(), xpv1.ReconcileSuccess())
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}

	// Not compliant → Update will run ensure.
	cr.SetConditions(xpv1.ReconcileSuccess())
	cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errNotCompliant, "Resource is not compliant")
	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: false,
	}, nil
	
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	// SSHTask does not create external resources; Apply happens in Update.
	// We treat Create same as Update for first convergence.
	logger := klog.FromContext(ctx).WithName("[CREATE]")
	logger.Info("Calling update...")
	_, err := c.Update(ctx, mg)
	return managed.ExternalCreation{}, err
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*apiv1a1.SSHTask)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotSSHTask)
	}
	logger := klog.FromContext(ctx).WithName("[UPDATE]")
	logger.Info("Updating scripts...")

	if c.ssh == nil {
		logger.Info("No SSH connection available during Update; skipping apply.")
		cr.SetConditions(xpv1.Unavailable(), xpv1.ReconcileError(errors.New("not connected")))
		return managed.ExternalUpdate{}, nil
	}

	execSpec := defaultExecution(cr.Spec.ForProvider.Execution)
	capture := defaultCapture(cr.Spec.ForProvider.ArtifactPolicy)
	populateEndpoint(cr, c.cfg)


	// 1) Run ensureScript with retries.
	ensure := cr.Spec.ForProvider.Scripts.EnsureScript
	if ensure == nil {
		logger.Info("Resource not compliant but no ensureScript provided.")
		cr.SetConditions(xpv1.Unavailable(), xpv1.ReconcileError(errors.New("ensureScript missing")))
		return managed.ExternalUpdate{}, nil
	}

	maxAttempts := int32(1)
	if execSpec.MaxAttempts != nil && *execSpec.MaxAttempts > 0 {
		maxAttempts = *execSpec.MaxAttempts
	}

	var attempt int32
	var exit int
	var out, errOut string
	var dur time.Duration
	var runErr error

	for attempt = 0; attempt < maxAttempts; attempt++ {
		logger.Info(fmt.Sprintf("Running ensureScript (attempt %d/%d)...", attempt+1, maxAttempts))
		now := metav1Now()
		exit, out, errOut, dur, runErr = runScript2(ctx, c.ssh, ensure.Inline, execSpec, capture)
		cr.Status.AtProvider.LastRun = &apiv1a1.LastRunStatus{
			Time:       &now,
			ExitCode:   int32Ptr(int32(exit)),
			RetryCount: int32Ptr(attempt),
		}
		cr.Status.AtProvider.Artifacts = selectArtifacts(capture, out, errOut)

		if runErr == nil && exit == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	if runErr != nil || exit != 0 {
		logger.Info(fmt.Sprintf("ensureScript failed after %d attempt(s): exit=%d err=%v dur=%s", attempt+1, exit, runErr, dur))
		cr.SetConditions(xpv1.Unavailable(), xpv1.ReconcileError(errors.Errorf("%s: exit=%d", errRunEnsureScript, exit)))
		return managed.ExternalUpdate{}, errors.Errorf("ensure failed: exit=%d", exit)
	}

	// Force a second probe to verify & refresh facts.
	logger.Info("Running probeScript (post-ensure verification)...")
	postExit, postOut, postErr, _, postRunErr := runScript2(ctx, c.ssh, cr.Spec.ForProvider.Scripts.ProbeScript.Inline, execSpec, capture)
	now := metav1Now()
	cr.Status.AtProvider.LastRun = &apiv1a1.LastRunStatus{
		Time:       &now,
		ExitCode:   int32Ptr(int32(postExit)),
		RetryCount: int32Ptr(attempt),
	}
	// Keep ensure artifacts unless capture is none; otherwise override with post-probe artifacts
	if capture == apiv1a1.CaptureNone {
		cr.Status.AtProvider.Artifacts = selectArtifacts(capture, postOut, postErr)
	}

	if postRunErr != nil || postExit != 0 {
		logger.Info(fmt.Sprintf("post-ensure probe failed: exit=%d err=%v", postExit, postRunErr))
		cr.SetConditions(xpv1.ReconcileSuccess())
		cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errRunProbeScript, fmt.Sprintf("post-ensure probe failed: exit=%d, err=%v", postExit, postRunErr))
		return managed.ExternalUpdate{}, nil
	}

	var post probePayload
	if err := json.Unmarshal([]byte(postOut), &post); err != nil {
		logger.Info(fmt.Sprintf("failed to parse post-probe JSON: %v", err))
		cr.SetConditions(xpv1.Unavailable(), xpv1.ReconcileSuccess())
		return managed.ExternalUpdate{}, nil
	}

	// Update observed from post-probe
	dgst2 := computeDigest(post.Facts)
	nowMeta2 := metav1Now()
	cr.Status.AtProvider.Observed.Raw = &apiextensionsv1.JSON{Raw: post.Facts}
	cr.Status.AtProvider.Observed.Digest = &dgst2
	cr.Status.AtProvider.Observed.ObservedAt = &nowMeta2
	mapObservedFields(cr, post.Facts)

	if !post.Compliant {
		logger.Info("post-ensure probe indicates not compliant; will requeue.")
		cr.SetConditions(xpv1.ReconcileSuccess())
		cr.SetCondition(xpv1.TypeReady, corev1.ConditionFalse, errNotCompliant, "Resource is not compliant after ensureScript")
		return managed.ExternalUpdate{}, nil
	}

	// Success
	cr.SetConditions(xpv1.Available(), xpv1.ReconcileSuccess())
	return managed.ExternalUpdate{}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*apiv1a1.SSHTask)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotSSHTask)
	}
	logger := klog.FromContext(ctx).WithName("[DELETE]")
	logger.Info("Deleting scripts...")

	// Run cleanupScript if present.
	if c.ssh != nil && cr.Spec.ForProvider.Scripts.CleanupScript != nil {
		execSpec := defaultExecution(cr.Spec.ForProvider.Execution)
		capture := defaultCapture(cr.Spec.ForProvider.ArtifactPolicy)
		exit, out, errOut, _, err := runScript2(ctx, c.ssh, cr.Spec.ForProvider.Scripts.CleanupScript.Inline, execSpec, capture)
		now := metav1Now()
		populateEndpoint(cr, c.cfg)
		cr.Status.AtProvider.LastRun = &apiv1a1.LastRunStatus{
			Time:       &now,
			ExitCode:   int32Ptr(int32(exit)),
			RetryCount: int32Ptr(0),
		}
		cr.Status.AtProvider.Artifacts = selectArtifacts(capture, out, errOut)

		if err != nil || exit != 0 {
			logger.Info(fmt.Sprintf("cleanupScript error: exit=%d err=%v", exit, err))
			// Still allow deletion to proceed.
		}
	}

	// Close SSH connection if it exists.
	if c.ssh != nil {
		c.ssh.Close()
	}
	cr.SetConditions(xpv1.ReconcileSuccess())
	return managed.ExternalDelete{}, nil
}

func (c *external) Disconnect(ctx context.Context) error {
	// no-op; connection lifecycle handled via cache
	return nil
}
