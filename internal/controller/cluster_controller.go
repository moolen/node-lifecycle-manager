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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wI2L/jsondiff"

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
		updateFn := func() error {
			return r.updateLaunchTemplate(candidate.Group, cluster, candidate.Subnet.Status.AtProvider.AvailabilityZone, eksCluster, launchTemplate)
		}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, launchTemplate, updateFn)
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

		if !launchTemplateSynced(*launchTemplate) {
			logger.Info("launch template not yet synced, requeueing")
			requeue = true
			continue
		}

		// update/verify node group
		nodeGroup := &eksv1beta1.NodeGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: groupName,
			},
		}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, nodeGroup, func() error {
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
			if !launchTemplateSynced(*launchTemplate) {
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

func (r *ClusterReconciler) getAwaitingUpdateDiff(ctx context.Context, obj ClientObject, f controllerutil.MutateFn) (jsondiff.Patch, error) {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		return nil, err
	}
	existing := obj.DeepCopyObject()
	if err := mutate(f, key, obj); err != nil {
		return nil, err
	}

	patches, err := jsondiff.Compare(existing, obj)
	if err != nil {
		return nil, err
	}

	// everything that is being changed in /spec/forProvider
	// will be reconciled by crossplane and the value in
	var filteredPatches jsondiff.Patch
	for _, p := range patches {
		if strings.HasPrefix(p.Path, "/spec/forProvider") {
			filteredPatches = append(filteredPatches, jsondiff.Operation{
				Value: p.Value,
				Type:  p.Type,
				From:  p.From,
				Path:  strings.Replace(p.Path, "/spec/forProvider", "/status/atProvider", 1),
			})
		}
	}

	return filteredPatches, nil
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
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, eksCluster, func() error {
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
			_, err = controllerutil.CreateOrUpdate(ctx, r.Client, eksSubnet, func() error {
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
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, workerIamRole, func() error {
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
