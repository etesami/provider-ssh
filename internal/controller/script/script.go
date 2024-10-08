/*
Copyright 2022 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package script

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"golang.org/x/crypto/ssh"

	apisv1alpha1 "github.com/crossplane/provider-ssh/apis/v1alpha1"
	sshv1alpha1 "github.com/crossplane/provider-ssh/internal/client"
	"github.com/crossplane/provider-ssh/internal/features"
)

const (
	errNotScript    = "managed resource is not a Script custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNewClient = "cannot create new Service"
)

// Setup adds a controller that reconciles Script managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(apisv1alpha1.ScriptGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(apisv1alpha1.ScriptGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			newServiceFn: sshv1alpha1.NewSSHClient}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...),
		managed.WithManagementPolicies())

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&apisv1alpha1.Script{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(ctx context.Context, creds []byte) (*ssh.Client, error)
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	logger := log.FromContext(ctx).WithName("[CONNECT]")
	logger.Info(fmt.Sprintf("[%s] Creating connection...", mg.GetName()))
	cr, ok := mg.(*apisv1alpha1.Script)
	if !ok {
		return nil, errors.New(errNotScript)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	// if the resource is being deleted, we just return
	cond := cr.GetCondition(xpv1.Deleting().Type)
	if cond.Type == xpv1.TypeReady && cond.Status == "False" && cond.Reason == xpv1.ReasonDeleting {
		logger.Info(fmt.Sprintf("[%s] Resource is being deleted. Skip the connection.", mg.GetName()))
		return &external{}, nil
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	svc, err := c.newServiceFn(ctx, data)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	logger.Info(fmt.Sprintf("[%s] Creating connection [okay]", mg.GetName()))
	return &external{service: svc}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	// A 'client' used to connect to the external resource API.
	service interface{}
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	logger := log.FromContext(ctx).WithName("[OBSERVE]")
	logger.Info(fmt.Sprintf("[%s] Observing...", mg.GetName()))
	cr, ok := mg.(*apisv1alpha1.Script)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotScript)
	}

	// if the resource is being deleted, we just return
	cd := cr.GetCondition(xpv1.Deleting().Type)
	if cd.Type == xpv1.TypeReady && cd.Status == "False" && cd.Reason == xpv1.ReasonDeleting {
		logger.Info(fmt.Sprintf("[%s] Observing failed. Resource is being deleted.", mg.GetName()))
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	// We expect to have the CheckStatusScript
	if cr.Spec.ForProvider.StatusCheckScript != "" {
		stdout, stderr, err := sshv1alpha1.ExecuteScript(
			ctx, c.service.(*ssh.Client), cr.Spec.ForProvider.StatusCheckScript, cr.Spec.ForProvider.Variables, cr.Spec.ForProvider.SudoEnabled)

		// nolint:nilerr
		if err != nil {
			// If the script fails, it means there is either an issue with the
			// init script and the target is not ready yet, or the init script is not
			// executed at all. In both cases, we request to run init script again
			// by returning ResourceExists: false
			var exitStatus int
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitStatus = exitErr.ExitStatus()
			} else {
				exitStatus = 1
				logger.Info(fmt.Sprintf("[%s] Unable to detect exit code", mg.GetName()))
			}

			logger.Info(fmt.Sprintf("[%s] Observing failed. Exit code: %d", mg.GetName(), exitStatus))
			cr.Status.AtProvider.Stdout = stdout
			cr.Status.AtProvider.Stderr = stderr
			cr.Status.AtProvider.StatusCode = exitStatus

			// if the exit code is 1, it means the script failed. This type of failure
			// is not recoverable automatically, so we set the status to ReconcileError.
			if exitStatus == 1 {
				cr.SetConditions(xpv1.ReconcileError(errors.Wrap(err, "Script failed with exit code 1.")))
				return managed.ExternalObservation{}, errors.Wrap(err, "Script failed with exit code 1.")
			}

			// If the exit code is 100, it means the resources does not exist yet.
			if exitStatus == 100 {
				return managed.ExternalObservation{ResourceExists: false}, nil
			}

			// If the exit code is a custom applecation-specific code, it means the script
			// failed but the failure may be recoverable. The recovery should be handled by
			// the update script. We don't return error here, as the update does not get called
			// instead we update resource status fields with returned stdout, stderr and exit code.
			return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, nil
		}

		logger.Info(fmt.Sprintf("[%s] Observing was [okay]. Update the status.", mg.GetName()))
		cr.Status.AtProvider.Stdout = stdout
		cr.Status.AtProvider.Stderr = stderr
		cr.SetConditions(xpv1.Available())
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil

	}

	logger.Info(fmt.Sprintf("[%s] Observing, no status check script.", mg.GetName()))

	return managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  true,
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	logger := log.FromContext(ctx).WithName("[CREATE]")
	logger.Info(fmt.Sprintf("[%s] Creating init script...", mg.GetName()))
	cr, ok := mg.(*apisv1alpha1.Script)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotScript)
	}

	if cr.Spec.ForProvider.InitScript != "" {
		_, _, err := sshv1alpha1.ExecuteScript(
			ctx, c.service.(*ssh.Client), cr.Spec.ForProvider.InitScript, cr.Spec.ForProvider.Variables, cr.Spec.ForProvider.SudoEnabled)
		if err != nil {
			// If the script fails, it means there is either an issue with the
			// init script and the target is not ready yet, or the init script is not
			// executed at all. By returning error here, the reconciler will not proceed,
			// and user intervention is required.
			cr.SetConditions(xpv1.ReconcileError(errors.Wrap(err, "Init Script failed.")))
			return managed.ExternalCreation{}, err
		}
	}
	return managed.ExternalCreation{
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	logger := log.FromContext(ctx).WithName("[UPDATE]")
	logger.Info(fmt.Sprintf("[%s] Updating resource...", mg.GetName()))
	cr, ok := mg.(*apisv1alpha1.Script)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotScript)
	}

	if cr.Spec.ForProvider.UpdateScript != "" {
		_, _, err := sshv1alpha1.ExecuteScript(
			ctx, c.service.(*ssh.Client), cr.Spec.ForProvider.UpdateScript, cr.Spec.ForProvider.Variables, cr.Spec.ForProvider.SudoEnabled)
		if err != nil {
			// the update script is supposed to return error if the update fails and is not recoverable.
			// If we return error here, the reconcile will not proceed, and user intervention is required.
			cr.SetConditions(xpv1.ReconcileError(errors.Wrap(err, "Update Script failed.")))
			return managed.ExternalUpdate{}, err
		}
	}
	// If there is no update script, or the update does not encounter any error, we return success.
	// and we will observe the resource again to check if the update was successful.
	return managed.ExternalUpdate{
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	logger := log.FromContext(ctx).WithName("[DELETE]")
	logger.Info(fmt.Sprintf("[%s] Deleting...", mg.GetName()))
	cr, ok := mg.(*apisv1alpha1.Script)
	if !ok {
		return errors.New(errNotScript)
	}

	if cr.Spec.ForProvider.CleanupScript != "" {
		_, _, err := sshv1alpha1.ExecuteScript(
			ctx, c.service.(*ssh.Client), cr.Spec.ForProvider.CleanupScript, cr.Spec.ForProvider.Variables, cr.Spec.ForProvider.SudoEnabled)

		if err != nil {
			logger.Info(fmt.Sprintf("[%s] Deleting failed.", mg.GetName()))
			return err
		}
	}

	return nil
}
