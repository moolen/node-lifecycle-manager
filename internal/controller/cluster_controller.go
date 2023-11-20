/*
Copyright 2023.

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

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"text/template"
	"time"

	"github.com/wI2L/jsondiff"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/go-logr/logr"
	"github.com/moolen/node-lifecycle-manager/api/v1alpha1"
	ec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	eksv1beta1 "github.com/upbound/provider-aws/apis/eks/v1beta1"
	iamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	utilpointer "k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ClusterReconciler reconciles a NodeGroup object
type ClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	RestConfig    *rest.Config
	DynamicClient *dynamic.DynamicClient
}

var defaultRequeueInterval = time.Second * 30

//+kubebuilder:rbac:groups=eks.nlm.tech,resources=clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=eks.nlm.tech,resources=clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=eks.nlm.tech,resources=clusters/finalizers,verbs=update

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.0/pkg/reconcile
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cluster v1alpha1.Cluster
	err := r.Get(ctx, req.NamespacedName, &cluster)
	if err != nil {
		// deletion will cause downstream deletions
		return ctrl.Result{}, err
	}

	// add finalizer to prevent deletion as it would cause a big disruption
	// users are supposed to clean them up manually
	// TODO: figure out process to "approve" the deletion
	controllerutil.AddFinalizer(&cluster, v1alpha1.FinalizerDeletionProtection)
	if cluster.ObjectMeta.DeletionTimestamp != nil {
		logger.Info("ignoring resource due to deletion timestamp. Please clean up this resource manually by deleting the finalizer")
		return ctrl.Result{}, nil
	}

	// patch status when done processing
	p := client.MergeFrom(cluster.DeepCopy())
	defer func() {
		err = r.Status().Patch(ctx, &cluster, p)
		if err != nil {
			logger.Error(err, "unable to patch status")
		}
	}()

	if requeue, err := r.initialize(ctx, cluster, logger); err != nil || requeue {
		if err != nil {
			logger.Error(err, "unable to initialize cluster")
			r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, err.Error())
		}
		logger.V(1).Info("requeue on initialize")
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, err
	}

	var eksCluster eksv1beta1.Cluster
	err = r.Get(ctx, types.NamespacedName{Name: cluster.Name}, &eksCluster)
	if err != nil {
		logger.Error(err, "unable to get EKS cluster")
		return ctrl.Result{}, err
	}

	var subnetList ec2v1beta1.SubnetList
	err = r.List(ctx, &subnetList, client.MatchingLabels{
		v1alpha1.LabelClusterName: cluster.Name,
	})
	if err != nil {
		logger.Error(err, "unable to list subnets")
		return ctrl.Result{}, err
	}

	candidates := r.getUpdateCandidates(ctx, cluster, eksCluster, subnetList)
	max := cluster.Spec.NodeGroupSpec.UpdateStrategy.RollingUpdate.MaxUnavailable
	if len(candidates) < max {
		max = len(candidates) // slice operator is non-inclusive on the high end
	}

	var requeue bool
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "upgrading launch template / node group")
	for _, candidate := range candidates[0:max] {
		groupName := nodeGroupName(candidate.Group.Name, *candidate.Subnet.Status.AtProvider.AvailabilityZone)
		logger.Info("updating candidate", "candidate", groupName)
		launchTemplate := &ec2v1beta1.LaunchTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name: groupName,
			},
		}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, launchTemplate, func() error {
			return r.updateLaunchTemplate(candidate.Group, cluster, candidate.Subnet.Status.AtProvider.AvailabilityZone, eksCluster, launchTemplate)
		})
		if err != nil {
			r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
			return ctrl.Result{}, err
		}

		// if we created/updated the Kind=LaunchTemplate we must
		// wait for crossplane to synced the resource, see below.
		// At this time, there is no way to figure out when/if crossplane
		// has synced a resource.
		if op != controllerutil.OperationResultNone {
			updateLaunchTemplateLastUpdateTime(groupName, &cluster, metav1.NewTime(time.Now().UTC()))
			requeue = true
			continue
		}
		if !resourceReady(launchTemplate.Status.Conditions) {
			requeue = true
			continue
		}

		// update/verify node group
		nodeGroup := &eksv1beta1.NodeGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: groupName,
			},
		}
		err = r.createOrApply(ctx, nodeGroup, func() error {
			r.updateNodeGroup(candidate.Group, cluster, candidate.Subnet, nodeGroup, *launchTemplate.Status.AtProvider.LatestVersion)
			return nil
		})
		if err != nil {
			r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
			return ctrl.Result{}, err
		}

		// verify that node group uses the latest launch template
		var nodeGroupTemplateMatches bool
		for _, usedTemplate := range nodeGroup.Status.AtProvider.LaunchTemplate {
			logger.Info("compare launch template version", "group", groupName, "used", usedTemplate.Version, "template", launchTemplate.Status.AtProvider.LatestVersion)
			if usedTemplate.Version != nil && launchTemplate.Status.AtProvider.LatestVersion != nil && fmt.Sprintf("%d", int(*launchTemplate.Status.AtProvider.LatestVersion)) == *usedTemplate.Version {
				nodeGroupTemplateMatches = true
				break
			}
		}

		if !resourceReady(nodeGroup.Status.Conditions) {
			requeue = true
			continue
		}

		if !nodeGroupTemplateMatches {
			requeue = true
			continue
		}
	}

	if requeue {
		// requeue time is higher here to allow crossplane to sync up the resource.
		// In particular, the LaunchTemplate must be synced to reflect the new
		// launch template version.
		// if crossplane does not sync the resources within the defined time duration
		// we will likely skip this NodeGroup and continue to upgrade the next one.
		logger.Info("candidates not ready, requeueing")
		return ctrl.Result{RequeueAfter: time.Second * 90}, nil
	}

	if len(candidates) > 0 && len(candidates) > max {
		logger.Info("requeueing remaining to update candidates")
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	logger.Info("cluster is ready")
	r.setReadyCondition(&cluster, v1.ConditionTrue, v1alpha1.ConditionReasonAvailable, "successfully reconciled")
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

type UpdateCandidate struct {
	Group  v1alpha1.NodeGroup
	Subnet ec2v1beta1.Subnet
}

// return a list of node groups who needs an update
func (r *ClusterReconciler) getUpdateCandidates(ctx context.Context, cluster v1alpha1.Cluster, eksCluster eksv1beta1.Cluster, subnetList ec2v1beta1.SubnetList) []UpdateCandidate {
	var candidates []UpdateCandidate
	for _, group := range cluster.Spec.NodeGroupSpec.Groups {
		for _, subnet := range subnetList.Items {
			groupName := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
			// cases:
			// - launch template does not exist
			// - launch template is out of date
			// - launch template not ready
			launchTemplate := &ec2v1beta1.LaunchTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name: groupName,
				},
			}
			if needsUpdate := r.needsUpdate(ctx, launchTemplate, func() error {
				return r.updateLaunchTemplate(group, cluster, subnet.Status.AtProvider.AvailabilityZone, eksCluster, launchTemplate)
			}); needsUpdate {
				candidates = append(candidates, UpdateCandidate{
					Group:  group,
					Subnet: subnet,
				})
				continue
			}
			if !resourceReady(launchTemplate.Status.Conditions) {
				candidates = append(candidates, UpdateCandidate{
					Group:  group,
					Subnet: subnet,
				})
				continue
			}

			// cases:
			// - node group does not exist
			// - node group is out of date
			// - node group not ready
			nodeGroup := &eksv1beta1.NodeGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: groupName,
				},
			}
			if needsUpdate := r.needsUpdate(ctx, nodeGroup, func() error {
				r.updateNodeGroup(group, cluster, subnet, nodeGroup, *launchTemplate.Status.AtProvider.LatestVersion)
				return nil
			}); needsUpdate {
				candidates = append(candidates, UpdateCandidate{
					Group:  group,
					Subnet: subnet,
				})
				continue
			}
			if !resourceReady(nodeGroup.Status.Conditions) {
				candidates = append(candidates, UpdateCandidate{
					Group:  group,
					Subnet: subnet,
				})
				continue
			}

			// cases:
			// - launch template version mismatch
			for _, usedTemplate := range nodeGroup.Status.AtProvider.LaunchTemplate {
				if usedTemplate.Version == nil ||
					launchTemplate.Status.AtProvider.LatestVersion == nil ||
					*usedTemplate.Version != fmt.Sprintf("%d", int(*launchTemplate.Status.AtProvider.LatestVersion)) {
					candidates = append(candidates, UpdateCandidate{
						Group:  group,
						Subnet: subnet,
					})
				}
			}
		}
	}

	// ensure we have stable sorting, otherwise
	sort.SliceStable(candidates, func(i, j int) bool {
		return (candidates[i].Group.Name + *candidates[i].Subnet.Status.AtProvider.AvailabilityZone) <
			(candidates[j].Group.Name + *candidates[j].Subnet.Status.AtProvider.AvailabilityZone)
	})

	return candidates
}

type ClientObject interface {
	metav1.Object
	runtime.Object
	TypeMeta
}

type TypeMeta interface {
	SetGroupVersionKind(gvk schema.GroupVersionKind)
}

func (r ClusterReconciler) needsUpdate(ctx context.Context, obj ClientObject, f controllerutil.MutateFn) bool {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		return true
	}
	existing := obj.DeepCopyObject()
	if err := mutate(f, key, obj); err != nil {
		return true
	}
	return !equality.Semantic.DeepEqual(existing, obj)
}

func (r ClusterReconciler) createOrApply(ctx context.Context, obj ClientObject, f controllerutil.MutateFn) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := mutate(f, key, obj); err != nil {
			return err
		}
		if err := r.Create(ctx, obj); err != nil {
			return err
		}
		return nil
	}

	existing := obj.DeepCopyObject()
	if err := mutate(f, key, obj); err != nil {
		return err
	}

	// GVK is missing, see:
	// https://github.com/kubernetes-sigs/controller-runtime/issues/526
	// https://github.com/kubernetes-sigs/controller-runtime/issues/1517
	// https://github.com/kubernetes/kubernetes/issues/80609
	// we need to manually set it before doing a Patch() as it depends on the GVK
	gvks, unversioned, err := r.Scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}
	if !unversioned && len(gvks) == 1 {
		obj.SetGroupVersionKind(gvks[0])
	}
	if equality.Semantic.DeepEqual(existing, obj) {
		return nil
	}
	patch, _ := jsondiff.Compare(existing, obj)
	b, _ := json.MarshalIndent(patch, "", "    ")
	fmt.Println("----")
	fmt.Println(obj.GetObjectKind().GroupVersionKind().String() + "|" + obj.GetName())
	fmt.Println(string(b))
	fmt.Println("----")
	// reset managed fields before calling .Patch()
	obj.SetManagedFields(nil)
	if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(v1alpha1.FieldOwner), client.ForceOwnership); err != nil {
		return err
	}
	return nil
}

func mutate(f controllerutil.MutateFn, key client.ObjectKey, obj client.Object) error {
	if err := f(); err != nil {
		return err
	}
	if newKey := client.ObjectKeyFromObject(obj); key != newKey {
		return fmt.Errorf("MutateFn cannot mutate object name and/or object namespace")
	}
	return nil
}

// Initialize creates the seed resources everything depends upon:
// Kind=Cluster (eks.aws.upbound.io)
// Kind=Role (iam.aws.upbound.io)
// Kind=Subnet (ec2.aws.upbound.io)
func (r *ClusterReconciler) initialize(ctx context.Context, cluster v1alpha1.Cluster, logger logr.Logger) (bool, error) {
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, "creating init resources")
	eksCluster := &eksv1beta1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name,
			Annotations: map[string]string{
				"crossplane.io/external-name": cluster.Name,
			},
		},
	}
	err := r.createOrApply(ctx, eksCluster, func() error {
		r.updateEKSCluster(cluster, eksCluster)
		return nil
	})
	if err != nil {
		return true, fmt.Errorf("unable to create/update cluster %q: %w", cluster.Name, err)
	}

	if !resourceReady(eksCluster.Status.Conditions) {
		return true, nil
	}

	// create subnets
	var someSubnetNotSynced bool
	for _, vpcConfig := range eksCluster.Status.AtProvider.VPCConfig {
		for _, subnet := range vpcConfig.SubnetIds {
			eksSubnet := &ec2v1beta1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name: *subnet,
					Annotations: map[string]string{
						"crossplane.io/external-name": *subnet,
					},
				},
			}
			err = r.createOrApply(ctx, eksSubnet, func() error {
				r.updateSubnet(*subnet, cluster, eksSubnet)
				return nil
			})
			if err != nil {
				logger.Error(err, "unable to create/update subnet resource")
				return true, err
			}
			if !resourceReady(eksSubnet.Status.Conditions) {
				someSubnetNotSynced = true
			}
		}
	}

	if someSubnetNotSynced {
		logger.Info("waiting for subnets to be ready")
		return true, nil
	}

	workerIamRole := &iamv1beta1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-eks-worker-node", cluster.Name),
		},
	}
	err = r.createOrApply(ctx, workerIamRole, func() error {
		r.updateWorkerIamRole(cluster, workerIamRole)
		return nil
	})
	if err != nil {
		return true, fmt.Errorf("unable to create/update worker iam role: %w", err)
	}

	if !resourceReady(workerIamRole.Status.Conditions) {
		return true, nil
	}

	// TODO: create custom SG + rules if needed

	return false, nil
}

func (r *ClusterReconciler) updateEKSCluster(cluster v1alpha1.Cluster, eksCluster *eksv1beta1.Cluster) {
	controllerutil.SetControllerReference(&cluster, &eksCluster.ObjectMeta, r.Scheme)

	// note: we must set the keys individually, we should not directly assign .Spec,
	//       otherwise values provided by crossplane would be overridden with the default value.
	//       We would end up in a case where both controllers fight over the .Spec values.
	//       As a matter of fact, crossplane will likely fail to update the status
	//       due to a resource conflict.
	eksCluster.Annotations = map[string]string{
		"crossplane.io/external-name": cluster.Name,
	}
	eksCluster.Labels = map[string]string{
		v1alpha1.LabelClusterName: cluster.Name,
	}
	eksCluster.Spec.ResourceSpec.ManagementPolicies = crossplanecommonv1.ManagementPolicies{
		crossplanecommonv1.ManagementActionObserve,
	}
	eksCluster.Spec.ForProvider.Region = &cluster.Spec.Region
}

func (r *ClusterReconciler) updateSubnet(name string, cluster v1alpha1.Cluster, subnet *ec2v1beta1.Subnet) {
	controllerutil.SetControllerReference(&cluster, &subnet.ObjectMeta, r.Scheme)

	// note: we must set the keys individually, we should not directly assign .Spec,
	//       otherwise values provided by crossplane would be overridden with the default value.
	//       We would end up in a case where both controllers fight over the .Spec values.
	//       As a matter of fact, crossplane will likely fail to update the status
	//       due to a resource conflict.
	subnet.ObjectMeta.Labels = map[string]string{
		v1alpha1.LabelClusterName: cluster.Name,
	}
	subnet.ObjectMeta.Annotations = map[string]string{
		"crossplane.io/external-name": name,
	}
	subnet.Spec.ForProvider.Region = &cluster.Spec.Region
	subnet.Spec.ResourceSpec.ManagementPolicies = crossplanecommonv1.ManagementPolicies{
		crossplanecommonv1.ManagementActionObserve,
	}
}

// note: user-data must start with either `#!` or `content-type`
// https://cloudinit.readthedocs.io/en/latest/explanation/format.html#user-data-script
const launchTemplateUserDataTpl = `#!/bin/bash
set -ex

/etc/eks/bootstrap.sh {{ .cluster_name }} \
  --b64-cluster-ca {{ .cluster_ca_data }} \
  --apiserver-endpoint {{ .cluster_endpoint }}
`

func (r *ClusterReconciler) updateLaunchTemplate(group v1alpha1.NodeGroup, cluster v1alpha1.Cluster, availabilityZone *string, eksCluster eksv1beta1.Cluster, launchTemplate *ec2v1beta1.LaunchTemplate) error {
	controllerutil.SetControllerReference(&cluster, &launchTemplate.ObjectMeta, r.Scheme)
	name := nodeGroupName(group.Name, *availabilityZone)

	// note: we must set the keys individually, we should not directly assign .Spec,
	//       otherwise values provided by crossplane would be overridden with the default value.
	//       We would end up in a case where both controllers fight over the .Spec values.
	//       As a matter of fact, crossplane will likely fail to update the status
	//       due to a resource conflict.
	launchTemplate.ObjectMeta.Labels = map[string]string{
		v1alpha1.LabelClusterName: cluster.Name,
	}

	// TODO: consider exposing block device configuration via Cluster.spec.nodeGroupSpec.groups[]
	launchTemplate.Spec.ForProvider.BlockDeviceMappings = []ec2v1beta1.BlockDeviceMappingsParameters{
		{
			DeviceName: utilpointer.String("/dev/sda1"),
			EBS: []ec2v1beta1.EBSParameters{
				{
					VolumeSize: utilpointer.Float64(40),
				},
			},
		},
	}
	launchTemplate.Spec.ForProvider.MetadataOptions = []ec2v1beta1.LaunchTemplateMetadataOptionsParameters{
		{
			HTTPEndpoint:            utilpointer.String("enabled"),
			HTTPPutResponseHopLimit: utilpointer.Float64(1),
			HTTPTokens:              utilpointer.String("required"),
			InstanceMetadataTags:    utilpointer.String("enabled"),
		},
	}
	launchTemplate.Spec.ForProvider.TagSpecifications = []ec2v1beta1.TagSpecificationsParameters{
		{
			ResourceType: utilpointer.String("instance"),
			Tags: map[string]*string{
				"Name": utilpointer.String(group.Name),
			},
		},
	}
	launchTemplate.Spec.ForProvider.Region = &cluster.Spec.Region
	launchTemplate.Spec.ForProvider.ImageID = utilpointer.String(group.AMI)
	launchTemplate.Spec.ForProvider.Name = utilpointer.String(name)
	launchTemplate.Spec.ForProvider.Placement = []ec2v1beta1.PlacementParameters{
		{
			AvailabilityZone: availabilityZone,
		},
	}
	launchTemplate.Spec.ForProvider.VPCSecurityGroupIds = append(
		eksCluster.Status.AtProvider.VPCConfig[0].SecurityGroupIds,
		eksCluster.Status.AtProvider.VPCConfig[0].ClusterSecurityGroupID,
	)

	// user-data
	tpl, err := template.New("user-data").Parse(launchTemplateUserDataTpl)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	err = tpl.Execute(&buf, map[string]string{
		"cluster_name":     cluster.Name,
		"cluster_ca_data":  *eksCluster.Status.AtProvider.CertificateAuthority[0].Data,
		"cluster_endpoint": *eksCluster.Status.AtProvider.Endpoint,
	})
	if err != nil {
		return err
	}
	launchTemplate.Spec.ForProvider.UserData = utilpointer.String(base64.StdEncoding.EncodeToString(buf.Bytes()))
	return nil
}

var (
	assumeRolePolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Sid": "EKSNodeAssumeRole",
			"Effect": "Allow",
			"Principal": {
				"Service": "ec2.amazonaws.com"
			},
			"Action": "sts:AssumeRole"
		}
	]
}`

	ecrReadPolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Effect": "Allow",
			"Action": [
				"ecr:GetAuthorizationToken",
				"ecr:BatchCheckLayerAvailability",
				"ecr:GetDownloadUrlForLayer",
				"ecr:GetRepositoryPolicy",
				"ecr:DescribeRepositories",
				"ecr:ListImages",
				"ecr:DescribeImages",
				"ecr:BatchGetImage",
				"ecr:GetLifecyclePolicy",
				"ecr:GetLifecyclePolicyPreview",
				"ecr:ListTagsForResource",
				"ecr:DescribeImageScanFindings"
			],
			"Resource": "*"
		}
	]
}`

	cniPolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Effect": "Allow",
			"Action": [
				"ec2:AssignPrivateIpAddresses",
				"ec2:AttachNetworkInterface",
				"ec2:CreateNetworkInterface",
				"ec2:DeleteNetworkInterface",
				"ec2:DescribeInstances",
				"ec2:DescribeTags",
				"ec2:DescribeNetworkInterfaces",
				"ec2:DescribeInstanceTypes",
				"ec2:DetachNetworkInterface",
				"ec2:ModifyNetworkInterfaceAttribute",
				"ec2:UnassignPrivateIpAddresses"
			],
			"Resource": "*"
		},
		{
			"Effect": "Allow",
			"Action": [
				"ec2:CreateTags"
			],
			"Resource": [
				"arn:aws:ec2:*:*:network-interface/*"
			]
		}
	]
}`

	workerNodePolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Effect": "Allow",
			"Action": [
				"ec2:DescribeInstances",
				"ec2:DescribeInstanceTypes",
				"ec2:DescribeRouteTables",
				"ec2:DescribeSecurityGroups",
				"ec2:DescribeSubnets",
				"ec2:DescribeVolumes",
				"ec2:DescribeVolumesModifications",
				"ec2:DescribeVpcs",
				"eks:DescribeCluster"
			],
			"Resource": "*"
		}
	]
}`
)

func (r *ClusterReconciler) updateWorkerIamRole(cluster v1alpha1.Cluster, role *iamv1beta1.Role) {
	controllerutil.SetControllerReference(&cluster, &role.ObjectMeta, r.Scheme)

	// note: we must set the keys individually, we should not directly assign .Spec,
	//       otherwise values provided by crossplane would be overridden with the default value.
	//       We would end up in a case where both controllers fight over the .Spec values.
	//       As a matter of fact, crossplane will likely fail to update the status
	//       due to a resource conflict.

	role.Spec.ForProvider.AssumeRolePolicy = &assumeRolePolicy
	role.Spec.ForProvider.InlinePolicy = []iamv1beta1.InlinePolicyParameters{
		{
			Name:   utilpointer.String("ecr-read"),
			Policy: &ecrReadPolicy,
		},
		{
			Name:   utilpointer.String("cni"),
			Policy: &cniPolicy,
		},
		{
			Name:   utilpointer.String("worker-node"),
			Policy: &workerNodePolicy,
		},
	}
	// note: the Kind=NodeGroup references this via Spec.NodeRoleArnSelector
	//       hence the LabelNodeIAMRole label.
	role.ObjectMeta.Labels = map[string]string{
		v1alpha1.LabelClusterName: cluster.Name,
		v1alpha1.LabelNodeIAMRole: "",
	}
}

func nodeGroupName(groupName, availabilityZone string) string {
	return fmt.Sprintf("%s-%s", groupName, availabilityZone)
}

func (r *ClusterReconciler) updateNodeGroup(
	group v1alpha1.NodeGroup,
	cluster v1alpha1.Cluster,
	subnet ec2v1beta1.Subnet,
	nodeGroup *eksv1beta1.NodeGroup,
	desiredLTVersion float64) {
	name := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
	controllerutil.SetControllerReference(&cluster, &nodeGroup.ObjectMeta, r.Scheme)

	// note: we must set the keys individually, we should not directly assign .Spec,
	//       otherwise values provided by crossplane would be overridden with the default value.
	//       We would end up in a case where both controllers fight over the .Spec values.
	//       As a matter of fact, crossplane will likely fail to update the status
	//       due to a resource conflict.
	if nodeGroup.ObjectMeta.Annotations == nil {
		nodeGroup.ObjectMeta.Annotations = make(map[string]string)
	}
	nodeGroup.ObjectMeta.Annotations["launch-template-version"] = strconv.FormatFloat(desiredLTVersion, 'f', 0, 64)
	nodeGroup.Spec.DeletionPolicy = crossplanecommonv1.DeletionDelete
	nodeGroup.Spec.ManagementPolicies = crossplanecommonv1.ManagementPolicies{
		crossplanecommonv1.ManagementActionAll,
	}
	nodeGroup.Spec.ForProvider.Region = &cluster.Spec.Region
	nodeGroup.Spec.ForProvider.ReleaseVersion = nil
	nodeGroup.Spec.ForProvider.ScalingConfig = []eksv1beta1.ScalingConfigParameters{
		{
			DesiredSize: &group.DesiredSize,
			MinSize:     &group.MinSize,
			MaxSize:     &group.MaxSize,
		},
	}
	nodeGroup.Spec.ForProvider.AMIType = utilpointer.String("CUSTOM")
	nodeGroup.Spec.ForProvider.ClusterNameRef = &crossplanecommonv1.Reference{
		Name: cluster.Name,
	}

	nodeGroup.Spec.ForProvider.SubnetIds = []*string{subnet.Status.AtProvider.ID}
	nodeGroup.Spec.ForProvider.NodeRoleArnSelector = &crossplanecommonv1.Selector{
		MatchLabels: map[string]string{
			v1alpha1.LabelClusterName: cluster.Name,
			v1alpha1.LabelNodeIAMRole: "",
		},
	}
	nodeGroup.Spec.ForProvider.LaunchTemplate = []eksv1beta1.LaunchTemplateParameters{
		{
			Name:    utilpointer.String(name),
			Version: utilpointer.String(fmt.Sprintf("%d", int(desiredLTVersion))),
		},
	}
	nodeGroup.Spec.ForProvider.UpdateConfig = []eksv1beta1.UpdateConfigParameters{
		{
			MaxUnavailablePercentage: utilpointer.Float64(33),
		},
	}
	nodeGroup.Spec.ForProvider.Labels = map[string]*string{
		v1alpha1.LabelNodePool: utilpointer.String(name),
	}
}

func (r *ClusterReconciler) setReadyCondition(cluster *v1alpha1.Cluster, status v1.ConditionStatus, reason, message string) {
	provisioning := NewClusterCondition(v1alpha1.ConditionTypeReady, status, reason, message)
	SetClusterCondition(cluster, *provisioning)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Cluster{}).
		Complete(r)
}
