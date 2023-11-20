package controller

import (
	"encoding/json"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	v1alpha1 "github.com/moolen/node-lifecycle-manager/api/v1alpha1"
	ec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	"github.com/wI2L/jsondiff"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewClusterCondition a set of default options for creating the resource Condition.
func NewClusterCondition(condType v1alpha1.ConditionType, status v1.ConditionStatus, reason, message string) *v1alpha1.StatusCondition {
	return &v1alpha1.StatusCondition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// GetClusterCondition returns the condition with the provided type.
func GetClusterCondition(status v1alpha1.ClusterStatus, condType v1alpha1.ConditionType) *v1alpha1.StatusCondition {
	for _, c := range status.Conditions {
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// SetClusterCondition updates the resource to include the provided
// condition.
func SetClusterCondition(cluster *v1alpha1.Cluster, condition v1alpha1.StatusCondition) {
	currentCond := GetClusterCondition(cluster.Status, condition.Type)

	if currentCond != nil && currentCond.Status == condition.Status &&
		currentCond.Reason == condition.Reason && currentCond.Message == condition.Message {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	cluster.Status.Conditions = append(filterOutCondition(cluster.Status.Conditions, condition.Type), condition)
}

// GetNodeGroupCondition returns the condition with the provided type.
func GetNodeGroupCondition(poolName string, status v1alpha1.ClusterStatus, condType v1alpha1.ConditionType) *v1alpha1.StatusCondition {
	for _, pool := range status.NodeGroups {
		if poolName != pool.Name {
			continue
		}
		for _, c := range pool.Conditions {
			if c.Type == condType {
				return &c
			}
		}
	}
	return nil
}

func updateLaunchTemplateLastUpdateTime(groupName string, cluster *v1alpha1.Cluster, lastUpdate metav1.Time) {
	for i := range cluster.Status.NodeGroups {
		if groupName == cluster.Status.NodeGroups[i].Name {
			cluster.Status.NodeGroups[i].LaunchTemplateLastUpdate = lastUpdate
			return
		}
	}
	cluster.Status.NodeGroups = append(cluster.Status.NodeGroups, v1alpha1.NodeGroupStatus{
		Name:                     groupName,
		LaunchTemplateLastUpdate: lastUpdate,
	})
}

// SetNodeGroupCondition updates the resource to include the provided
// condition.
func SetNodeGroupCondition(poolName string, cluster *v1alpha1.Cluster, condition v1alpha1.StatusCondition) {
	currentCond := GetNodeGroupCondition(poolName, cluster.Status, condition.Type)

	if currentCond != nil && currentCond.Status == condition.Status &&
		currentCond.Reason == condition.Reason && currentCond.Message == condition.Message {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	for i, pool := range cluster.Status.NodeGroups {
		if pool.Name != poolName {
			continue
		}
		cluster.Status.NodeGroups[i].Conditions = append(filterOutCondition(cluster.Status.Conditions, condition.Type), condition)
	}
}

// filterOutCondition returns an empty set of conditions with the provided type.
func filterOutCondition(conditions []v1alpha1.StatusCondition, condType v1alpha1.ConditionType) []v1alpha1.StatusCondition {
	newConditions := make([]v1alpha1.StatusCondition, 0, len(conditions))
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

func resourceReady(conds []crossplanecommonv1.Condition) bool {
	for _, cond := range conds {
		if cond.Type == crossplanecommonv1.TypeReady && cond.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func launchTemplateSynced(launchTemplate ec2v1beta1.LaunchTemplate) bool {
	diff, err := jsondiff.Compare(launchTemplate.Spec.ForProvider, launchTemplate.Status.AtProvider)
	if err != nil {
		return false
	}
	fmt.Println("---- launch template diff ----")
	bt, _ := json.MarshalIndent(diff, "", "  ")
	fmt.Println(string(bt))
	fmt.Println("---- end launch template diff ----")
	return len(diff) == 0
}
