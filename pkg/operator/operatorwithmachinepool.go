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

package operator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awsclient "github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	awscache "github.com/aws/karpenter-provider-aws/pkg/cache"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	awsinstancetype "github.com/aws/karpenter-provider-aws/pkg/providers/instancetype"
	awspricing "github.com/aws/karpenter-provider-aws/pkg/providers/pricing"
	"github.com/aws/karpenter-provider-aws/pkg/providers/subnet"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"knative.dev/pkg/logging"
	clusterapi "sigs.k8s.io/karpenter-provider-cluster-api/pkg/cloudprovider"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers"
	"sigs.k8s.io/karpenter/pkg/operator"
)

type OperatorWithMacchinePool struct {
	*operator.Operator

	Session                   *session.Session
	UnavailableOfferingsCache *awscache.UnavailableOfferings
	// SubnetProvider            awssubnet.Provider
	PricingProvider       clusterapi.PricingProvider
	InstanceTypesProvider clusterapi.InstanceTypeProvider
	InstanceProvider      clusterapi.InstanceProvider
}

func NewOperatorWithMachinePool(ctx context.Context, operator *operator.Operator) (context.Context, *OperatorWithMacchinePool) {
	// DESIGN: This is written over the aws provider only for dev purposes.
	// DESIGN: we can discriminate here at runtime so we can have different instance type, pricing implementations for different providers.
	// DESIGN: We could just vendor any provider in our CAPI provider repo and reuse their instance type/pricing controllers at our convenience
	// as showed in this code.
	var pricingProvider clusterapi.PricingProvider
	var instanceTypeProvider clusterapi.InstanceTypeProvider
	var instanceProvider clusterapi.InstanceProvider

	platform := "AWS"
	if platform == "AWS" {
		session := newAWSSession(ctx)
		pricingProvider = NewAWSPricingProvider(ctx, session)
		instanceTypeProvider = NewAWSInstanceTypeProvider(ctx, session, pricingProvider)
		instanceProvider = providers.NewCAPAInstanceProvider(operator.GetClient())
	}
	return ctx, &OperatorWithMacchinePool{
		Operator:              operator,
		PricingProvider:       pricingProvider,
		InstanceTypesProvider: instanceTypeProvider,
		InstanceProvider:      instanceProvider,
	}
}

// Provider specific helpers.
func newAWSSession(ctx context.Context) *session.Session {
	config := &aws.Config{
		STSRegionalEndpoint: endpoints.RegionalSTSEndpoint,
	}

	if assumeRoleARN := options.FromContext(ctx).AssumeRoleARN; assumeRoleARN != "" {
		config.Credentials = stscreds.NewCredentials(session.Must(session.NewSession()), assumeRoleARN,
			func(provider *stscreds.AssumeRoleProvider) { SetDurationAndExpiry(ctx, provider) })
	}

	return WithUserAgent(session.Must(session.NewSession(
		request.WithRetryer(
			config,
			awsclient.DefaultRetryer{NumMaxRetries: awsclient.DefaultRetryerMaxNumRetries},
		),
	)))
}

func NewAWSPricingProvider(ctx context.Context, sess *session.Session) clusterapi.PricingProvider {
	if *sess.Config.Region == "" {
		logging.FromContext(ctx).Debug("retrieving region from IMDS")
		region, err := ec2metadata.New(sess).Region()
		*sess.Config.Region = lo.Must(region, err, "failed to get region from metadata server")
	}
	ec2api := ec2.New(sess)
	if err := CheckEC2Connectivity(ctx, ec2api); err != nil {
		logging.FromContext(ctx).Fatalf("Checking EC2 API connectivity, %s", err)
	}

	return awspricing.NewDefaultProvider(
		ctx,
		awspricing.NewAPI(sess, *sess.Config.Region),
		ec2api,
		*sess.Config.Region,
	)
}

func NewAWSInstanceTypeProvider(ctx context.Context, sess *session.Session, pricingProvider clusterapi.PricingProvider) clusterapi.InstanceTypeProvider {
	unavailableOfferingsCache := awscache.NewUnavailableOfferings()

	ec2api := ec2.New(sess)
	if err := CheckEC2Connectivity(ctx, ec2api); err != nil {
		logging.FromContext(ctx).Fatalf("Checking EC2 API connectivity, %s", err)
	}
	subnetProvider := subnet.NewDefaultProvider(ec2api, cache.New(awscache.DefaultTTL, awscache.DefaultCleanupInterval), cache.New(awscache.AvailableIPAddressTTL, awscache.DefaultCleanupInterval), cache.New(awscache.AssociatePublicIPAddressTTL, awscache.DefaultCleanupInterval))

	return awsinstancetype.NewDefaultProvider(
		*sess.Config.Region,
		cache.New(awscache.InstanceTypesAndZonesTTL, awscache.DefaultCleanupInterval),
		ec2api,
		subnetProvider,
		unavailableOfferingsCache,
		pricingProvider,
	)
}

// WithUserAgent adds a karpenter specific user-agent string to AWS session
func WithUserAgent(sess *session.Session) *session.Session {
	userAgent := fmt.Sprintf("karpenter.sh-%s", operator.Version)
	sess.Handlers.Build.PushBack(request.MakeAddToUserAgentFreeFormHandler(userAgent))
	return sess
}

// CheckEC2Connectivity makes a dry-run call to DescribeInstanceTypes.  If it fails, we provide an early indicator that we
// are having issues connecting to the EC2 API.
func CheckEC2Connectivity(ctx context.Context, api ec2iface.EC2API) error {
	_, err := api.DescribeInstanceTypesWithContext(ctx, &ec2.DescribeInstanceTypesInput{DryRun: aws.Bool(true)})
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "DryRunOperation" {
		return nil
	}
	return err
}

func SetDurationAndExpiry(ctx context.Context, provider *stscreds.AssumeRoleProvider) {
	provider.Duration = options.FromContext(ctx).AssumeRoleDuration
	provider.ExpiryWindow = time.Duration(10) * time.Second
}
