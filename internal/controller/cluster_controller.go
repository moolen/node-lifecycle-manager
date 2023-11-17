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
	v1alpha1 "github.com/moolen/node-lifecycle-manager/api/v1alpha1"
	ec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	eksv1beta1 "github.com/upbound/provider-aws/apis/eks/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
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

	// fetch node pools
	nodeGroupMap := make(map[string]*eksv1beta1.NodeGroup)
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		var nodeGroup eksv1beta1.NodeGroup
		err = r.Client.Get(ctx, types.NamespacedName{Name: pool.Name}, &nodeGroup)
		if !apierrors.IsNotFound(err) {
			nodeGroupMap[pool.Name] = &nodeGroup
		}
		if err != nil {
			logger.Error(err, "unable to get node group")
			return ctrl.Result{}, err
		}
	}

	// fetch launch template
	launchTemplateMap := make(map[string]*ec2v1beta1.LaunchTemplate)
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		var launchTemplate ec2v1beta1.LaunchTemplate
		err = r.Client.Get(ctx, types.NamespacedName{Name: pool.Name}, &launchTemplate)
		if !apierrors.IsNotFound(err) {
			launchTemplateMap[pool.Name] = &launchTemplate
		}
		if err != nil {
			logger.Error(err, "unable to get launch template")
			return ctrl.Result{}, err
		}
	}

	// seed resources everything depends upon:
	// Kind=Cluster (eks.aws.upbound.io)
	// Kind=Role (iam.aws.upbound.io)
	// Kind=Subnet (ec2.aws.upbound.io)
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonInitializing, "creating init resources")
	eksCluster := &eksv1beta1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name,
			Annotations: map[string]string{
				"crossplane.io/external-name": cluster.Name,
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
	err = r.Get(ctx, client.ObjectKeyFromObject(eksCluster), eksCluster)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, eksCluster)
		if err != nil {
			logger.Error(err, "unable to create eks cluster resource")
			return ctrl.Result{}, err
		}
	}

	// wait for it being ready
	var eksClusterReady bool
	for _, cond := range eksCluster.Status.Conditions {
		if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
			eksClusterReady = true
		}
	}
	if !eksClusterReady {
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	// create subnets
	var someSubnetNotReady bool
	for _, vpcConfig := range eksCluster.Status.AtProvider.VPCConfig {
		for _, subnet := range vpcConfig.SubnetIds {
			eksSubnet := &ec2v1beta1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name: *subnet,
					Annotations: map[string]string{
						"crossplane.io/external-name": *subnet,
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
			err = r.Get(ctx, types.NamespacedName{Name: *subnet}, eksSubnet)
			if apierrors.IsNotFound(err) {
				err = r.Create(ctx, eksSubnet)
				if err != nil {
					logger.Error(err, "unable to create subnet resource")
					return ctrl.Result{}, err
				}
			}

			if len(eksSubnet.Status.Conditions) == 0 {
				someSubnetNotReady = true
			}
			for _, cond := range eksSubnet.Status.Conditions {
				if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
					continue
				}
				someSubnetNotReady = true
			}
		}
	}

	if someSubnetNotReady {
		logger.Info("waiting for subnets to be ready")
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	// TODO: create iam role
	//       this must be created manually
	// iamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	//
	// IAMRole := &iamv1beta1.Role{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		Name: cluster.Name,
	// 	},
	// }

	// type=Upgrading
	// create missing launch templates
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "waiting for launch templates to be up to date and healthy")
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		// if missing: create & requeue
		launchTemplate, ok := launchTemplateMap[pool.Name]
		if !ok {
			// create
			lt := &ec2v1beta1.LaunchTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name: pool.Name,
				},
				Spec: cluster.Spec.NodeGroupSpec.Template.LaunchTemplateSpec,
			}
			err = r.Create(ctx, lt)
			if err != nil {
				logger.Info("creating launch template and requeue", "name", pool.Name)
				r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, err
			}
		}

		// check status, continue to next if it's ready & available
		var launchTemplateReady bool
		for _, cond := range launchTemplate.Status.Conditions {
			if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
				launchTemplateReady = true
			}
		}

		if !launchTemplateReady {
			logger.Info("launch template not ready, requeueing", "name", pool.Name)
			// requeue item
			return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
		}
	}

	// create missing node pools
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "waiting for node groups to be healthy and up to date")
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		// if missing: create & requeue
		nodeGroup, ok := nodeGroupMap[pool.Name]
		if !ok {
			// create
			ng := &eksv1beta1.NodeGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: pool.Name,
				},
				Spec: cluster.Spec.NodeGroupSpec.Template.NodeGroupSpec,
			}
			err = r.Create(ctx, ng)
			if err != nil {
				logger.Info("creating node group and requeue", "group", pool.Name)
				r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, err
			}
		}

		// check status, continue to next if it's ready & available
		var nodeGroupReady bool
		for _, cond := range nodeGroup.Status.Conditions {
			if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
				nodeGroupReady = true
				break
			}
		}

		// verify that node group uses the latest launch template
		var nodeGroupTemplateMatches bool
		for _, usedTemplate := range nodeGroup.Status.AtProvider.LaunchTemplate {
			tplState, ok := launchTemplateMap[pool.Name]
			if !ok {
				logger.Info("creating node group and requeue", "group", pool.Name)
				return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
			}
			logger.Info("compare launch template version", "ng-used", usedTemplate, "template", tplState.Status.AtProvider)
			if usedTemplate.Version != nil && tplState.Status.AtProvider.LatestVersion != nil && fmt.Sprintf("%d", int(*tplState.Status.AtProvider.LatestVersion)) == *usedTemplate.Version {
				nodeGroupTemplateMatches = true
				break
			}
		}

		if !nodeGroupReady {
			logger.Info("node group not ready, requeueing", "name", nodeGroup.Name)
			return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
		}

		if !nodeGroupTemplateMatches {
			logger.Info("node group launch template doesn't match, requeueing", "name", nodeGroup.Name)
			return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
		}
	}

	// type=Upgrading
	// reconcile node group spec
	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "launch template spec being upgraded")
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		launchTemplate, ok := launchTemplateMap[pool.Name]
		if !ok {
			return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
		}
		launchTemplateCpy := launchTemplate.DeepCopy()
		launchTemplateCpy.Spec = cluster.Spec.NodeGroupSpec.Template.LaunchTemplateSpec
		launchTemplateCpy.Spec.ForProvider.ImageID = &pool.AMI

		if equality.Semantic.DeepEqual(launchTemplate, launchTemplateCpy) {
			continue
		}
		err = r.Update(ctx, launchTemplateCpy)
		if err != nil {
			logger.Error(err, "unable to update launch template")
			r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, err.Error())
			return ctrl.Result{}, err
		}

		// requeue to wait for update
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	r.setReadyCondition(&cluster, v1.ConditionFalse, v1alpha1.ConditionReasonUpgrading, "node group spec being upgraded")
	for _, pool := range cluster.Spec.NodeGroupSpec.Groups {
		nodeGroup, ok := nodeGroupMap[pool.Name]
		if !ok {
			return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
		}
		nodeGroupCpy := nodeGroup.DeepCopy()
		nodeGroupCpy.Spec = cluster.Spec.NodeGroupSpec.Template.NodeGroupSpec
		if equality.Semantic.DeepEqual(nodeGroup, nodeGroupCpy) {
			continue
		}
		err = r.Update(ctx, nodeGroupCpy)
		if err != nil {
			logger.Error(err, "unable to update node group")
			return ctrl.Result{}, err
		}

		// requeue to wait for update
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	// done
	r.setReadyCondition(&cluster, v1.ConditionTrue, v1alpha1.ConditionReasonAvailable, "successfully reconciled")
	return ctrl.Result{}, nil
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
