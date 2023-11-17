package controller

import (
	v1alpha1 "github.com/moolen/node-lifecycle-manager/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewClusterCondition a set of default options for creating an External Secret Condition.
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

// SetClusterCondition updates the external secret to include the provided
// condition.
func SetClusterCondition(es *v1alpha1.Cluster, condition v1alpha1.StatusCondition) {
	currentCond := GetClusterCondition(es.Status, condition.Type)

	if currentCond != nil && currentCond.Status == condition.Status &&
		currentCond.Reason == condition.Reason && currentCond.Message == condition.Message {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	es.Status.Conditions = append(filterOutCondition(es.Status.Conditions, condition.Type), condition)
}

// GetNodePoolCondition returns the condition with the provided type.
func GetNodePoolCondition(poolName string, status v1alpha1.ClusterStatus, condType v1alpha1.ConditionType) *v1alpha1.StatusCondition {
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

// SetNodePoolCondition updates the external secret to include the provided
// condition.
func SetNodePoolCondition(poolName string, es *v1alpha1.Cluster, condition v1alpha1.StatusCondition) {
	currentCond := GetNodePoolCondition(poolName, es.Status, condition.Type)

	if currentCond != nil && currentCond.Status == condition.Status &&
		currentCond.Reason == condition.Reason && currentCond.Message == condition.Message {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	for i, pool := range es.Status.NodeGroups {
		if pool.Name != poolName {
			continue
		}
		es.Status.NodeGroups[i].Conditions = append(filterOutCondition(es.Status.Conditions, condition.Type), condition)
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
