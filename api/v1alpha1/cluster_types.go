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

package v1alpha1

import (
	ec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	eksv1beta1 "github.com/upbound/provider-aws/apis/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	Region        string        `json:"region"`
	NodeGroupSpec NodeGroupSpec `json:"nodeGroupSpec"`
}

type NodeGroupSpec struct {
	UpdateStrategy NodeGroupUpdateStrategy `json:"updateStrategy"`
	Template       NodeGroupTemplate       `json:"template"`
	Groups         []NodeGroup             `json:"groups"`
}

type NodeGroupTemplate struct {
	LaunchTemplateSpec ec2v1beta1.LaunchTemplateSpec `json:"launchTemplateSpec"`
	NodeGroupSpec      eksv1beta1.NodeGroupSpec      `json:"nodeGroupSpec"`
}

type NodeGroup struct {
	Name string `json:"name"`
	AMI  string `json:"ami"`
}

type NodeGroupUpdateStrategy struct {
	RollingUpdate NodeGroupRollingUpdate `json:"rollingUpdate"`
}

type NodeGroupRollingUpdate struct {
	MaxUnavailable int `json:"maxUnavailable"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:subresource:status

type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// ClusterStatus defines the observed state of Cluster
type ClusterStatus struct {
	Conditions []StatusCondition `json:"conditions"`
	NodePools  []NodePoolStatus  `json:"nodePools"`
}

type NodePoolStatus struct {
	Name       string            `json:"name"`
	Conditions []StatusCondition `json:"conditions"`
}

type ConditionType string

const (
	ConditionTypeReady ConditionType = "Ready"
)

const (
	ConditionReasonInitializing = "Initializing"
	ConditionReasonUpgrading    = "Upgrading"
	ConditionReasonAvailable    = "Available"
	ConditionReasonDeleting     = "Deleting"
)

type StatusCondition struct {
	Type   ConditionType          `json:"type"`
	Status corev1.ConditionStatus `json:"status"`

	// +optional
	Reason string `json:"reason,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}
