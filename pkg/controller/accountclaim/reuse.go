package accountclaim

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/go-logr/logr"
	awsv1alpha1 "github.com/openshift/aws-account-operator/pkg/apis/aws/v1alpha1"
	"github.com/openshift/aws-account-operator/pkg/awsclient"
	"github.com/openshift/aws-account-operator/pkg/controller/account"
)

const (
	// AccountReady indicates account creation is ready
	AccountReady = "Ready"
	// AccountFailed indicates account reuse has failed
	AccountFailed = "Failed"
)

func (r *ReconcileAccountClaim) finalizeAccountClaim(reqLogger logr.Logger, accountClaim *awsv1alpha1.AccountClaim) error {

	// Get account claimed by deleted accountclaim
	reusedAccount, err := r.getClaimedAccount(accountClaim.Spec.AccountLink, awsv1alpha1.AccountCrNamespace)
	if err != nil {
		reqLogger.Error(err, "Failed to get claimed account")
		return err
	}

	// Perform account clean up in AWS
	err = r.cleanUpAwsAccount(reqLogger, accountClaim, reusedAccount)
	if err != nil {
		reqLogger.Error(err, "Failed to clean up AWS account")
		return err
	}

	err = r.resetAccountSpecStatus(reqLogger, reusedAccount, accountClaim, awsv1alpha1.AccountReused, "Ready")
	if err != nil {
		reqLogger.Error(err, "Failed to reset account entity")
		return err
	}

	reqLogger.Info("Successfully finalized AccountClaim")
	return nil
}

func (r *ReconcileAccountClaim) resetAccountSpecStatus(reqLogger logr.Logger, reusedAccount *awsv1alpha1.Account, deletedAccountClaim *awsv1alpha1.AccountClaim, accountState awsv1alpha1.AccountConditionType, conditionStatus string) error {

	// Reset claimlink and carry over legal entity from deleted claim
	reusedAccount.Spec.ClaimLink = ""

	// LegalEntity is being carried over here to support older accounts, that were claimed
	// prior to the introduction of reuse (their account's legalEntity will be blank )
	if reusedAccount.Spec.LegalEntity.ID == "" {
		reusedAccount.Spec.LegalEntity.ID = deletedAccountClaim.Spec.LegalEntity.ID
		reusedAccount.Spec.LegalEntity.Name = deletedAccountClaim.Spec.LegalEntity.Name
	}

	err := r.accountSpecUpdate(reqLogger, reusedAccount)
	if err != nil {
		reqLogger.Error(err, "Failed to update account spec for reuse")
		return err
	}

	// Update account status and add conditions indicating account reuse
	reusedAccount.Status.State = conditionStatus
	reusedAccount.Status.Claimed = false
	reusedAccount.Status.Reused = true
	conditionMsg := fmt.Sprintf("Account Reuse - %s", conditionStatus)
	account.SetAccountStatus(reqLogger, reusedAccount, conditionMsg, accountState, conditionStatus)
	err = r.accountStatusUpdate(reqLogger, reusedAccount)
	if err != nil {
		reqLogger.Error(err, "Failed to update account status for reuse")
		return err
	}

	return nil
}

func (r *ReconcileAccountClaim) cleanUpAwsAccount(reqLogger logr.Logger, accountClaim *awsv1alpha1.AccountClaim, account *awsv1alpha1.Account) error {
	// Cleean up status, used to store an error if any of the cleanup functions recveived one
	cleanUpStatusFailed := false

	// Channels to track clean up functions
	awsNotifications, awsErrors := make(chan string), make(chan string)

	defer close(awsNotifications)
	defer close(awsErrors)

	// Region comes from accountClaim
	clusterAwsRegion := accountClaim.Spec.Aws.Regions[0].Name
	// AWS credential comes from account object
	awsClientInput := awsclient.NewAwsClientInput{
		SecretName: account.Spec.IAMUserSecret,
		NameSpace:  awsv1alpha1.AccountCrNamespace,
		AwsRegion:  clusterAwsRegion,
	}

	awsClient, err := awsclient.GetAWSClient(r.client, awsClientInput)

	if err != nil {
		connErr := fmt.Sprintf("Unable to create aws client for region %s", clusterAwsRegion)
		reqLogger.Error(err, connErr)
		return err
	}

	// Declare un array of cleanup functions
	cleanUpFunctions := []func(logr.Logger, awsclient.Client, chan string, chan string) error{
		r.cleanUpAwsAccountSnapshots,
		r.cleanUpAwsAccountEbsVolumes,
		r.cleanUpAwsAccountS3,
	}

	// Call the clean up functions in parallel
	for _, cleanUpFunc := range cleanUpFunctions {
		go cleanUpFunc(reqLogger, awsClient, awsNotifications, awsErrors)
	}

	// Wait for clean up functions to end
	for i := 0; i < len(cleanUpFunctions); i++ {
		select {
		case msg := <-awsNotifications:
			reqLogger.Info(msg)
		case errMsg := <-awsErrors:
			err = errors.New(errMsg)
			reqLogger.Error(err, errMsg)
			cleanUpStatusFailed = true
		}
	}

	// Return an error if we saw any errors on the awsErrors channel so we can make the reused account as failed
	if cleanUpStatusFailed {
		cleanUpStatusFailedMsg := "Failed to clean up AWS account"
		err = errors.New(cleanUpStatusFailedMsg)
		reqLogger.Error(err, cleanUpStatusFailedMsg)
	}

	reqLogger.Info("AWS account cleanup completed")

	return nil
}

func (r *ReconcileAccountClaim) cleanUpAwsAccountSnapshots(reqLogger logr.Logger, awsClient awsclient.Client, awsNotifications chan string, awsErrors chan string) error {

	// Filter only for snapshots owned by the account
	selfOwnerFilter := ec2.Filter{
		Name: aws.String("owner-alias"),
		Values: []*string{
			aws.String("self"),
		},
	}
	describeSnapshotsInput := ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			&selfOwnerFilter,
		},
	}
	ebsSnapshots, err := awsClient.DescribeSnapshots(&describeSnapshotsInput)
	if err != nil {
		descError := "Failed describing EBS snapshots"
		awsErrors <- descError
		return err
	}

	for _, snapshot := range ebsSnapshots.Snapshots {

		deleteSnapshotInput := ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(*snapshot.SnapshotId),
		}

		_, err = awsClient.DeleteSnapshot(&deleteSnapshotInput)
		if err != nil {
			delError := fmt.Sprintf("Failed deleting EBS snapshot: %s", *snapshot.SnapshotId)
			awsErrors <- delError
			return err
		}
	}

	successMsg := fmt.Sprintf("Snapshot cleanup finished successfully")
	awsNotifications <- successMsg
	return nil
}

func (r *ReconcileAccountClaim) cleanUpAwsAccountEbsVolumes(reqLogger logr.Logger, awsClient awsclient.Client, awsNotifications chan string, awsErrors chan string) error {

	describeVolumesInput := ec2.DescribeVolumesInput{}
	ebsVolumes, err := awsClient.DescribeVolumes(&describeVolumesInput)
	if err != nil {
		descError := "Failed describing EBS volumes"
		awsErrors <- descError
		return err
	}

	for _, volume := range ebsVolumes.Volumes {

		deleteVolumeInput := ec2.DeleteVolumeInput{
			VolumeId: aws.String(*volume.VolumeId),
		}

		_, err = awsClient.DeleteVolume(&deleteVolumeInput)
		if err != nil {
			delError := fmt.Sprintf("Failed deleting EBS volume: %s", *volume.VolumeId)
			awsErrors <- delError
			return err
		}

	}

	successMsg := fmt.Sprintf("EBS Volume cleanup finished successfully")
	awsNotifications <- successMsg
	return nil
}

func (r *ReconcileAccountClaim) cleanUpAwsAccountS3(reqLogger logr.Logger, awsClient awsclient.Client, awsNotifications chan string, awsErrors chan string) error {
	listBucketsInput := s3.ListBucketsInput{}
	s3Buckets, err := awsClient.ListBuckets(&listBucketsInput)
	if err != nil {
		listError := "Failed listing S3 buckets"
		awsErrors <- listError
		return err
	}

	for _, bucket := range s3Buckets.Buckets {

		deleteBucketInput := s3.DeleteBucketInput{
			Bucket: aws.String(*bucket.Name),
		}

		// delete any content if any
		err := DeleteBucketContent(awsClient, *bucket.Name)
		if err != nil {
			ContentDelErr := fmt.Sprintf("Failed to delete bucket content: %s", *bucket.Name)
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case s3.ErrCodeNoSuchBucket:
					//ignore these errors
				default:
					awsErrors <- ContentDelErr
					return err
				}
			}
		}
		_, err = awsClient.DeleteBucket(&deleteBucketInput)
		if err != nil {
			DelError := fmt.Sprintf("Failed deleting S3 bucket: %s", *bucket.Name)
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case s3.ErrCodeNoSuchBucket:
					//ignore these errors
				default:
					awsErrors <- DelError
					return err
				}
			}
		}

	}

	successMsg := fmt.Sprintf("S3 cleanup finished successfully")
	awsNotifications <- successMsg
	return nil
}

// DeleteBucketContent deletes any content in a bucket if it is not empty
func DeleteBucketContent(awsClient awsclient.Client, bucketName string) error {
	// check if objects exits
	objects, err := awsClient.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return err
	}
	if len((*objects).Contents) == 0 {
		return nil
	}

	err = awsClient.BatchDeleteBucketObjects(aws.String(bucketName))
	if err != nil {
		return err
	}

	return nil
}

func (r *ReconcileAccountClaim) accountStatusUpdate(reqLogger logr.Logger, account *awsv1alpha1.Account) error {
	err := r.client.Status().Update(context.TODO(), account)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", account.Name))
	}
	return err
}

func matchAccountForReuse(account *awsv1alpha1.Account, accountClaim *awsv1alpha1.AccountClaim) bool {
	if account.Spec.LegalEntity.ID == accountClaim.Spec.LegalEntity.ID {
		return true
	}
	return false
}
