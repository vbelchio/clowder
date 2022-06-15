package autoscaler

import (
	"fmt"
	"log"

	res "k8s.io/apimachinery/pkg/api/resource"

	crd "github.com/RedHatInsights/clowder/apis/cloud.redhat.com/v1alpha1"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/config"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers"
	deployProvider "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/deployment"
	apps "k8s.io/api/apps/v1"
	v2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewNoneDBProvider returns a new none db provider object.
func NewSimpleAutoScalerProvider(p *providers.Provider) (providers.ClowderProvider, error) {
	return &simpleAutoScalerProvider{Provider: *p}, nil
}

type simpleAutoScalerProvider struct {
	providers.Provider
	Config config.DatabaseConfig
}

func (sp *simpleAutoScalerProvider) Provide(app *crd.ClowdApp, appConfig *config.AppConfig) error {
	for _, deployment := range app.Spec.Deployments {
		// Create the autoscaler the current deployment if one is specified
		if deployment.SimpleAutoScaler != nil {

			cachedDeployment, err := sp.getDeploymentFromCache(&deployment, app)
			//Failing to get the deployment from cache isn't fatal
			//May just mean a resource isn't ready yet
			//Move on to the next iteration
			if err != nil {
				log.Println(err)
				return err
			}
			deploymentHPA := makeDeployemntSimpleHPA(&deployment, app, appConfig, cachedDeployment)
			hpaResource := deploymentHPA.getResource()

			err = sp.Client.Create(sp.Ctx, &hpaResource)
			if err != nil {
				fmt.Println("HPA Error: ", err)
			}
		}
	}
	return nil
}

//Get the core apps.Deployment from the provider cache
//This is in simpleAutoScalerProvider because we need access to the provider cache
func (sp *simpleAutoScalerProvider) getDeploymentFromCache(clowdDeployment *crd.Deployment, app *crd.ClowdApp) (*apps.Deployment, error) {
	nn := app.GetDeploymentNamespacedName(clowdDeployment)
	d := &apps.Deployment{}
	if err := sp.Cache.Get(deployProvider.CoreDeployment, d, nn); err != nil {
		return d, err
	}
	return d, nil
}

func makeDeployemntSimpleHPA(deployment *crd.Deployment, app *crd.ClowdApp, appConfig *config.AppConfig, coreDeployment *apps.Deployment) deployemntSimpleHPA {
	return deployemntSimpleHPA{
		deployment:     deployment,
		app:            app,
		appConfig:      appConfig,
		coreDeployment: coreDeployment,
	}
}

type deployemntSimpleHPA struct {
	deployment     *crd.Deployment
	app            *crd.ClowdApp
	appConfig      *config.AppConfig
	coreDeployment *apps.Deployment
}

func (d *deployemntSimpleHPA) getResource() v2.HorizontalPodAutoscaler {
	hpa := d.makeHPA()
	hpa.Spec.Metrics = d.makeMetricsSpecs()
	return hpa
}

func (d *deployemntSimpleHPA) makeHPA() v2.HorizontalPodAutoscaler {
	hpa := v2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       "HorizontalPodAutoscaler",
			APIVersion: "autoscaling/v2",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.app.Name + "-" + d.coreDeployment.Name + "-" + "hpa",
			Namespace: d.coreDeployment.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: d.coreDeployment.TypeMeta.APIVersion,
					Kind:       d.coreDeployment.TypeMeta.Kind,
					Name:       d.coreDeployment.ObjectMeta.Name,
					UID:        d.coreDeployment.ObjectMeta.UID,
				},
			},
		},
		Spec: v2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: v2.CrossVersionObjectReference{
				APIVersion: d.coreDeployment.APIVersion,
				Kind:       d.coreDeployment.Kind,
				Name:       d.coreDeployment.Name,
			},
			MinReplicas: &d.deployment.SimpleAutoScaler.Replicas.Min,
			MaxReplicas: d.deployment.SimpleAutoScaler.Replicas.Max,
		},
	}
	return hpa
}

func (d *deployemntSimpleHPA) makeMetricsSpecs() []v2.MetricSpec {
	metricsSpecs := []v2.MetricSpec{}

	if d.deployment.SimpleAutoScaler.RAM.ScaleAtUtilization != 0 {
		metricsSpec := d.makeAverageUtilizationMetricSpec(v1.ResourceMemory, d.deployment.SimpleAutoScaler.RAM.ScaleAtUtilization)
		metricsSpecs = append(metricsSpecs, metricsSpec)
	}
	if d.deployment.SimpleAutoScaler.RAM.ScaleAtValue != "" {
		threshhold := res.MustParse(d.deployment.SimpleAutoScaler.RAM.ScaleAtValue)
		metricsSpec := d.makeAverageValueMetricSpec(v1.ResourceMemory, threshhold)
		metricsSpecs = append(metricsSpecs, metricsSpec)
	}

	if d.deployment.SimpleAutoScaler.CPU.ScaleAtUtilization != 0 {
		metricsSpec := d.makeAverageUtilizationMetricSpec(v1.ResourceCPU, d.deployment.SimpleAutoScaler.CPU.ScaleAtUtilization)
		metricsSpecs = append(metricsSpecs, metricsSpec)
	}
	if d.deployment.SimpleAutoScaler.CPU.ScaleAtValue != "" {
		threshhold := res.MustParse(d.deployment.SimpleAutoScaler.CPU.ScaleAtValue)
		metricsSpec := d.makeAverageValueMetricSpec(v1.ResourceCPU, threshhold)
		metricsSpecs = append(metricsSpecs, metricsSpec)
	}

	return metricsSpecs
}

func (d *deployemntSimpleHPA) makeAverageValueMetricSpec(resource v1.ResourceName, threshhold res.Quantity) v2.MetricSpec {
	ms := d.makeBasicMetricSpec(resource)
	ms.Resource.Target.Type = v2.AverageValueMetricType
	ms.Resource.Target.AverageValue = &threshhold
	return ms
}

func (d *deployemntSimpleHPA) makeAverageUtilizationMetricSpec(resource v1.ResourceName, threshhold int32) v2.MetricSpec {
	ms := d.makeBasicMetricSpec(resource)
	ms.Resource.Target.Type = v2.UtilizationMetricType
	ms.Resource.Target.AverageUtilization = &threshhold
	return ms
}

func (d *deployemntSimpleHPA) makeBasicMetricSpec(resource v1.ResourceName) v2.MetricSpec {
	ms := v2.MetricSpec{
		Type: v2.MetricSourceType("Resource"),
		Resource: &v2.ResourceMetricSource{
			Name:   resource,
			Target: v2.MetricTarget{},
		},
	}
	return ms
}
