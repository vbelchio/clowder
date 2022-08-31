package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	crd "github.com/RedHatInsights/clowder/apis/cloud.redhat.com/v1alpha1"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/errors"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers"
	provutils "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/utils"
	rc "github.com/RedHatInsights/rhc-osdk-utils/resource_cache"
	"github.com/prometheus/client_golang/prometheus"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	cond "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	SKIPRECONCILE = "SKIPRECONCILE"
)

//During reconciliation we handle errors in 2 ways: sometimes we want to error out of reconciliation and sometimes we want to skip reconciliation.
func shouldSkipReconciliation(err error) bool {
	if err != nil && err.Error() == SKIPRECONCILE {
		return true
	}
	return false
}

//This is the base type for reconcilier states that is extended for each specific step
type ReconcilerStep struct {
	//client.Client
	provider *providers.Provider
	cache    *rc.ObjectCache
	recorder record.EventRecorder
}

func (r *ReconcilerStep) SetProvider(provider *providers.Provider) {
	r.provider = provider
}
func (r *ReconcilerStep) SetCache(cache *rc.ObjectCache) {
	r.cache = cache
}
func (r *ReconcilerStep) SetRecorder(recorder record.EventRecorder) {
	r.recorder = recorder
}

//Many steps use this logic to wrap errors while status is set.
func (r *ReconcilerStep) clowdStatusErrorWrapper(msg string, errorToHandle error) (reconcile.Result, error) {
	if errorToHandle != nil {
		r.provider.Log.Info(msg, "err", errorToHandle)
		if setClowdStatusErr := SetClowdEnvConditions(r.provider.Ctx, r.provider.Client, r.provider.Env, crd.ReconciliationFailed, errorToHandle); setClowdStatusErr != nil {
			r.provider.Log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, errorToHandle
	}
	return ctrl.Result{}, nil
}

//So we can treat all of these different types of reconcilier steps the same way
type iReconcilerStep interface {
	Run() (reconcile.Result, error)
	SetCache(*rc.ObjectCache)
	SetProvider(*providers.Provider)
	SetRecorder(record.EventRecorder)
}

func MakeReconcileStepRunner(provider *providers.Provider, cache *rc.ObjectCache, recorder record.EventRecorder) ReconcileStepRunner {
	return ReconcileStepRunner{
		Provider: provider,
		Cache:    cache,
		Recorder: recorder,
	}
}

//Does the work of running a collection of reconcilier steps
type ReconcileStepRunner struct {
	steps    []iReconcilerStep
	Provider *providers.Provider
	Cache    *rc.ObjectCache
	Recorder record.EventRecorder
}

//add a step to the runner
func (r *ReconcileStepRunner) AddStep(step iReconcilerStep) {
	step.SetProvider(r.Provider)
	step.SetCache(r.Cache)
	step.SetRecorder(r.Recorder)
	r.steps = append(r.steps, step)
}

//run the steps
func (r *ReconcileStepRunner) Run() (reconcile.Result, error) {
	for _, step := range r.steps {
		result, err := step.Run()
		if err != nil {
			return result, err
		}
	}
	return reconcile.Result{}, nil
}

////// Step Types

//Determines of a ClowdEnvironment is marked for deletion and if so
//calls finalizers and updates
type MarkedForDeleteionStep struct {
	ReconcilerStep
}

//Determine if an env has been marked for deletion. If so deal with finalizaers.
func (r *MarkedForDeleteionStep) Run() (ctrl.Result, error) {
	isEnvMarkedForDeletion := r.provider.Env.GetDeletionTimestamp() != nil
	if !isEnvMarkedForDeletion {
		return ctrl.Result{}, nil
	}
	if contains(r.provider.Env.GetFinalizers(), envFinalizer) {
		if finalizeErr := r.finalizeEnvironment(); finalizeErr != nil {
			r.provider.Log.Info("Cloud not finalize", "err", finalizeErr)
			return ctrl.Result{Requeue: true}, finalizeErr
		}

		controllerutil.RemoveFinalizer(r.provider.Env, envFinalizer)
		removeFinalizeErr := r.provider.Client.Update(r.provider.Ctx, r.provider.Env)
		if removeFinalizeErr != nil {
			r.provider.Log.Info("Cloud not remove finalizer", "err", removeFinalizeErr)
			return ctrl.Result{}, removeFinalizeErr
		}
	}
	return ctrl.Result{}, nil
}
func (r *MarkedForDeleteionStep) finalizeEnvironment() error {

	err := runProvidersForEnvFinalize(r.provider.Log, *r.provider)
	if err != nil {
		return err
	}

	if r.provider.Env.Spec.TargetNamespace == "" {
		namespace := &core.Namespace{}
		namespace.SetName(r.provider.Env.Status.TargetNamespace)
		r.provider.Log.Info(fmt.Sprintf("Removing auto-generated namespace for %s", r.provider.Env.Name))
		r.recorder.Eventf(r.provider.Env, "Warning", "NamespaceDeletion", "Clowder Environment [%s] had no targetNamespace, deleting generated namespace", r.provider.Env.Name)
		r.provider.Client.Delete(context.TODO(), namespace)
	}
	delete(managedEnvironments, r.provider.Env.Name)
	managedEnvsMetric.Set(float64(len(managedEnvironments)))

	delete(presentEnvironments, r.provider.Env.Name)
	presentEnvsMetric.Set(float64(len(presentEnvironments)))

	r.provider.Log.Info("Successfully finalized ClowdEnvironment")
	return nil
}

//Adds a finalizer to a ClowdEnvironment
type AddFinalizerStep struct {
	ReconcilerStep
}

func (r *AddFinalizerStep) Run() (ctrl.Result, error) {
	//Add finalizer to the environment if it doesn't have it already
	if !contains(r.provider.Env.GetFinalizers(), envFinalizer) {
		if addFinalizeErr := r.addFinalizer(); addFinalizeErr != nil {
			r.provider.Log.Info("Could not add finalizer", "err", addFinalizeErr)
			return ctrl.Result{}, addFinalizeErr
		}
	}
	return ctrl.Result{}, nil
}

func (r *AddFinalizerStep) addFinalizer() error {
	r.provider.Log.Info("Adding Finalizer for the ClowdEnvironment")
	controllerutil.AddFinalizer(r.provider.Env, envFinalizer)

	// Update CR
	err := r.provider.Client.Update(context.TODO(), r.provider.Env)
	if err != nil {
		r.provider.Log.Error(err, "Failed to update ClowdEnvironment with finalizer")
		return err
	}
	return nil
}

//Creates, gets, or updates the target namespace for a ClowdEnvironment
type SetupNamespaceStep struct {
	ReconcilerStep
}

func (r *SetupNamespaceStep) Run() (ctrl.Result, error) {
	if r.provider.Env.Status.TargetNamespace != "" {
		return ctrl.Result{}, nil
	}

	if r.provider.Env.Spec.TargetNamespace == "" {
		result, err := r.createTargetNamespace()
		if err != nil {
			return result, err
		}
	} else {
		result, err := r.getTargetNamespace()
		if err != nil {
			return result, err
		}
	}

	result, err := r.updateNamespace()
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *SetupNamespaceStep) createTargetNamespace() (reconcile.Result, error) {
	env := r.provider.Env
	ctx := r.provider.Ctx
	log := r.provider.Log
	env.Status.TargetNamespace = env.GenerateTargetNamespace()
	namespace := &core.Namespace{}
	namespace.SetName(env.Status.TargetNamespace)
	if snErr := r.provider.Client.Create(ctx, namespace); snErr != nil {
		log.Info("Namespace create error", "err", snErr)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.provider.Client, env, crd.ReconciliationFailed, snErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, snErr
	}
	return ctrl.Result{}, nil
}

func (r *SetupNamespaceStep) getTargetNamespace() (reconcile.Result, error) {
	env := r.provider.Env
	ctx := r.provider.Ctx
	log := r.provider.Log
	namespace := core.Namespace{}
	namespaceName := types.NamespacedName{
		Name: env.Spec.TargetNamespace,
	}
	if nErr := r.provider.Client.Get(ctx, namespaceName, &namespace); nErr != nil {
		log.Info("Namespace get error", "err", nErr)
		r.recorder.Eventf(env, "Warning", "NamespaceMissing", "Requested Target Namespace [%s] is missing", env.Spec.TargetNamespace)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.provider.Client, env, crd.ReconciliationFailed, nErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, nErr
	}
	env.Status.TargetNamespace = env.Spec.TargetNamespace
	return ctrl.Result{}, nil
}

func (r *SetupNamespaceStep) updateNamespace() (reconcile.Result, error) {
	env := r.provider.Env
	ctx := r.provider.Ctx
	log := r.provider.Log
	if statErr := r.provider.Client.Status().Update(ctx, env); statErr != nil {
		log.Info("Namespace create error", "err", statErr)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.provider.Client, env, crd.ReconciliationFailed, statErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, statErr
	}
	return ctrl.Result{}, nil
}

//Verifies the target namespace for a ClowdEnvironment and checks if it should be deleted
type VerifyNamespaceStep struct {
	ReconcilerStep
}

func (r *VerifyNamespaceStep) Run() (ctrl.Result, error) {
	ens := &core.Namespace{}
	if getNSErr := r.provider.Client.Get(r.provider.Ctx, types.NamespacedName{Name: r.provider.Env.Status.TargetNamespace}, ens); getNSErr != nil {
		return ctrl.Result{Requeue: true}, getNSErr
	}

	if ens.ObjectMeta.DeletionTimestamp != nil {
		r.provider.Log.Info("Env target namespace is to be deleted - skipping reconcile")
		return ctrl.Result{}, errors.New(SKIPRECONCILE)
	}

	return ctrl.Result{}, nil
}

//Runs all of the registered providers
type RunProviderStep struct {
	ReconcilerStep
}

func (r *RunProviderStep) Run() (ctrl.Result, error) {
	return r.clowdStatusErrorWrapper("Error running providers", r.runProvidersForEnv())
}

func (r *RunProviderStep) runProvidersForEnv() error {
	for _, provAcc := range providers.ProvidersRegistration.Registry {
		provutils.DebugLog(r.provider.Log, "running provider:", "name", provAcc.Name, "order", provAcc.Order)
		start := time.Now()
		_, err := provAcc.SetupProvider(r.provider)
		elapsed := time.Since(start).Seconds()
		providerMetrics.With(prometheus.Labels{"provider": provAcc.Name, "source": "clowdenv"}).Observe(elapsed)
		if err != nil {
			return errors.Wrap(fmt.Sprintf("getprov: %s", provAcc.Name), err)
		}
		provutils.DebugLog(r.provider.Log, "running provider: complete", "name", provAcc.Name, "order", provAcc.Order, "elapsed", fmt.Sprintf("%f", elapsed))
	}
	return nil
}

//Tried to apply all objects in the cache
type ApplyCacheStep struct {
	ReconcilerStep
}

func (r *ApplyCacheStep) Run() (ctrl.Result, error) {
	return r.clowdStatusErrorWrapper("Error running providers", r.cache.ApplyAll())
}

//Sets the names and info for clowd apps
type SetAppInfoStep struct {
	ReconcilerStep
}

func (r *SetAppInfoStep) Run() (ctrl.Result, error) {
	return r.clowdStatusErrorWrapper("setAppInfo error", r.setAppInfo())
}

func (r *SetAppInfoStep) setAppInfo() error {
	p := r.provider
	// Get all the ClowdApp resources
	appList, err := p.Env.GetAppsInEnv(p.Ctx, p.Client)

	if err != nil {
		return err
	}
	apps := []crd.AppInfo{}

	appMap := map[string]crd.ClowdApp{}
	names := []string{}

	for _, app := range appList.Items {
		name := fmt.Sprintf("%s-%s", app.Name, app.Namespace)
		names = append(names, name)
		appMap[name] = app
	}

	sort.Strings(names)

	// Populate
	for _, name := range names {
		app := appMap[name]

		if app.GetDeletionTimestamp() != nil {
			continue
		}

		appstatus := crd.AppInfo{
			Name:        app.Name,
			Deployments: []crd.DeploymentInfo{},
		}

		depMap := map[string]crd.Deployment{}
		depNames := []string{}

		for _, pod := range app.Spec.Deployments {
			depNames = append(depNames, pod.Name)
			depMap[pod.Name] = pod
		}

		sort.Strings(depNames)

		for _, podName := range depNames {
			pod := depMap[podName]

			deploymentStatus := crd.DeploymentInfo{
				Name: fmt.Sprintf("%s-%s", app.Name, pod.Name),
			}
			if bool(pod.Web) || pod.WebServices.Public.Enabled {
				deploymentStatus.Hostname = fmt.Sprintf("%s.%s.svc", deploymentStatus.Name, app.Namespace)
				deploymentStatus.Port = p.Env.Spec.Providers.Web.Port
			}
			appstatus.Deployments = append(appstatus.Deployments, deploymentStatus)
		}
		apps = append(apps, appstatus)
	}

	p.Env.Status.Apps = apps
	return nil
}

type SetResourceStatusStep struct {
	ReconcilerStep
}

func (r *SetResourceStatusStep) Run() (reconcile.Result, error) {
	provider := r.provider
	return r.clowdStatusErrorWrapper("SetEnvResourceStatus error", SetEnvResourceStatus(provider.Ctx, r.provider.Client, provider.Env))
}

type SetPrometheusStatusStep struct {
	ReconcilerStep
}

//We don't do any error checking around this. I'm not sure why, but that's just how it always was
func (r *SetPrometheusStatusStep) Run() (reconcile.Result, error) {
	setPrometheusStatus(r.provider.Env)
	return ctrl.Result{}, nil
}

type GetResourceStatusStep struct {
	ReconcilerStep
}

func (r *GetResourceStatusStep) Run() (reconcile.Result, error) {
	provider := r.provider
	envReady, _, getEnvResErr := GetEnvResourceStatus(provider.Ctx, r.provider.Client, provider.Env)
	if getEnvResErr != nil {
		provider.Log.Info("GetEnvResourceStatus error", "err", getEnvResErr)
		return ctrl.Result{Requeue: true}, getEnvResErr
	}

	envStatus := core.ConditionFalse
	successCond := cond.Get(provider.Env, crd.ReconciliationSuccessful)
	if successCond != nil {
		envStatus = successCond.Status
	}

	provider.Env.Status.Ready = envReady && (envStatus == core.ConditionTrue)
	provider.Env.Status.Generation = provider.Env.Generation

	return ctrl.Result{}, nil
}

type SetFinalStatusStep struct {
	ReconcilerStep
}

func (r *SetFinalStatusStep) Run() (reconcile.Result, error) {
	provider := r.provider
	finalStatusErr := r.provider.Client.Status().Update(provider.Ctx, provider.Env)
	if finalStatusErr == nil {
		return ctrl.Result{}, nil
	}
	provider.Log.Info("Final Status error", "err", finalStatusErr)
	setClowdStatusErr := SetClowdEnvConditions(provider.Ctx, r.provider.Client, provider.Env, crd.ReconciliationFailed, finalStatusErr)
	if setClowdStatusErr == nil {
		return ctrl.Result{Requeue: true}, finalStatusErr
	}
	provider.Log.Info("Set status error", "err", setClowdStatusErr)
	return ctrl.Result{Requeue: true}, setClowdStatusErr
}

type DeleteUnunsedResourcesStep struct {
	ReconcilerStep
}

func (r *DeleteUnunsedResourcesStep) Run() (reconcile.Result, error) {
	provider := r.provider
	cache := r.cache
	opts := []client.ListOption{
		client.MatchingLabels{provider.Env.GetPrimaryLabel(): provider.Env.GetClowdName()},
	}

	// Delete all resources that are not used anymore
	rErr := cache.Reconcile(provider.Env.GetUID(), opts...)
	if rErr != nil {
		return ctrl.Result{Requeue: true}, rErr
	}

	if successSetErr := SetClowdEnvConditions(provider.Ctx, r.provider.Client, provider.Env, crd.ReconciliationSuccessful, nil); successSetErr != nil {
		provider.Log.Info("Set status error", "err", successSetErr)
		return ctrl.Result{Requeue: true}, successSetErr
	}

	return ctrl.Result{}, nil
}
