package providers

import (
	"context"
	"fmt"

	"github.com/aws/karpenter-provider-aws/pkg/providers/subnet"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	capaexpv1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type CAPAInstanceProvider struct {
	client client.Client
}

// TODO (alberto): Implement this
// type CAPZInstanceProvider struct {
// 	client client.Client
// }

func NewCAPAInstanceProvider(c client.Client) *CAPAInstanceProvider {
	return &CAPAInstanceProvider{
		client: c,
	}
}

func (p *CAPAInstanceProvider) Create(ctx context.Context, nodeClaim *corev1beta1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*v1.ObjectReference, error) {
	capacityType := getCapacityType(nodeClaim, instanceTypes)
	overrides := getOverrides(instanceTypes,
		map[string]*subnet.Subnet{},
		scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...).Get(v1.LabelTopologyZone),
		capacityType,
		"imageID")
	awsMachinePool := &capaexpv1.AWSMachinePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:            nodeClaim.Name,
			Namespace:       "",
			Labels:          map[string]string{},
			Annotations:     map[string]string{},
			OwnerReferences: []metav1.OwnerReference{},
		},
		Spec: capaexpv1.AWSMachinePoolSpec{
			MinSize: 1,
			MaxSize: 1,
			MixedInstancesPolicy: &capaexpv1.MixedInstancesPolicy{
				InstancesDistribution: &capaexpv1.InstancesDistribution{
					OnDemandBaseCapacity:                pointer.Int64(1),
					OnDemandPercentageAboveBaseCapacity: pointer.Int64(0),
					SpotAllocationStrategy:              capaexpv1.SpotAllocationStrategyPriceCapacityOptimized,
					OnDemandAllocationStrategy:          capaexpv1.OnDemandAllocationStrategyLowestPrice,
				},
				Overrides: overrides,
			},
		},
	}
	if capacityType == corev1beta1.CapacityTypeSpot {
		awsMachinePool.Spec.MixedInstancesPolicy.InstancesDistribution.SpotAllocationStrategy = capaexpv1.SpotAllocationStrategyCapacityOptimized
	} else {
		awsMachinePool.Spec.MixedInstancesPolicy.InstancesDistribution.OnDemandAllocationStrategy = capaexpv1.OnDemandAllocationStrategyLowestPrice
	}

	if err := p.client.Create(ctx, awsMachinePool); err != nil {
		return nil, fmt.Errorf("creating AWSMachinePool, %w", err)
	}

	return &v1.ObjectReference{
		APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
		Kind:       "AWSMachinePool",
		Name:       awsMachinePool.Name,
		Namespace:  "",
	}, nil
}

func (p *CAPAInstanceProvider) Get(ctx context.Context, instanceID string) error {
	// Implement me.
	return nil
}

func (p *CAPAInstanceProvider) List(ctx context.Context) error {
	// Implement me.
	return nil
}

func (p *CAPAInstanceProvider) Delete(ctx context.Context, instanceID string) error {
	// Implement me.
	return nil
}

func (p *CAPAInstanceProvider) CreateTags(ctx context.Context, instanceID string, tags map[string]string) error {
	// Implement me.
	return nil
}

// getCapacityType selects spot if both constraints are flexible and there is an
// available offering. The AWS Cloud Provider defaults to [ on-demand ], so spot
// must be explicitly included in capacity type requirements.
func getCapacityType(nodeClaim *corev1beta1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) string {
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.
		Spec.Requirements...)
	if requirements.Get(corev1beta1.CapacityTypeLabelKey).Has(corev1beta1.CapacityTypeSpot) {
		for _, instanceType := range instanceTypes {
			for _, offering := range instanceType.Offerings.Available() {
				if requirements.Get(v1.LabelTopologyZone).Has(offering.Zone) && offering.CapacityType == corev1beta1.CapacityTypeSpot {
					return corev1beta1.CapacityTypeSpot
				}
			}
		}
	}
	return corev1beta1.CapacityTypeOnDemand
}

// getOverrides creates and returns awsMachinePool overrides for the cross product of InstanceTypes and subnets (with subnets being constrained by
// zones and the offerings in InstanceTypes)
func getOverrides(instanceTypes []*cloudprovider.InstanceType, zonalSubnets map[string]*subnet.Subnet, zones *scheduling.Requirement, capacityType string, image string) []capaexpv1.Overrides {
	// Unwrap all the offerings to a flat slice that includes a pointer
	// to the parent instance type name
	type offeringWithParentName struct {
		cloudprovider.Offering
		parentInstanceTypeName string
	}
	var unwrappedOfferings []offeringWithParentName
	for _, it := range instanceTypes {
		ofs := lo.Map(it.Offerings.Available(), func(of cloudprovider.Offering, _ int) offeringWithParentName {
			return offeringWithParentName{
				Offering:               of,
				parentInstanceTypeName: it.Name,
			}
		})
		unwrappedOfferings = append(unwrappedOfferings, ofs...)
	}

	// TODO (alberto): ignore subnets and image filtering for now for simplicity.
	var overrides []capaexpv1.Overrides
	for _, offering := range unwrappedOfferings {
		if capacityType != offering.CapacityType {
			continue
		}
		// if !zones.Has(offering.Zone) {
		// 	continue
		// }
		// _, ok := zonalSubnets[offering.Zone]
		// if !ok {
		// 	continue
		// }
		overrides = append(overrides, capaexpv1.Overrides{
			InstanceType: offering.parentInstanceTypeName,
			// SubnetId:     lo.ToPtr(subnet.ID),
			// ImageId:      aws.String(image),
			// // This is technically redundant, but is useful if we have to parse insufficient capacity errors from
			// // CreateFleet so that we can figure out the zone rather than additional API calls to look up the subnet
			// AvailabilityZone: lo.ToPtr(subnet.Zone),
		})
	}
	return overrides
}
