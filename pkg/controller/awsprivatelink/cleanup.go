package awsprivatelink

import (
	"context"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/route53"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	hivev1aws "github.com/openshift/hive/apis/hive/v1/aws"
	"github.com/openshift/hive/pkg/awsclient"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
)

func (r *ReconcileAWSPrivateLink) cleanupClusterDeployment(cd *hivev1.ClusterDeployment, metadata *hivev1.ClusterMetadata, logger log.FieldLogger) (reconcile.Result, error) {
	if !controllerutils.HasFinalizer(cd, finalizer) {
		return reconcile.Result{}, nil
	}

	if err := r.setProgressCondition(cd, corev1.ConditionFalse,
		"CleanupForDeprovisionInprogress",
		"cleaning up all the resources created for private link access for deprovisioning the cluster",
		logger); err != nil {
		logger.WithError(err).Error("failed to update condition on cluster deployment")
		return reconcile.Result{}, err
	}

	if err := r.cleanupPrivateLink(cd, metadata, logger); err != nil {
		logger.WithError(err).Error("error cleaning up PrivateLink resources for ClusterDeployment")

		if err := r.setErrCondition(cd, "CleanupForDeprovisionFailed", err, logger); err != nil {
			logger.WithError(err).Error("failed to update condition on cluster deployment")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, err
	}

	logger.Info("removing finalizer from ClusterDeployment")
	controllerutils.DeleteFinalizer(cd, finalizer)
	if err := r.Update(context.Background(), cd); err != nil {
		logger.WithError(err).Log(controllerutils.LogLevel(err), "could not remove finalizer from ClusterDeployment")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileAWSPrivateLink) cleanupPreviousProvisionAttempt(cd *hivev1.ClusterDeployment, cp *hivev1.ClusterProvision,
	logger log.FieldLogger) error {
	if cd.Spec.ClusterMetadata == nil {
		return errors.New("cannot cleanup previous resources because the admin kubeconfig is not available")
	}
	metadata := &hivev1.ClusterMetadata{
		InfraID:                  *cp.Spec.PrevInfraID,
		AdminKubeconfigSecretRef: cd.Spec.ClusterMetadata.AdminKubeconfigSecretRef,
	}

	if err := r.cleanupPrivateLink(cd, metadata, logger); err != nil {
		logger.WithError(err).Error("error cleaning up PrivateLink resources for ClusterDeployment")
		return err
	}
	if cd.Annotations == nil {
		cd.Annotations = map[string]string{}
	}
	cd.Annotations[lastCleanupAnnotationKey] = metadata.InfraID
	return updateAnnotations(r.Client, cd)
}

func cleanupRequired(cd *hivev1.ClusterDeployment) bool {
	var plStatus hivev1aws.PrivateLinkAccessStatus
	if cd.Status.Platform != nil && cd.Status.Platform.AWS != nil && cd.Status.Platform.AWS.PrivateLink != nil {
		plStatus = *cd.Status.Platform.AWS.PrivateLink
	}
	return plStatus.VPCEndpointID != "" ||
		plStatus.VPCEndpointService.ID != "" ||
		plStatus.VPCEndpointService.Name != "" ||
		plStatus.HostedZoneID != ""
}

func (r *ReconcileAWSPrivateLink) cleanupPrivateLink(cd *hivev1.ClusterDeployment, metadata *hivev1.ClusterMetadata, logger log.FieldLogger) error {
	awsClient, err := newAWSClient(r, cd)
	if err != nil {
		logger.WithError(err).Error("error creating AWS client for the cluster")
		return err
	}

	if err := r.cleanupHostedZone(awsClient.hub, cd, metadata, logger); err != nil {
		logger.WithError(err).Error("error cleaning up Hosted Zone")
		return err
	}
	if err := r.cleanupVPCEndpoint(awsClient.hub, cd, metadata, logger); err != nil {
		logger.WithError(err).Error("error cleaning up VPCEndpoint")
		return err
	}
	if err := r.cleanupVPCEndpointService(awsClient.user, cd, metadata, logger); err != nil {
		logger.WithError(err).Error("error cleaning up VPCEndpoint Service")
		return err
	}

	initPrivateLinkStatus(cd)
	cd.Status.Platform.AWS.PrivateLink = nil
	if err := r.updatePrivateLinkStatus(cd, logger); err != nil {
		logger.WithError(err).Error("error updating clusterdeployment after cleanup of private link")
		return err
	}

	return nil
}

func (r *ReconcileAWSPrivateLink) cleanupHostedZone(awsClient awsclient.Client,
	cd *hivev1.ClusterDeployment, metadata *hivev1.ClusterMetadata,
	logger log.FieldLogger) error {
	apiDomain, err := initialURL(r.Client,
		client.ObjectKey{Namespace: cd.Namespace, Name: metadata.AdminKubeconfigSecretRef.Name})
	if err != nil {
		logger.WithError(err).Error("could not get API URL from kubeconfig")
		return err
	}

	idLog := logger.WithField("infraID", metadata.InfraID)
	endpointResp, err := awsClient.DescribeVpcEndpoints(&ec2.DescribeVpcEndpointsInput{
		Filters: []*ec2.Filter{ec2FilterForCluster(metadata)},
	})
	if err != nil {
		idLog.WithError(err).Error("error getting the VPC Endpoint")
		return err
	}
	if len(endpointResp.VpcEndpoints) == 0 {
		return nil // no work
	}

	vpcEndpoint := endpointResp.VpcEndpoints[0]
	hzID, err := findHostedZone(awsClient, *vpcEndpoint.VpcId, cd.Spec.Platform.AWS.Region, apiDomain, logger)
	if err != nil && errors.Is(err, errNoHostedZoneFoundForVPC) {
		return nil // no work
	}
	if err != nil {
		idLog.WithError(err).Error("error getting the Hosted Zone")
		return err
	}

	hzLog := logger.WithField("hostedZoneID", hzID)
	recordsResp, err := awsClient.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(hzID),
	})
	if err != nil {
		hzLog.WithError(err).Error("failed to list the hosted zone")
		return err
	}
	for _, record := range recordsResp.ResourceRecordSets {
		if *record.Type == "SOA" || *record.Type == "NS" {
			// can't delete SOA and NS types
			continue
		}
		_, err := awsClient.ChangeResourceRecordSets(&route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(hzID),
			ChangeBatch: &route53.ChangeBatch{
				Changes: []*route53.Change{{
					Action:            aws.String("DELETE"),
					ResourceRecordSet: record,
				}},
			},
		})
		if err != nil {
			hzLog.WithField("record", *record.Name).WithError(err).Error("failed to list the hosted zone")
			return err
		}
	}

	_, err = awsClient.DeleteHostedZone(&route53.DeleteHostedZoneInput{
		Id: aws.String(hzID),
	})
	if err != nil && !awsErrCodeEquals(err, "NoSuchHostedZone") {
		hzLog.WithError(err).Error("error deleting the hosted zone")
		return err
	}

	return nil

}

func (r *ReconcileAWSPrivateLink) cleanupVPCEndpoint(awsClient awsclient.Client,
	cd *hivev1.ClusterDeployment, metadata *hivev1.ClusterMetadata,
	logger log.FieldLogger) error {
	idLog := logger.WithField("infraID", metadata.InfraID)
	resp, err := awsClient.DescribeVpcEndpoints(&ec2.DescribeVpcEndpointsInput{
		Filters: []*ec2.Filter{ec2FilterForCluster(metadata)},
	})
	if err != nil {
		idLog.WithError(err).Error("error getting the VPC Endpoint")
		return err
	}
	if len(resp.VpcEndpoints) == 0 {
		return nil // no work
	}

	vpcEndpoint := resp.VpcEndpoints[0]
	endpointLog := logger.WithField("vpcEndpointID", *vpcEndpoint.VpcEndpointId)

	_, err = awsClient.DeleteVpcEndpoints(&ec2.DeleteVpcEndpointsInput{
		VpcEndpointIds: aws.StringSlice([]string{*vpcEndpoint.VpcEndpointId}),
	})
	if err != nil && !awsErrCodeEquals(err, "InvalidVpcEndpointId.NotFound") {
		endpointLog.WithError(err).Error("error deleting the VPC Endpoint")
		return err
	}

	return nil
}

func (r *ReconcileAWSPrivateLink) cleanupVPCEndpointService(awsClient awsclient.Client,
	cd *hivev1.ClusterDeployment, metadata *hivev1.ClusterMetadata,
	logger log.FieldLogger) error {
	idLog := logger.WithField("infraID", metadata.InfraID)
	resp, err := awsClient.DescribeVpcEndpointServiceConfigurations(&ec2.DescribeVpcEndpointServiceConfigurationsInput{
		Filters: []*ec2.Filter{ec2FilterForCluster(metadata)},
	})
	if err != nil {
		idLog.WithError(err).Error("error getting the VPC Endpoint Service")
		return err
	}
	if len(resp.ServiceConfigurations) == 0 {
		return nil // no work
	}

	service := resp.ServiceConfigurations[0]
	serviceLog := logger.WithField("vpcEndpointServiceID", *service.ServiceId)

	_, err = awsClient.DeleteVpcEndpointServiceConfigurations(&ec2.DeleteVpcEndpointServiceConfigurationsInput{
		ServiceIds: aws.StringSlice([]string{*service.ServiceId}),
	})
	if err != nil && !awsErrCodeEquals(err, "InvalidVpcEndpointService.NotFound") {
		serviceLog.WithError(err).Error("error deleting the VPC Endpoint Service")
		return err
	}

	return nil
}
