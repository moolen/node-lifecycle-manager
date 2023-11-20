package controller

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"text/template"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/moolen/node-lifecycle-manager/api/v1alpha1"
	ec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	eksv1beta1 "github.com/upbound/provider-aws/apis/eks/v1beta1"
	iamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	utilpointer "k8s.io/utils/pointer"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

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
	eksCluster.Spec.ProviderConfigReference = cluster.Spec.ProviderConfigReference
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
	subnet.Spec.ProviderConfigReference = cluster.Spec.ProviderConfigReference
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
	launchTemplate.Spec.ProviderConfigReference = cluster.Spec.ProviderConfigReference

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

	role.Spec.ProviderConfigReference = cluster.Spec.ProviderConfigReference
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
	nodeGroup.Spec.ProviderConfigReference = cluster.Spec.ProviderConfigReference
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
