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
	"context"
	"fmt"
	"time"

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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	utilpointer "k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ClusterReconciler reconciles a NodeGroup object
type ClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	RestConfig    *rest.Config
	DynamicClient *dynamic.DynamicClient
}

var defaultRequeueInterval = time.Second * 15

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
		// TODO: delete node pools?! deletionPolicy?
		return ctrl.Result{}, err
	}
	// patch status when done processing
	p := client.MergeFrom(cluster.DeepCopy())
	defer func() {
		err = r.Status().Patch(ctx, &cluster, p)
		if err != nil {
			logger.Error(err, "unable to patch status")
		}
	}()

	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, "initializing basic cluster resources")
	if requeue, err := r.initialize(ctx, cluster, logger); err != nil || requeue {
		if err != nil {
			logger.Error(err, "unable to initialize cluster")
			r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, err.Error())
		}
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, err
	}

	var subnetList ec2v1beta1.SubnetList
	err = r.List(ctx, &subnetList, client.MatchingLabels{
		v1alpha1.LabelClusterName: cluster.Name,
	})
	if err != nil {
		logger.Error(err, "unable to list subnets")
		return ctrl.Result{}, err
	}

	// fetch node pools
	nodeGroupMap := make(map[string]*eksv1beta1.NodeGroup)
	for _, group := range cluster.Spec.NodeGroupSpec.Groups {
		for _, subnet := range subnetList.Items {
			var nodeGroup eksv1beta1.NodeGroup
			name := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
			err = r.Client.Get(ctx, types.NamespacedName{Name: name}, &nodeGroup)
			if !apierrors.IsNotFound(err) {
				nodeGroupMap[name] = &nodeGroup
			}
		}
	}

	// fetch launch template
	launchTemplateMap := make(map[string]*ec2v1beta1.LaunchTemplate)
	for _, group := range cluster.Spec.NodeGroupSpec.Groups {
		for _, subnet := range subnetList.Items {
			var launchTemplate ec2v1beta1.LaunchTemplate
			name := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
			err = r.Client.Get(ctx, types.NamespacedName{Name: name}, &launchTemplate)
			if !apierrors.IsNotFound(err) {
				launchTemplateMap[name] = &launchTemplate
			}
		}
	}

	// type=Upgrading
	// create missing launch templates
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "waiting for launch templates to be up to date and healthy")
	for _, group := range cluster.Spec.NodeGroupSpec.Groups {
		for _, subnet := range subnetList.Items {
			groupName := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
			// if missing: create & requeue
			launchTemplate, ok := launchTemplateMap[groupName]
			if !ok {
				lt := makeLaunchTemplate(group, cluster, subnet.Status.AtProvider.AvailabilityZone)
				logger.Info("creating launch template and requeue", "name", groupName)
				err = r.Create(ctx, lt)
				if err != nil {
					r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}

			// TODO: factor update out into own method
			newLaunchTemplate := makeLaunchTemplate(group, cluster, subnet.Status.AtProvider.AvailabilityZone)
			launchTemplate.Spec = newLaunchTemplate.Spec
			launchTemplate.ObjectMeta.Labels = newLaunchTemplate.ObjectMeta.Labels
			launchTemplate.ObjectMeta.Annotations = newLaunchTemplate.ObjectMeta.Annotations
			if !equality.Semantic.DeepEqual(launchTemplate.Spec, newLaunchTemplate.Spec) {
				err = r.Update(ctx, launchTemplate)
				if err != nil {
					logger.Error(err, "unable to update launch template")
					r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
					return ctrl.Result{}, err
				}
				logger.Info("requeue after launch template update")
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}

			if !crossplaneResourceReady(launchTemplate.Status.Conditions) {
				logger.Info("launch template not ready, requeueing", "name", groupName)
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}
		}
	}

	// create missing node group, update existing ones
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "waiting for node groups to be healthy and up to date")
	for _, group := range cluster.Spec.NodeGroupSpec.Groups {
		for _, subnet := range subnetList.Items {
			groupName := nodeGroupName(group.Name, *subnet.Status.AtProvider.AvailabilityZone)
			// if missing: create & requeue
			nodeGroup, ok := nodeGroupMap[groupName]
			if !ok {
				// create
				logger.Info("creating node group and requeue", "group", groupName)
				ng := makeNodeGroup(groupName, cluster, subnet.Status.AtProvider.ID)
				err = r.Create(ctx, ng)
				if err != nil {
					r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}

			// TODO: factor update out into own method
			newNodeGroup := makeNodeGroup(groupName, cluster, subnet.Status.AtProvider.ID)
			nodeGroup.Spec = newNodeGroup.Spec
			nodeGroup.ObjectMeta.Labels = newNodeGroup.ObjectMeta.Labels
			nodeGroup.ObjectMeta.Annotations = newNodeGroup.ObjectMeta.Annotations
			if !equality.Semantic.DeepEqual(nodeGroup.Spec, newNodeGroup.Spec) {
				err = r.Update(ctx, nodeGroup)
				if err != nil {
					logger.Error(err, "unable to update node group")
					r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
					return ctrl.Result{}, err
				}
				logger.Info("requeue after node group update")
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}

			// verify that node group uses the latest launch template
			var nodeGroupTemplateMatches bool
			for _, usedTemplate := range nodeGroup.Status.AtProvider.LaunchTemplate {
				tplState, ok := launchTemplateMap[groupName]
				if !ok {
					logger.Info("creating node group and requeue", "group", groupName)
					return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
				}
				logger.Info("compare launch template version", "ng-used", usedTemplate, "template", tplState.Status.AtProvider)
				if usedTemplate.Version != nil && tplState.Status.AtProvider.LatestVersion != nil && fmt.Sprintf("%d", int(*tplState.Status.AtProvider.LatestVersion)) == *usedTemplate.Version {
					nodeGroupTemplateMatches = true
					break
				}
			}

			if !crossplaneResourceReady(nodeGroup.Status.Conditions) {
				logger.Info("node group not ready, requeueing", "name", nodeGroup.Name)
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}

			if !nodeGroupTemplateMatches {
				logger.Info("node group launch template doesn't match, requeueing", "name", nodeGroup.Name)
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}
		}

	}

	// done
	r.setReadyCondition(&cluster, v1.ConditionTrue, v1alpha1.ConditionReasonAvailable, "successfully reconciled")
	return ctrl.Result{}, nil
}

func (r *ClusterReconciler) initialize(ctx context.Context, cluster v1alpha1.Cluster, logger logr.Logger) (bool, error) {
	// seed resources everything depends upon:
	// Kind=Cluster (eks.aws.upbound.io)
	// Kind=Role (iam.aws.upbound.io)
	// Kind=Subnet (ec2.aws.upbound.io)
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, "creating init resources")
	eksCluster := makeEKSCluster(cluster)
	err := r.Get(ctx, client.ObjectKeyFromObject(eksCluster), eksCluster)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, eksCluster)
		if err != nil {
			logger.Error(err, "unable to create eks cluster resource")
			return true, err
		}
	}
	// TODO: factor update out into own method
	updatedCluster := makeEKSCluster(cluster)
	eksCluster.Spec = updatedCluster.Spec
	eksCluster.ObjectMeta.Labels = updatedCluster.Labels
	eksCluster.ObjectMeta.Annotations = updatedCluster.Annotations
	err = r.Update(ctx, eksCluster)
	if err != nil {
		return true, fmt.Errorf("unable to update cluster %q: %w", cluster.Name, err)
	}

	if !crossplaneResourceReady(eksCluster.Status.Conditions) {
		return true, nil
	}

	// create subnets
	var someSubnetNotSynced bool
	for _, vpcConfig := range eksCluster.Status.AtProvider.VPCConfig {
		for _, subnet := range vpcConfig.SubnetIds {
			eksSubnet := makeSubnet(*subnet, cluster)
			err = r.Get(ctx, types.NamespacedName{Name: *subnet}, eksSubnet)
			if apierrors.IsNotFound(err) {
				err = r.Create(ctx, eksSubnet)
				if err != nil {
					logger.Error(err, "unable to create subnet resource")
					return true, err
				}
			}
			// TODO: factor update out into own method
			updatedSubnet := makeSubnet(*subnet, cluster)
			eksSubnet.Spec = updatedSubnet.Spec
			eksSubnet.ObjectMeta.Labels = updatedSubnet.ObjectMeta.Labels
			eksSubnet.ObjectMeta.Annotations = updatedSubnet.ObjectMeta.Annotations
			err = r.Update(ctx, eksSubnet)
			if err != nil {
				return true, fmt.Errorf("unable to update subnet %q: %w", *subnet, err)
			}
			if !crossplaneResourceSynced(eksSubnet.Status.Conditions) {
				someSubnetNotSynced = true
			}
		}
	}

	if someSubnetNotSynced {
		logger.Info("waiting for subnets to be ready")
		return true, nil
	}

	workerIamRole := makeWorkerIamRole(cluster)
	err = r.Get(ctx, client.ObjectKeyFromObject(workerIamRole), workerIamRole)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, workerIamRole)
		if err != nil {
			return true, fmt.Errorf("unable to create worker iam role: %w", err)
		}
	}
	// TODO: factor update out into own method
	updatedWorkerIamRole := makeWorkerIamRole(cluster)
	workerIamRole.Spec = updatedWorkerIamRole.Spec
	workerIamRole.ObjectMeta.Labels = updatedWorkerIamRole.ObjectMeta.Labels
	workerIamRole.ObjectMeta.Annotations = updatedWorkerIamRole.ObjectMeta.Annotations
	err = r.Update(ctx, workerIamRole)
	if err != nil {
		return true, fmt.Errorf("unable to update worker iam role: %w", err)
	}

	if !crossplaneResourceReady(workerIamRole.Status.Conditions) {
		return true, nil
	}
	return false, nil
}

func makeEKSCluster(cluster v1alpha1.Cluster) *eksv1beta1.Cluster {
	return &eksv1beta1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name,
			Annotations: map[string]string{
				"crossplane.io/external-name": cluster.Name,
			},
			Labels: map[string]string{
				v1alpha1.LabelClusterName: cluster.Name,
			},
		},
		Spec: eksv1beta1.ClusterSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ManagementPolicies: crossplanecommonv1.ManagementPolicies{
					crossplanecommonv1.ManagementActionObserve,
				},
			},
			ForProvider: eksv1beta1.ClusterParameters{
				Region: &cluster.Spec.Region,
			},
		},
	}
}

func makeSubnet(name string, cluster v1alpha1.Cluster) *ec2v1beta1.Subnet {
	return &ec2v1beta1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"crossplane.io/external-name": name,
			},
			Labels: map[string]string{
				v1alpha1.LabelClusterName: cluster.Name,
			},
		},
		Spec: ec2v1beta1.SubnetSpec{
			ForProvider: ec2v1beta1.SubnetParameters_2{
				Region: &cluster.Spec.Region,
			},
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ManagementPolicies: crossplanecommonv1.ManagementPolicies{
					crossplanecommonv1.ManagementActionObserve,
				},
			},
		},
	}
}

func makeLaunchTemplate(group v1alpha1.NodeGroup, cluster v1alpha1.Cluster, availabilityZone *string) *ec2v1beta1.LaunchTemplate {
	name := nodeGroupName(group.Name, *availabilityZone)
	launchTemplate := &ec2v1beta1.LaunchTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1alpha1.LabelClusterName: cluster.Name,
			},
		},
		Spec: cluster.Spec.NodeGroupSpec.Template.LaunchTemplateSpec,
	}

	launchTemplate.Spec.ForProvider.Region = &cluster.Spec.Region
	launchTemplate.Spec.ForProvider.ImageID = utilpointer.String(group.AMI)
	launchTemplate.Spec.ForProvider.Name = utilpointer.String(name)
	launchTemplate.Spec.ForProvider.Placement = []ec2v1beta1.PlacementParameters{
		{
			AvailabilityZone: availabilityZone,
		},
	}

	// TODO: create or import?
	launchTemplate.Spec.ForProvider.VPCSecurityGroupIds = []*string{}

	return launchTemplate
}

func makeWorkerIamRole(cluster v1alpha1.Cluster) *iamv1beta1.Role {
	assumeRolePolicy := `{
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

	ecrReadPolicy := `{
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

	cniPolicy := `{
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

	workerNodePolicy := `{
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

	return &iamv1beta1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-eks-worker-node", cluster.Name),
			Labels: map[string]string{
				v1alpha1.LabelClusterName: cluster.Name,
				v1alpha1.LabelNodeIAMRole: "",
			},
		},
		Spec: iamv1beta1.RoleSpec{
			ForProvider: iamv1beta1.RoleParameters{
				AssumeRolePolicy: &assumeRolePolicy,
				InlinePolicy: []iamv1beta1.InlinePolicyParameters{
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
				},
			},
		},
	}
}

func nodeGroupName(groupName, availabilityZone string) string {
	return fmt.Sprintf("%s-%s", groupName, availabilityZone)
}

func makeNodeGroup(name string, cluster v1alpha1.Cluster, subnetId *string) *eksv1beta1.NodeGroup {
	nodeGroup := &eksv1beta1.NodeGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: cluster.Spec.NodeGroupSpec.Template.NodeGroupSpec,
	}

	nodeGroup.Spec.ForProvider.Region = &cluster.Spec.Region
	nodeGroup.Spec.ForProvider.ClusterNameRef = &crossplanecommonv1.Reference{
		Name: cluster.Name,
	}

	nodeGroup.Spec.ForProvider.SubnetIds = []*string{subnetId}
	nodeGroup.Spec.ForProvider.NodeRoleArnSelector = &crossplanecommonv1.Selector{
		MatchLabels: map[string]string{
			v1alpha1.LabelClusterName: cluster.Name,
			v1alpha1.LabelNodeIAMRole: "",
		},
	}

	nodeGroup.Spec.ForProvider.LaunchTemplate = []eksv1beta1.LaunchTemplateParameters{
		{
			Name:    utilpointer.String(name),
			Version: utilpointer.String("$Latest"),
		},
	}

	if nodeGroup.Spec.ForProvider.Labels == nil {
		nodeGroup.Spec.ForProvider.Labels = make(map[string]*string)
	}

	nodeGroup.Spec.ForProvider.Labels[v1alpha1.LabelNodePool] = utilpointer.String(name)

	return nodeGroup
}

func crossplaneResourceReady(conds []crossplanecommonv1.Condition) bool {
	for _, cond := range conds {
		if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func crossplaneResourceSynced(conds []crossplanecommonv1.Condition) bool {
	for _, cond := range conds {
		if cond.Type == crossplanecommonv1.TypeSynced && cond.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
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
