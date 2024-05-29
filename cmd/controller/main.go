/*
Copyright The Kubernetes Authors.

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

package main

import (
	"context"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/karpenter-provider-aws/pkg/cache"
	awscontrollersinstancetype "github.com/aws/karpenter-provider-aws/pkg/controllers/providers/instancetype"
	awscontrollerspricing "github.com/aws/karpenter-provider-aws/pkg/controllers/providers/pricing"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instancetype"
	"github.com/aws/karpenter-provider-aws/pkg/providers/pricing"
	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clusterapi "sigs.k8s.io/karpenter-provider-cluster-api/pkg/cloudprovider"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/operator"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/webhooks"
	corewebhooks "sigs.k8s.io/karpenter/pkg/webhooks"
)

func main() {
	ctx, op := operator.NewOperatorWithMachinePool(coreoperator.NewOperator())
	capiCloudProvider := clusterapi.NewCloudProvider(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		op.EventRecorder,
		op.GetClient(),
	)
	lo.Must0(op.AddHealthzCheck("cloud-provider", capiCloudProvider.LivenessProbe))
	cloudProvider := metrics.Decorate(capiCloudProvider)

	operator := op.
		WithControllers(ctx, corecontrollers.NewControllers(
			op.Clock,
			op.GetClient(),
			state.NewCluster(op.Clock, op.GetClient(), cloudProvider),
			op.EventRecorder,
			cloudProvider,
		)...)

	var controllers []controller.Controller

	platform := "AWS"
	if platform == "AWS" {
		controllers = NewAWSControllers(
			ctx,
			op.Session,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			op.UnavailableOfferingsCache,
			cloudProvider,
			op.PricingProvider,
			op.InstanceTypesProvider,
		)
	}

	operator.WithControllers(ctx, controllers...).
		WithWebhooks(ctx, corewebhooks.NewWebhooks()...).
		WithWebhooks(ctx, webhooks.NewWebhooks()...).
		Start(ctx)

}

// Instantiate platform specific controllers.
func NewAWSControllers(ctx context.Context, sess *session.Session, clk clock.Clock, kubeClient client.Client, recorder events.Recorder,
	unavailableOfferings *cache.UnavailableOfferings, cloudProvider cloudprovider.CloudProvider,
	pricingProvider pricing.Provider, instanceTypeProvider instancetype.Provider) []controller.Controller {

	controllers := []controller.Controller{
		awscontrollerspricing.NewController(pricingProvider),
		awscontrollersinstancetype.NewController(instanceTypeProvider),
	}
	return controllers
}
