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

package cloudprovider

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"

	awsv1beta1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capiexpv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machine"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machinedeployment"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	// TODO (elmiko) if we make these exposed constants from the CAS we can import them instead of redifining and risking drift
	cpuKey          = "capacity.cluster-autoscaler.kubernetes.io/cpu"
	memoryKey       = "capacity.cluster-autoscaler.kubernetes.io/memory"
	gpuCountKey     = "capacity.cluster-autoscaler.kubernetes.io/gpu-count"
	gpuTypeKey      = "capacity.cluster-autoscaler.kubernetes.io/gpu-type"
	diskCapacityKey = "capacity.cluster-autoscaler.kubernetes.io/ephemeral-disk"
	labelsKey       = "capacity.cluster-autoscaler.kubernetes.io/labels"
	taintsKey       = "capacity.cluster-autoscaler.kubernetes.io/taints"
	maxPodsKey      = "capacity.cluster-autoscaler.kubernetes.io/maxPods"
)

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

func NewCloudProvider(instanceTypeProvider InstanceTypeProvider, instanceProvider InstanceProvider, recorder events.Recorder, kubeClient client.Client) *CloudProvider {
	return &CloudProvider{
		instanceProvider:     instanceProvider,
		instanceTypeProvider: instanceTypeProvider,
		kubeClient:           kubeClient,
		recorder:             recorder,
	}
}

// Providers implement this leverage cloud provider specific
type InstanceTypeProvider interface {
	LivenessProbe(*http.Request) error
	// TODO(alberto): change this interface to use unstructured instead of *awsv1beta1.EC2NodeClass.
	// Or NodeClaim, so providers can call resolveNodeClassFromNodeClaim themselves.
	// so it can be satisfied by multiple providers
	List(context.Context, *corev1beta1.KubeletConfiguration, *awsv1beta1.EC2NodeClass) ([]*cloudprovider.InstanceType, error)
	UpdateInstanceTypes(ctx context.Context) error
	UpdateInstanceTypeOfferings(ctx context.Context) error
}

// Providers implement this leverage cloud provider specific pricing APIs to provide pricing information.
type PricingProvider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdateOnDemandPricing(context.Context) error
	UpdateSpotPricing(context.Context) error
}

// Providers implement this to create provider specific MachinePools with CAPI cloud provider.
// e.g awsMachinePool, azureMachinePool, etc.
type InstanceProvider interface {
	Create(context.Context, *corev1beta1.NodeClaim, []*cloudprovider.InstanceType) (*v1.ObjectReference, error)
	Get(context.Context, string) error
	List(context.Context) error
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type CloudProvider struct {
	instanceTypeProvider InstanceTypeProvider
	instanceProvider     InstanceProvider
	recorder             events.Recorder
	kubeClient           client.Client

	// Below this line in unsued by this poc.
	machineProvider           machine.Provider
	machineDeploymentProvider machinedeployment.Provider
}

func (c *CloudProvider) GetSupportedNodeClasses() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{
			Group:   v1beta1.SchemeGroupVersion.Group,
			Version: v1beta1.SchemeGroupVersion.Version,
			Kind:    "EC2NodeClass",
		},
	}
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return c.instanceTypeProvider.LivenessProbe(req)
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *corev1beta1.NodeClaim) (*unstructured.Unstructured, error) {
	nodeClass := &unstructured.Unstructured{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound
	if !nodeClass.GetDeletionTimestamp().IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, fmt.Errorf("nodeClass %s is being deleted", nodeClass.GetName())
	}
	return nodeClass, nil
}

func (c *CloudProvider) Create(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("nodeClaim -> CAPI call to create -> CAPA call to asg/fleet", nodeClaim))

	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		return nil, fmt.Errorf("resolving node class, %w", err)
	}

	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClaim, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving instance types, %w", err)
	}
	if len(instanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all requested instance types were unavailable during launch"))
	}

	providerMachinePoolRef, err := c.instanceProvider.Create(ctx, nodeClaim, instanceTypes)
	if err != nil {
		return nil, fmt.Errorf("creating provider MachinePool, %w", err)
	}

	machinePool := &capiexpv1.MachinePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:            nodeClaim.Name,
			Namespace:       "",
			Labels:          map[string]string{},
			Annotations:     map[string]string{},
			OwnerReferences: []metav1.OwnerReference{},
		},
		// Populate the fields with empty values
		Spec: capiexpv1.MachinePoolSpec{
			ClusterName: "",
			Replicas:    new(int32),
			Template: capiv1.MachineTemplateSpec{
				Spec: capiv1.MachineSpec{
					ClusterName: "",
					Bootstrap:   capiv1.Bootstrap{
						// TODO (alberto) inject OCP user data here.
					},
					InfrastructureRef:       *providerMachinePoolRef,
					Version:                 new(string),
					ProviderID:              new(string),
					FailureDomain:           new(string),
					NodeDrainTimeout:        &metav1.Duration{},
					NodeVolumeDetachTimeout: &metav1.Duration{},
					NodeDeletionTimeout:     &metav1.Duration{},
				},
			},
			MinReadySeconds: new(int32),
			ProviderIDList:  []string{},
			FailureDomains:  []string{},
		},
	}

	if err := c.kubeClient.Create(ctx, machinePool); err != nil {
		return nil, fmt.Errorf("creating MachinePool, %w", err)
	}

	// TODO (alberto): The instanceType could be contractually coming from infraMachinePool.status.InstanceType
	// So we read it via unstructured here for any provider.
	// instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
	// 	return i.Name == string(lo.FromPtr(infraMachinePool.status.InstanceType))
	// })
	//return c.instanceToNodeClaim(machinePool, instanceType)
	return nil, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *v1beta1.NodeClaim) error {
	return fmt.Errorf("not implemented")
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*v1beta1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *CloudProvider) List(ctx context.Context) ([]*v1beta1.NodeClaim, error) {
	// machines, err := c.machineProvider.List(ctx)
	// if err != nil {
	// 	return nil, fmt.Errorf("listing machines, %w", err)
	// }

	// var nodeClaims []*v1beta1.NodeClaim
	// for _, machine := range machines {
	// 	nodeClaim, err := c.machineToNodeClaim(ctx, machine)
	// 	if err != nil {
	// 		return []*v1beta1.NodeClaim{}, err
	// 	}
	// 	nodeClaims = append(nodeClaims, nodeClaim)
	// }

	// return nodeClaims, nil
	return nil, nil
}

// Return the hard-coded instance types.
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *v1beta1.NodePool) ([]*cloudprovider.InstanceType, error) {
	instanceTypes := []*cloudprovider.InstanceType{}

	if nodePool == nil {
		return instanceTypes, fmt.Errorf("node pool reference is nil, no way to proceed")
	}

	// otherwise, get the details from the nodepool to inform which named nodeclass (if present) and other options

	// things to do:
	// - get the infra ref from the node pool
	// - look up the records
	// - build the instance types list
	//   - use status.capacity to inform resources

	return instanceTypes, nil
}

// Return nothing since there's no cloud provider drift.
func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

func (c *CloudProvider) Name() string {
	return "clusterapi"
}

func (c *CloudProvider) machineToNodeClaim(ctx context.Context, machine *capiv1beta1.Machine) (*v1beta1.NodeClaim, error) {
	nodeClaim := v1beta1.NodeClaim{}
	if machine.Spec.ProviderID != nil {
		nodeClaim.Status.ProviderID = *machine.Spec.ProviderID
	}

	// we want to get the MachineDeployment that owns this Machine to read the capacity information.
	// to being this process, we get the MachineDeployment name from the Machine labels.
	mdName, found := machine.GetLabels()[capiv1beta1.MachineDeploymentNameLabel]
	if !found {
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, Machine has no MachineDeployment label %q", machine.GetName(), capiv1beta1.MachineDeploymentNameLabel)
	}
	machineDeployment, err := c.machineDeploymentProvider.Get(ctx, mdName, machine.GetNamespace())
	if err != nil {
		return nil, err
	}

	// machine capacity
	// we are using the scale from zero annotations on the MachineDeployment to make this accessible.
	// TODO (elmiko) improve this once upstream has advanced the state of the art, also add a mechanism
	// to lookup the infra machine template similar to how CAS does it.
	capacity := corev1.ResourceList{}
	cpu, found := machineDeployment.GetAnnotations()[cpuKey]
	if !found {
		// if there is no cpu resource we aren't going to get far, return an error
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, no cpu capacity found on MachineDeployment %q", machine.GetName(), mdName)
	}
	capacity[corev1.ResourceCPU] = resource.MustParse(cpu)

	memory, found := machineDeployment.GetAnnotations()[memoryKey]
	if !found {
		// if there is no memory resource we aren't going to get far, return an error
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, no memory capacity found on MachineDeployment %q", machine.GetName(), mdName)
	}
	capacity[corev1.ResourceMemory] = resource.MustParse(memory)

	// TODO (elmiko) add gpu, maxPods, labels, and taints

	nodeClaim.Status.Capacity = capacity

	return &nodeClaim, nil
}

// Filter out instance types that don't meet the requirements
func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClaim *corev1beta1.NodeClaim, nodeClass *unstructured.Unstructured) ([]*cloudprovider.InstanceType, error) {
	// See comment no InstanceTypeProvider interface re EC2NodeClass.
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClaim.Spec.Kubelet, &awsv1beta1.EC2NodeClass{})
	if err != nil {
		return nil, fmt.Errorf("getting instance types, %w", err)
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	return lo.Filter(instanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			len(i.Offerings.Compatible(reqs).Available()) > 0 &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	}), nil
}
