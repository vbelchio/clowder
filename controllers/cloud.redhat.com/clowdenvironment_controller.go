/*


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

package controllers

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	rc "github.com/RedHatInsights/rhc-osdk-utils/resource_cache"
	strimzi "github.com/RedHatInsights/strimzi-client-go/apis/kafka.strimzi.io/v1beta2"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	// Import the providers to initialize them
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/confighash"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/cronjob"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/database"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/dependencies"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/deployment"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/featureflags"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/inmemorydb"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/iqe"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/kafka"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/logging"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/metrics"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/namespace"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/objectstore"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/pullsecrets"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/serviceaccount"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/servicemesh"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/sidecar"
	provutils "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/utils"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/web"

	crd "github.com/RedHatInsights/clowder/apis/cloud.redhat.com/v1alpha1"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/clowderconfig"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/errors"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	cond "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/RedHatInsights/rhc-osdk-utils/utils"
)

var mu sync.RWMutex
var cEnv = ""

const (
	envFinalizer  = "finalizer.env.cloud.redhat.com"
	SKIPRECONCILE = "SKIPRECONCILE"
)

// ClowdEnvironmentReconciler reconciles a ClowdEnvironment object
type ClowdEnvironmentReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cloud.redhat.com,resources=clowdenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.redhat.com,resources=clowdenvironments/status,verbs=get;update;patch

func SetEnv(name string) {
	mu.Lock()
	defer mu.Unlock()
	cEnv = name
}

func ReleaseEnv() {
	mu.Lock()
	defer mu.Unlock()
	cEnv = ""
}

func ReadEnv() string {
	mu.RLock()
	defer mu.RUnlock()
	return cEnv
}

//Before reconciliation we perform some sanity checks and bail out if any of them fail
func (r *ClowdEnvironmentReconciler) reconcileGuard(ctx context.Context, req ctrl.Request, env *crd.ClowdEnvironment, log logr.Logger) (bool, error) {
	canReconcile := true
	if getEnvErr := r.Client.Get(ctx, req.NamespacedName, env); getEnvErr != nil {
		if k8serr.IsNotFound(getEnvErr) {
			// Must have been deleted
			return false, nil
		}
		log.Info("Namespace not found", "err", getEnvErr)
		return false, getEnvErr
	}
	return canReconcile, nil
}

//Determine if an env has been marked for deletion. If so deal with finalizaers.
func (r *ClowdEnvironmentReconciler) markedForDeletion(provider *providers.Provider, _ *rc.ObjectCache) (ctrl.Result, error) {
	isEnvMarkedForDeletion := provider.Env.GetDeletionTimestamp() != nil
	if !isEnvMarkedForDeletion {
		return ctrl.Result{}, nil
	}
	if contains(provider.Env.GetFinalizers(), envFinalizer) {
		if finalizeErr := r.finalizeEnvironment(provider.Log, provider.Env, *provider); finalizeErr != nil {
			provider.Log.Info("Cloud not finalize", "err", finalizeErr)
			return ctrl.Result{Requeue: true}, finalizeErr
		}

		controllerutil.RemoveFinalizer(provider.Env, envFinalizer)
		removeFinalizeErr := r.Update(provider.Ctx, provider.Env)
		if removeFinalizeErr != nil {
			provider.Log.Info("Cloud not remove finalizer", "err", removeFinalizeErr)
			return ctrl.Result{}, removeFinalizeErr
		}
	}
	return ctrl.Result{}, nil
}

//Add finalizer to the environment if it doesn't have it already
func (r *ClowdEnvironmentReconciler) addFinalizerForCR(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	// Add finalizer for this CR
	if !contains(provider.Env.GetFinalizers(), envFinalizer) {
		if addFinalizeErr := r.addFinalizer(provider.Log, provider.Env); addFinalizeErr != nil {
			provider.Log.Info("Could not add finalizer", "err", addFinalizeErr)
			return ctrl.Result{}, addFinalizeErr
		}
	}
	return ctrl.Result{}, nil
}

//Get the target namespace for the environment
func (r *ClowdEnvironmentReconciler) getTargetNamespace(env *crd.ClowdEnvironment, ctx context.Context, log logr.Logger) (reconcile.Result, error) {
	namespace := core.Namespace{}
	namespaceName := types.NamespacedName{
		Name: env.Spec.TargetNamespace,
	}
	if nErr := r.Client.Get(ctx, namespaceName, &namespace); nErr != nil {
		log.Info("Namespace get error", "err", nErr)
		r.Recorder.Eventf(env, "Warning", "NamespaceMissing", "Requested Target Namespace [%s] is missing", env.Spec.TargetNamespace)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.Client, env, crd.ReconciliationFailed, nErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, nErr
	}
	env.Status.TargetNamespace = env.Spec.TargetNamespace
	return ctrl.Result{}, nil
}

//Create a target namespace for the environment if none is present
func (r *ClowdEnvironmentReconciler) createTargetNamespace(env *crd.ClowdEnvironment, ctx context.Context, log logr.Logger) (reconcile.Result, error) {
	env.Status.TargetNamespace = env.GenerateTargetNamespace()
	namespace := &core.Namespace{}
	namespace.SetName(env.Status.TargetNamespace)
	if snErr := r.Client.Create(ctx, namespace); snErr != nil {
		log.Info("Namespace create error", "err", snErr)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.Client, env, crd.ReconciliationFailed, snErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, snErr
	}
	return ctrl.Result{}, nil
}

//Update the target namespace for the environment if it has changed
func (r *ClowdEnvironmentReconciler) updateNamespace(env *crd.ClowdEnvironment, ctx context.Context, log logr.Logger) (reconcile.Result, error) {
	if statErr := r.Client.Status().Update(ctx, env); statErr != nil {
		log.Info("Namespace create error", "err", statErr)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.Client, env, crd.ReconciliationFailed, statErr); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, statErr
	}
	return ctrl.Result{}, nil
}

//Set up the target namespace for the env. This is highly conditional. We may create or get a namespace, then update and finally update it
func (r *ClowdEnvironmentReconciler) setupNamespace(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	if provider.Env.Status.TargetNamespace != "" {
		return ctrl.Result{}, nil
	}

	if provider.Env.Spec.TargetNamespace == "" {
		result, err := r.createTargetNamespace(provider.Env, provider.Ctx, provider.Log)
		if err != nil {
			return result, err
		}
	} else {
		result, err := r.getTargetNamespace(provider.Env, provider.Ctx, provider.Log)
		if err != nil {
			return result, err
		}
	}

	result, err := r.updateNamespace(provider.Env, provider.Ctx, provider.Log)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil

}

//Verify that the target namespace exists and hasn't been deleted
func (r *ClowdEnvironmentReconciler) verifyNamespace(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	ens := &core.Namespace{}
	if getNSErr := r.Client.Get(provider.Ctx, types.NamespacedName{Name: provider.Env.Status.TargetNamespace}, ens); getNSErr != nil {
		return ctrl.Result{Requeue: true}, getNSErr
	}

	if ens.ObjectMeta.DeletionTimestamp != nil {
		provider.Log.Info("Env target namespace is to be deleted - skipping reconcile")
		return ctrl.Result{}, errors.New(SKIPRECONCILE)
	}

	return ctrl.Result{}, nil
}

func (r *ClowdEnvironmentReconciler) clowdStatusErrorWrapper(msg string, errorToHandle error, log logr.Logger, env *crd.ClowdEnvironment, ctx context.Context) (reconcile.Result, error) {
	if errorToHandle != nil {
		log.Info(msg, "err", errorToHandle)
		if setClowdStatusErr := SetClowdEnvConditions(ctx, r.Client, env, crd.ReconciliationFailed, errorToHandle); setClowdStatusErr != nil {
			log.Info("Set status error", "err", setClowdStatusErr)
			return ctrl.Result{Requeue: true}, setClowdStatusErr
		}
		return ctrl.Result{Requeue: true}, errorToHandle
	}
	return ctrl.Result{}, nil
}

func (r *ClowdEnvironmentReconciler) getResourceStatus(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	envReady, _, getEnvResErr := GetEnvResourceStatus(provider.Ctx, r.Client, provider.Env)
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

func (r *ClowdEnvironmentReconciler) setFinalStatus(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	finalStatusErr := r.Client.Status().Update(provider.Ctx, provider.Env)
	if finalStatusErr == nil {
		return ctrl.Result{}, nil
	}
	provider.Log.Info("Final Status error", "err", finalStatusErr)
	setClowdStatusErr := SetClowdEnvConditions(provider.Ctx, r.Client, provider.Env, crd.ReconciliationFailed, finalStatusErr)
	if setClowdStatusErr == nil {
		return ctrl.Result{Requeue: true}, finalStatusErr
	}
	provider.Log.Info("Set status error", "err", setClowdStatusErr)
	return ctrl.Result{Requeue: true}, setClowdStatusErr
}

func (r *ClowdEnvironmentReconciler) runProviders(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	return r.clowdStatusErrorWrapper("Error running providers", runProvidersForEnv(provider.Log, *provider), provider.Log, provider.Env, provider.Ctx)
}

func (r *ClowdEnvironmentReconciler) applyCache(provider *providers.Provider, cache *rc.ObjectCache) (reconcile.Result, error) {
	return r.clowdStatusErrorWrapper("Cache Error", cache.ApplyAll(), provider.Log, provider.Env, provider.Ctx)
}

func (r *ClowdEnvironmentReconciler) setAppInfoWrapper(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	return r.clowdStatusErrorWrapper("setAppInfo error", r.setAppInfo(*provider), provider.Log, provider.Env, provider.Ctx)
}

func (r *ClowdEnvironmentReconciler) setResourceStatus(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	return r.clowdStatusErrorWrapper("SetEnvResourceStatus error", SetEnvResourceStatus(provider.Ctx, r.Client, provider.Env), provider.Log, provider.Env, provider.Ctx)
}

func (r *ClowdEnvironmentReconciler) setPrometheusStatus(provider *providers.Provider, _ *rc.ObjectCache) (reconcile.Result, error) {
	setPrometheusStatus(provider.Env)
	return ctrl.Result{}, nil
}

func (r *ClowdEnvironmentReconciler) deleteUnunsedResources(provider *providers.Provider, cache *rc.ObjectCache) (reconcile.Result, error) {
	opts := []client.ListOption{
		client.MatchingLabels{provider.Env.GetPrimaryLabel(): provider.Env.GetClowdName()},
	}

	// Delete all resources that are not used anymore
	rErr := cache.Reconcile(provider.Env.GetUID(), opts...)
	if rErr != nil {
		return ctrl.Result{Requeue: true}, rErr
	}

	if successSetErr := SetClowdEnvConditions(provider.Ctx, r.Client, provider.Env, crd.ReconciliationSuccessful, nil); successSetErr != nil {
		provider.Log.Info("Set status error", "err", successSetErr)
		return ctrl.Result{Requeue: true}, successSetErr
	}

	return ctrl.Result{}, nil
}

//ClowdEnvironmentReconciler reconciles a ClowdEnvironment
func (r *ClowdEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("env", req.Name).WithValues("rid", utils.RandString(5))
	ctx = context.WithValue(ctx, errors.ClowdKey("log"), &log)
	ctx = context.WithValue(ctx, errors.ClowdKey("recorder"), &r.Recorder)
	env := crd.ClowdEnvironment{}

	canReconcile, err := r.reconcileGuard(ctx, req, &env, log)
	if !canReconcile {
		return ctrl.Result{}, err
	}

	SetEnv(env.Name)
	defer ReleaseEnv()

	if _, ok := presentEnvironments[env.Name]; !ok {
		presentEnvironments[env.Name] = true
	}
	presentEnvsMetric.Set(float64(len(presentEnvironments)))

	delete(managedEnvironments, env.Name)

	defer func() {
		managedEnvsMetric.Set(float64(len(managedEnvironments)))
	}()

	ctx = context.WithValue(ctx, errors.ClowdKey("obj"), &env)

	cacheConfig := rc.NewCacheConfig(Scheme, errors.ClowdKey("log"), ProtectedGVKs, DebugOptions)

	cache := rc.NewObjectCache(ctx, r.Client, cacheConfig)

	provider := providers.Provider{
		Ctx:    ctx,
		Client: r.Client,
		Env:    &env,
		Cache:  &cache,
		Log:    log,
	}

	//So, we've entered into a sort of a workqueue type system here
	//The right way to build this out it to create a type and interface for ReconcileStep
	//And then move all of these methods into new ReconcileStep implementations
	//I started to do that but it is too big for this refactor
	//This hints at how it would look
	steps := []func(*providers.Provider, *rc.ObjectCache) (ctrl.Result, error){
		r.markedForDeletion,
		r.addFinalizerForCR,
		r.setupNamespace,
		r.verifyNamespace,
		r.runProviders,
		r.applyCache,
		r.setAppInfoWrapper,
		r.setResourceStatus,
		r.setPrometheusStatus,
		r.getResourceStatus,
		r.setFinalStatus,
		r.deleteUnunsedResources,
	}

	for _, step := range steps {
		result, err := step(&provider, &cache)
		if err != nil {
			if err.Error() == SKIPRECONCILE {
				return result, nil
			}
			return result, err
		}
	}

	managedEnvironments[env.Name] = true

	r.Recorder.Eventf(&env, "Normal", "SuccessfulReconciliation", "Environment reconciled [%s]", env.GetClowdName())
	log.Info("Reconciliation successful")

	return ctrl.Result{}, nil
}

func setPrometheusStatus(env *crd.ClowdEnvironment) {
	var hostname string

	if env.Spec.Providers.Metrics.Mode == "app-interface" {
		hostname = env.Spec.Providers.Metrics.Prometheus.AppInterfaceHostname
	} else {
		hostname = fmt.Sprintf("prometheus-operated.%s.svc.cluster.local", env.Status.TargetNamespace)
	}

	env.Status.Prometheus = crd.PrometheusStatus{Hostname: hostname}
}

func runProvidersForEnv(log logr.Logger, provider providers.Provider) error {
	for _, provAcc := range providers.ProvidersRegistration.Registry {
		provutils.DebugLog(log, "running provider:", "name", provAcc.Name, "order", provAcc.Order)
		start := time.Now()
		_, err := provAcc.SetupProvider(&provider)
		elapsed := time.Since(start).Seconds()
		providerMetrics.With(prometheus.Labels{"provider": provAcc.Name, "source": "clowdenv"}).Observe(elapsed)
		if err != nil {
			return errors.Wrap(fmt.Sprintf("getprov: %s", provAcc.Name), err)
		}
		provutils.DebugLog(log, "running provider: complete", "name", provAcc.Name, "order", provAcc.Order, "elapsed", fmt.Sprintf("%f", elapsed))
	}
	return nil
}

func runProvidersForEnvFinalize(log logr.Logger, provider providers.Provider) error {
	for _, provAcc := range providers.ProvidersRegistration.Registry {
		if provAcc.FinalizeProvider != nil {
			provutils.DebugLog(log, "running provider finalize:", "name", provAcc.Name, "order", provAcc.Order)
			err := provAcc.FinalizeProvider(&provider)
			if err != nil {
				return errors.Wrap(fmt.Sprintf("prov finalize: %s", provAcc.Name), err)
			}
			provutils.DebugLog(log, "running provider finalize: complete", "name", provAcc.Name, "order", provAcc.Order)
		}
	}
	return nil
}

// SetupWithManager sets up with manager
func (r *ClowdEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("env")

	ctrlr := ctrl.NewControllerManagedBy(mgr).For(&crd.ClowdEnvironment{})

	ctrlr.Owns(&apps.Deployment{}, builder.WithPredicates(deploymentPredicate(r.Log, "app")))
	ctrlr.Owns(&core.Service{}, builder.WithPredicates(generationOnlyPredicate(r.Log, "app")))
	ctrlr.Owns(&core.Secret{}, builder.WithPredicates(alwaysPredicate(r.Log, "app")))
	ctrlr.Watches(
		&source.Kind{Type: &crd.ClowdApp{}},
		handler.EnqueueRequestsFromMapFunc(r.envToEnqueueUponAppUpdate),
		builder.WithPredicates(generationOnlyPredicate(r.Log, "env")),
	)

	if clowderconfig.LoadedConfig.Features.WatchStrimziResources {
		ctrlr.Owns(&strimzi.Kafka{}, builder.WithPredicates(kafkaPredicate(r.Log, "app")))
		ctrlr.Owns(&strimzi.KafkaConnect{}, builder.WithPredicates(alwaysPredicate(r.Log, "app")))
		ctrlr.Owns(&strimzi.KafkaUser{}, builder.WithPredicates(alwaysPredicate(r.Log, "app")))
		ctrlr.Owns(&strimzi.KafkaTopic{}, builder.WithPredicates(alwaysPredicate(r.Log, "app")))
	}

	ctrlr.WithOptions(controller.Options{
		RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(time.Duration(500*time.Millisecond), time.Duration(60*time.Second)),
	})
	return ctrlr.Complete(r)
}

func (r *ClowdEnvironmentReconciler) envToEnqueueUponAppUpdate(a client.Object) []reconcile.Request {
	ctx := context.Background()
	obj := types.NamespacedName{
		Name:      a.GetName(),
		Namespace: a.GetNamespace(),
	}

	// Get the ClowdEnvironment resource

	app := crd.ClowdApp{}
	err := r.Client.Get(ctx, obj, &app)

	if err != nil {
		if k8serr.IsNotFound(err) {
			// Must have been deleted
			return []reconcile.Request{}
		}
		r.Log.Error(err, "Failed to fetch ClowdApp")
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name: app.Spec.EnvName,
		},
	}}
}

func (r *ClowdEnvironmentReconciler) setAppInfo(p providers.Provider) error {
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

func (r *ClowdEnvironmentReconciler) finalizeEnvironment(reqLogger logr.Logger, e *crd.ClowdEnvironment, provider providers.Provider) error {

	err := runProvidersForEnvFinalize(reqLogger, provider)
	if err != nil {
		return err
	}

	if e.Spec.TargetNamespace == "" {
		namespace := &core.Namespace{}
		namespace.SetName(e.Status.TargetNamespace)
		reqLogger.Info(fmt.Sprintf("Removing auto-generated namespace for %s", e.Name))
		r.Recorder.Eventf(e, "Warning", "NamespaceDeletion", "Clowder Environment [%s] had no targetNamespace, deleting generated namespace", e.Name)
		r.Delete(context.TODO(), namespace)
	}
	delete(managedEnvironments, e.Name)
	managedEnvsMetric.Set(float64(len(managedEnvironments)))

	delete(presentEnvironments, e.Name)
	presentEnvsMetric.Set(float64(len(presentEnvironments)))

	reqLogger.Info("Successfully finalized ClowdEnvironment")
	return nil
}

func (r *ClowdEnvironmentReconciler) addFinalizer(reqLogger logr.Logger, e *crd.ClowdEnvironment) error {
	reqLogger.Info("Adding Finalizer for the ClowdEnvironment")
	controllerutil.AddFinalizer(e, envFinalizer)

	// Update CR
	err := r.Update(context.TODO(), e)
	if err != nil {
		reqLogger.Error(err, "Failed to update ClowdEnvironment with finalizer")
		return err
	}
	return nil
}
