package hibernation

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	machineapi "github.com/openshift/api/machine/v1beta1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	awsclient "github.com/openshift/hive/pkg/awsclient"
	"github.com/openshift/hive/pkg/constants"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
)

var (
	runningStates           = sets.NewString("running")
	stoppedStates           = sets.NewString("stopped")
	pendingStates           = sets.NewString("pending")
	stoppingStates          = sets.NewString("stopping", "shutting-down")
	runningOrPendingStates  = runningStates.Union(pendingStates)
	stoppedOrStoppingStates = stoppedStates.Union(stoppingStates)
	notRunningStates        = stoppedOrStoppingStates.Union(pendingStates)
	notStoppedStates        = runningOrPendingStates.Union(stoppingStates)
)

func init() {
	RegisterActuator(&awsActuator{awsClientFn: getAWSClient})
}

type awsActuator struct {
	// awsClientFn is the function to build an AWS client, here for testing
	awsClientFn func(*hivev1.ClusterDeployment, client.Client, log.FieldLogger) (awsclient.Client, error)
}

// CanHandle returns true if the actuator can handle a particular ClusterDeployment
func (a *awsActuator) CanHandle(cd *hivev1.ClusterDeployment) bool {
	return cd.Spec.Platform.AWS != nil
}

// StopMachines will stop machines belonging to the given ClusterDeployment
func (a *awsActuator) StopMachines(cd *hivev1.ClusterDeployment, hiveClient client.Client, logger log.FieldLogger) error {
	logger = logger.WithField("cloud", "aws")
	awsClient, err := a.awsClientFn(cd, hiveClient, logger)
	if err != nil {
		return err
	}

	instances, err := getClusterInstances(cd, awsClient, runningOrPendingStates, logger)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		logger.Warning("No instances were found to stop")
		return nil
	}
	instances, spotInstances := filterOutSpotInstances(instances)
	if err := a.stopOnDemandInstances(awsClient, instanceIDs(instances), logger); err != nil {
		return err
	}
	if err := a.stopSpotInstances(awsClient, instanceIDs(spotInstances), logger); err != nil {
		return err
	}

	return nil
}

func (a *awsActuator) stopOnDemandInstances(awsClient awsclient.Client, instanceIDs []string, logger log.FieldLogger) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	logger.WithField("instanceIDs", instanceIDs).Info("Stopping on-demand cluster instances")
	_, err := awsClient.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: aws.StringSlice(instanceIDs),
	})
	if err != nil {
		logger.WithError(err).Error("failed to stop on-demand instances")
		return err
	}
	return nil
}

func (a *awsActuator) stopSpotInstances(awsClient awsclient.Client, instanceIDs []string, logger log.FieldLogger) error {
	if len(instanceIDs) == 0 {
		return nil
	}
	logger.WithField("instanceIDs", instanceIDs).Info("Terminating spot cluster instances")
	_, err := awsClient.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice(instanceIDs),
	})
	if err != nil {
		logger.WithError(err).Error("failed to terminate spot instances")
		return err
	}
	return nil
}

// StartMachines will select machines belonging to the given ClusterDeployment
func (a *awsActuator) StartMachines(cd *hivev1.ClusterDeployment, hiveClient client.Client, logger log.FieldLogger) error {
	logger = logger.WithField("cloud", "aws")
	awsClient, err := a.awsClientFn(cd, hiveClient, logger)
	if err != nil {
		return err
	}

	instances, err := getClusterInstances(cd, awsClient, stoppedOrStoppingStates, logger)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		logger.Info("No instances were found to start")
		return nil
	}

	ids := instanceIDs(instances)
	logger.WithField("instanceIDs", ids).Info("Starting on-demand cluster instances")
	_, err = awsClient.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	})
	if err != nil {
		logger.WithError(err).Error("failed to start on-demand instances")
		return err
	}
	return nil
}

// MachinesRunning will return true if the machines associated with the given
// ClusterDeployment are in a running state. It also returns a list of machines that
// are not running.
func (a *awsActuator) MachinesRunning(cd *hivev1.ClusterDeployment, hiveClient client.Client, logger log.FieldLogger) (bool, []string, error) {
	logger = logger.WithField("cloud", "aws")
	logger.Infof("checking whether machines are running")
	awsClient, err := a.awsClientFn(cd, hiveClient, logger)
	if err != nil {
		return false, nil, err
	}
	instances, err := getClusterInstances(cd, awsClient, notRunningStates, logger)
	if err != nil {
		return false, nil, err
	}
	return len(instances) == 0, instanceIDs(instances), nil
}

// MachinesStopped will return true if the machines associated with the given
// ClusterDeployment are in a stopped state. It also returns a list of machines
// that have not stopped.
func (a *awsActuator) MachinesStopped(cd *hivev1.ClusterDeployment, hiveClient client.Client, logger log.FieldLogger) (bool, []string, error) {
	logger = logger.WithField("cloud", "aws")
	logger.Infof("checking whether machines are stopped")
	awsClient, err := a.awsClientFn(cd, hiveClient, logger)
	if err != nil {
		return false, nil, err
	}
	instances, err := getClusterInstances(cd, awsClient, notStoppedStates, logger)
	if err != nil {
		return false, nil, err
	}
	return len(instances) == 0, instanceIDs(instances), nil
}

// ReplaceMachines implements HibernationPreemptibleMachines interface.
func (a *awsActuator) ReplaceMachines(cd *hivev1.ClusterDeployment, remoteClient client.Client, logger log.FieldLogger) (bool, error) {
	hibernatingCondition := controllerutils.FindCondition(cd.Status.Conditions,
		hivev1.ClusterHibernatingCondition)
	if hibernatingCondition == nil {
		return false, errors.New("cannot find hibernating condition")
	}
	hibernationStartedTime := hibernatingCondition.LastTransitionTime

	machineList := &machineapi.MachineList{}
	err := remoteClient.List(context.TODO(), machineList,
		client.InNamespace(machineAPINamespace),
		client.MatchingLabels{machineAPIInterruptibleLabel: ""},
	)
	if err != nil {
		logger.WithError(err).Error("Failed to list machines")
		return false, errors.Wrap(err, "failed to list machines")
	}
	if len(machineList.Items) == 0 {
		return false, nil
	}

	var toBeReplaced []machineapi.Machine
	for _, m := range machineList.Items {
		if m.GetDeletionTimestamp() != nil {
			// this object is already marked for deletion
			continue
		}
		if m.Status.LastUpdated.After(hibernationStartedTime.Time) &&
			m.Status.Phase != nil && *m.Status.Phase != "Failed" {
			// this is a machine that is reporting not failed
			// after hibernation was started, therefore do not
			// remove
			continue
		}

		toBeReplaced = append(toBeReplaced, m)
	}

	logger.WithField("machines", machineNames(toBeReplaced)).Debug("Preemptible Machine objects will be replaced")
	var replaced bool
	var errs []error
	for _, m := range toBeReplaced {
		// We want the machine-api to skip the draining
		// since we already know these nodes were terminated
		// during hibernation.
		anno := m.GetAnnotations()
		if anno == nil {
			anno = map[string]string{}
		}
		anno[machineAPIExcludeDrainingAnnotation] = "true"
		m.SetAnnotations(anno)
		if err := remoteClient.Update(context.TODO(), &m); err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to update machine %s/%s to be excluded from draining",
				machineAPINamespace, m.GetName()))
			continue
		}

		// Delete the machine object so that it will be replaced
		// by the machine set.
		if err := remoteClient.Delete(context.TODO(), &m); err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to delete machine %s/%s", machineAPINamespace, m.GetName()))
			continue
		}
		replaced = true
	}
	if len(errs) > 0 {
		err := utilerrors.NewAggregate(errs)
		logger.WithError(err).Error("Failed to delete machines")
		return replaced, err
	}

	return replaced, nil
}

func machineNames(machines []machineapi.Machine) []string {
	result := make([]string, len(machines))
	for idx, m := range machines {
		result[idx] = m.GetName()
	}
	return result
}

func getAWSClient(cd *hivev1.ClusterDeployment, c client.Client, logger log.FieldLogger) (awsclient.Client, error) {
	options := awsclient.Options{
		Region: cd.Spec.Platform.AWS.Region,
		CredentialsSource: awsclient.CredentialsSource{
			Secret: &awsclient.SecretCredentialsSource{
				Namespace: cd.Namespace,
				Ref:       &cd.Spec.Platform.AWS.CredentialsSecretRef,
			},
			AssumeRole: &awsclient.AssumeRoleCredentialsSource{
				SecretRef: corev1.SecretReference{
					Name:      os.Getenv(constants.HiveAWSServiceProviderCredentialsSecretRefEnvVar),
					Namespace: controllerutils.GetHiveNamespace(),
				},
				Role: cd.Spec.Platform.AWS.CredentialsAssumeRole,
			},
		},
	}

	return awsclient.New(c, options)
}

// filterOutSpotInstances removes the spot instances from the list and returns it. It
// also returned the spot instances that were filtered out in a separate list.
func filterOutSpotInstances(instances []*ec2.Instance) ([]*ec2.Instance, []*ec2.Instance) {
	var spotInstances []*ec2.Instance
	n := 0
	for _, i := range instances {
		if aws.StringValue(i.InstanceLifecycle) == "spot" {
			spotInstances = append(spotInstances, i)
			continue
		}
		instances[n] = i
		n++
	}
	instances = instances[:n]
	return instances, spotInstances
}

func instanceIDs(instances []*ec2.Instance) []string {
	result := make([]string, len(instances))
	for idx, i := range instances {
		result[idx] = aws.StringValue(i.InstanceId)
	}
	return result
}

func getClusterInstances(cd *hivev1.ClusterDeployment, c awsclient.Client, states sets.String, logger log.FieldLogger) ([]*ec2.Instance, error) {
	infraID := cd.Spec.ClusterMetadata.InfraID
	logger = logger.WithField("infraID", infraID)
	logger.Debug("listing cluster instances")
	out, err := c.DescribeInstances(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", infraID)),
				Values: []*string{aws.String("owned")},
			},
		},
	})
	if err != nil {
		logger.WithError(err).Error("failed to list instances")
		return nil, err
	}
	var result []*ec2.Instance
	for _, r := range out.Reservations {
		for idx, i := range r.Instances {
			if states.Has(aws.StringValue(i.State.Name)) {
				result = append(result, r.Instances[idx])
			}
		}
	}
	logger.WithField("count", len(result)).WithField("states", states).Debug("result of listing instances")
	return result, nil
}
