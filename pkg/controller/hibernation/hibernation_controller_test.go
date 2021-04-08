package hibernation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	batchv1 "k8s.io/api/batch/v1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekubeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machineapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	hiveintv1alpha1 "github.com/openshift/hive/apis/hiveinternal/v1alpha1"
	"github.com/openshift/hive/pkg/controller/hibernation/mock"
	"github.com/openshift/hive/pkg/remoteclient"
	remoteclientmock "github.com/openshift/hive/pkg/remoteclient/mock"
	testcd "github.com/openshift/hive/pkg/test/clusterdeployment"
	testcs "github.com/openshift/hive/pkg/test/clustersync"
	testgeneric "github.com/openshift/hive/pkg/test/generic"
)

const (
	namespace = "test-namespace"
	cdName    = "test-cluster-deployment"
)

func TestReconcile(t *testing.T) {
	logger := log.New()
	logger.SetLevel(log.DebugLevel)

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	batchv1.AddToScheme(scheme)
	hivev1.AddToScheme(scheme)
	hiveintv1alpha1.AddToScheme(scheme)
	machineapi.AddToScheme(scheme)

	cdBuilder := testcd.FullBuilder(namespace, cdName, scheme).Options(
		testcd.Installed(),
		testcd.WithClusterVersion("4.4.9"),
	)
	o := clusterDeploymentOptions{}
	csBuilder := testcs.FullBuilder(namespace, cdName, scheme).Options(
		testcs.WithFirstSuccessTime(time.Now().Add(-10 * time.Hour)),
	)

	tests := []struct {
		name           string
		cd             *hivev1.ClusterDeployment
		cs             *hiveintv1alpha1.ClusterSync
		setupActuator  func(actuator *mock.MockHibernationActuator)
		setupCSRHelper func(helper *mock.MockcsrHelper)
		setupRemote    func(builder *remoteclientmock.MockBuilder)
		validate       func(t *testing.T, cd *hivev1.ClusterDeployment)
		expectError    bool
	}{
		{
			name: "cluster deleted",
			cd:   cdBuilder.GenericOptions(testgeneric.Deleted()).Options(o.shouldHibernate).Build(),
			cs:   csBuilder.Build(),
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				if getHibernatingCondition(cd) != nil {
					t.Errorf("not expecting hibernating condition")
				}
			},
		},
		{
			name: "cluster not installed",
			cd:   cdBuilder.Options(o.notInstalled, o.shouldHibernate).Build(),
			cs:   csBuilder.Build(),
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				require.Nil(t, getHibernatingCondition(cd))
			},
		},
		{
			name: "start hibernating, older version",
			cd:   cdBuilder.Options(o.shouldHibernate, testcd.WithClusterVersion("4.3.11")).Build(),
			cs:   csBuilder.Build(),
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionFalse, cond.Status)
				assert.Equal(t, hivev1.UnsupportedHibernationReason, cond.Reason)
			},
		},
		{
			name: "start hibernating, syncsets not applied",
			cd:   cdBuilder.Options(o.shouldHibernate, testcd.InstalledTimestamp(time.Now())).Build(),
			cs:   csBuilder.Options(testcs.WithNoFirstSuccessTime()).Build(),
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionFalse, cond.Status)
				assert.Equal(t, hivev1.SyncSetsNotAppliedReason, cond.Reason)
			},
			expectError: true,
		},
		{
			name: "start hibernating, syncsets not applied but 10 minutes have passed since cd install",
			cd:   cdBuilder.Options(o.shouldHibernate, testcd.InstalledTimestamp(time.Now().Add(-15*time.Minute))).Build(),
			cs:   csBuilder.Options(testcs.WithNoFirstSuccessTime()).Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StopMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.StoppingHibernationReason, cond.Reason)
			},
		},
		{
			name: "start hibernating",
			cd:   cdBuilder.Options(o.shouldHibernate).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StopMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.StoppingHibernationReason, cond.Reason)
			},
		},
		{
			name: "fail to stop machines",
			cd:   cdBuilder.Options(o.shouldHibernate).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StopMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(fmt.Errorf("error"))
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionFalse, cond.Status)
				assert.Equal(t, hivev1.FailedToStopHibernationReason, cond.Reason)
			},
		},
		{
			name: "stopping, machines have stopped",
			cd:   cdBuilder.Options(o.shouldHibernate, o.stopping).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().MachinesStopped(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(true, nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.HibernatingHibernationReason, cond.Reason)
			},
		},
		{
			name: "stopping, machines have not stopped",
			cd:   cdBuilder.Options(o.shouldHibernate, o.stopping).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().MachinesStopped(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(false, nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.StoppingHibernationReason, cond.Reason)
			},
		},
		{
			name: "stopping after MachinesFailedToStart",
			cd: cdBuilder.Options(o.shouldHibernate).Build(
				testcd.WithCondition(hivev1.ClusterDeploymentCondition{
					Type:   hivev1.ClusterHibernatingCondition,
					Status: corev1.ConditionTrue,
					Reason: hivev1.FailedToStartHibernationReason,
				},
				)),
			cs: csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				// Ensure we try to stop machines in this state (bugfix)
				actuator.EXPECT().StopMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.StoppingHibernationReason, cond.Reason)
			},
		},
		{
			name: "start resuming",
			cd:   cdBuilder.Options(o.hibernating).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StartMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.ResumingHibernationReason, cond.Reason)
			},
		},
		{
			name: "fail to start machines",
			cd:   cdBuilder.Options(o.hibernating).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StartMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(fmt.Errorf("error"))
			},
			expectError: true,
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.FailedToStartHibernationReason, cond.Reason)
			},
		},
		{
			name: "starting machines have already failed to start",
			cd: cdBuilder.Options(o.resuming).Build(
				testcd.WithCondition(hivev1.ClusterDeploymentCondition{
					Type:   hivev1.ClusterHibernatingCondition,
					Status: corev1.ConditionTrue,
					Reason: hivev1.FailedToStartHibernationReason,
				},
				)),
			cs: csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				// Call will succeed which should clear the FailedToStart reason:
				actuator.EXPECT().StartMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.ResumingHibernationReason, cond.Reason)
			},
		},
		{
			name: "starting, machines have not started",
			cd:   cdBuilder.Options(o.resuming).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StartMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
				actuator.EXPECT().MachinesRunning(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(false, nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.ResumingHibernationReason, cond.Reason)
			},
		},
		{
			name: "starting, machines running, nodes ready",
			cd:   cdBuilder.Options(o.resuming).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().MachinesRunning(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(true, nil)
			},
			setupRemote: func(builder *remoteclientmock.MockBuilder) {
				c := fake.NewFakeClientWithScheme(scheme, readyNodes()...)
				builder.EXPECT().Build().Times(1).Return(c, nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionFalse, cond.Status)
				assert.Equal(t, hivev1.RunningHibernationReason, cond.Reason)
			},
		},
		{
			name: "starting, machines running, unready node",
			cd:   cdBuilder.Options(o.resuming).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().MachinesRunning(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(true, nil)
			},
			setupRemote: func(builder *remoteclientmock.MockBuilder) {
				fakeClient := fake.NewFakeClientWithScheme(scheme, unreadyNode()...)
				fakeKubeClient := fakekubeclient.NewSimpleClientset()
				builder.EXPECT().Build().Times(1).Return(fakeClient, nil)
				builder.EXPECT().BuildKubeClient().Times(1).Return(fakeKubeClient, nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.ResumingHibernationReason, cond.Reason)
			},
		},
		{
			name: "starting, machines running, unready node, csrs to approve",
			cd:   cdBuilder.Options(o.resuming).Build(),
			cs:   csBuilder.Build(),
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().MachinesRunning(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(true, nil)
			},
			setupRemote: func(builder *remoteclientmock.MockBuilder) {
				fakeClient := fake.NewFakeClientWithScheme(scheme, unreadyNode()...)
				fakeKubeClient := fakekubeclient.NewSimpleClientset(csrs()...)
				builder.EXPECT().Build().Times(1).Return(fakeClient, nil)
				builder.EXPECT().BuildKubeClient().Times(1).Return(fakeKubeClient, nil)
			},
			setupCSRHelper: func(helper *mock.MockcsrHelper) {
				count := len(csrs())
				helper.EXPECT().IsApproved(gomock.Any()).Times(count).Return(false)
				helper.EXPECT().Parse(gomock.Any()).Times(count).Return(nil, nil)
				helper.EXPECT().Authorize(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(count).Return(nil)
				helper.EXPECT().Approve(gomock.Any(), gomock.Any()).Times(count).Return(nil)
			},
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, corev1.ConditionTrue, cond.Status)
				assert.Equal(t, hivev1.ResumingHibernationReason, cond.Reason)
			},
		},
		{
			name: "previously unsupported hibernation, now supported",
			cd:   cdBuilder.Options(o.unsupported, testcd.WithHibernateAfter(8*time.Hour)).Build(),
			cs:   csBuilder.Build(),
			validate: func(t *testing.T, cd *hivev1.ClusterDeployment) {
				cond := getHibernatingCondition(cd)
				require.NotNil(t, cond)
				assert.Equal(t, hivev1.RunningHibernationReason, cond.Reason)
				assert.Equal(t, "Hibernation capable", cond.Message)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockActuator := mock.NewMockHibernationActuator(ctrl)
			mockActuator.EXPECT().CanHandle(gomock.Any()).AnyTimes().Return(true)
			if test.setupActuator != nil {
				test.setupActuator(mockActuator)
			}
			mockBuilder := remoteclientmock.NewMockBuilder(ctrl)
			if test.setupRemote != nil {
				test.setupRemote(mockBuilder)
			}
			mockCSRHelper := mock.NewMockcsrHelper(ctrl)
			if test.setupCSRHelper != nil {
				test.setupCSRHelper(mockCSRHelper)
			}
			actuators = []HibernationActuator{mockActuator}
			c := fake.NewFakeClientWithScheme(scheme, test.cd, test.cs)

			reconciler := hibernationReconciler{
				Client: c,
				logger: log.WithField("controller", "hibernation"),
				remoteClientBuilder: func(cd *hivev1.ClusterDeployment) remoteclient.Builder {
					return mockBuilder
				},
				csrUtil: mockCSRHelper,
			}
			_, err := reconciler.Reconcile(context.TODO(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: namespace, Name: cdName},
			})

			if test.expectError {
				assert.Error(t, err, "expected error from reconcile")
			} else {
				assert.NoError(t, err, "expected no error from reconcile")
			}
			if test.validate != nil {
				cd := &hivev1.ClusterDeployment{}
				err := c.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: cdName}, cd)
				require.Nil(t, err)
				test.validate(t, cd)
			}
			ctrl.Finish()
		})
	}

}

func TestHibernateAfter(t *testing.T) {
	logger := log.New()
	logger.SetLevel(log.DebugLevel)

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	hivev1.AddToScheme(scheme)
	hiveintv1alpha1.AddToScheme(scheme)

	cdBuilder := testcd.FullBuilder(namespace, cdName, scheme).Options(
		testcd.Installed(),
		testcd.WithClusterVersion("4.4.9"),
	)
	o := clusterDeploymentOptions{}
	csBuilder := testcs.FullBuilder(namespace, cdName, scheme).Options(
		testcs.WithFirstSuccessTime(time.Now().Add(-10 * time.Hour)),
	)

	tests := []struct {
		name          string
		setupActuator func(actuator *mock.MockHibernationActuator)
		cd            *hivev1.ClusterDeployment
		cs            *hiveintv1alpha1.ClusterSync

		expectError             bool
		expectRequeueAfter      time.Duration
		expectedPowerState      hivev1.ClusterPowerState
		expectedConditionReason string
	}{
		{
			name: "cluster due for hibernate no condition", // cluster that has never been hibernated and thus has no running condition
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs:                 csBuilder.Build(),
			expectedPowerState: hivev1.HibernatingClusterPowerState,
		},
		{
			name: "cluster due for hibernate older version", // cluster that has never been hibernated and thus has no running condition
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.WithClusterVersion("4.3.11"),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs:                      csBuilder.Build(),
			expectedPowerState:      "",
			expectedConditionReason: hivev1.UnsupportedHibernationReason,
		},
		{
			name: "cluster not yet due for hibernate older version", // cluster that has never been hibernated and thus has no running condition
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.WithClusterVersion("4.3.11"),
				testcd.InstalledTimestamp(time.Now().Add(-3*time.Hour))),
			cs: csBuilder.Options(
				testcs.WithFirstSuccessTime(time.Now().Add(-3 * time.Hour)),
			).Build(),
			expectedPowerState:      "",
			expectedConditionReason: hivev1.UnsupportedHibernationReason,
		},
		{
			name: "cluster not yet due for hibernate no condition", // cluster that has never been hibernated and thus has no running condition
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(12*time.Hour),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs:                 csBuilder.Build(),
			expectRequeueAfter: 2 * time.Hour,
			expectedPowerState: "",
		},
		{
			name: "cluster with running condition due for hibernate",
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.WithCondition(hibernatingCondition(corev1.ConditionFalse, hivev1.RunningHibernationReason, 9*time.Hour)),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs:                 csBuilder.Build(),
			expectedPowerState: hivev1.HibernatingClusterPowerState,
		},
		{
			name: "cluster with running condition not due for hibernate",
			cd: cdBuilder.Build(
				testcd.WithCondition(hibernatingCondition(corev1.ConditionFalse, hivev1.RunningHibernationReason, 6*time.Hour)),
				testcd.WithHibernateAfter(20*time.Hour),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs:                 csBuilder.Build(),
			expectRequeueAfter: 14 * time.Hour,
			expectedPowerState: "",
		},
		{
			name: "cluster waking from hibernate",
			setupActuator: func(actuator *mock.MockHibernationActuator) {
				actuator.EXPECT().StartMachines(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(nil)
				actuator.EXPECT().MachinesRunning(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(false, nil)
			},
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour)),
				testcd.WithCondition(hibernatingCondition(corev1.ConditionTrue, hivev1.ResumingHibernationReason, 8*time.Hour)),
				o.shouldRun),
			cs:                 csBuilder.Build(),
			expectedPowerState: hivev1.RunningClusterPowerState,
			expectRequeueAfter: stateCheckInterval,
		},
		{
			name: "cluster due for hibernate but syncsets not applied",
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Minute),
				testcd.WithCondition(hibernatingCondition(corev1.ConditionFalse, hivev1.RunningHibernationReason, 8*time.Minute)),
				testcd.InstalledTimestamp(time.Now().Add(-8*time.Minute))),
			cs: csBuilder.Options(
				testcs.WithNoFirstSuccessTime(),
			).Build(),
			expectError:        true,
			expectedPowerState: "",
			expectRequeueAfter: time.Duration(time.Minute * 2),
		},
		{
			name: "cluster due for hibernate, syncsets not applied but 10 minutes have passed since cd install",
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.WithCondition(hibernatingCondition(corev1.ConditionFalse, hivev1.RunningHibernationReason, 9*time.Hour)),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs: csBuilder.Options(
				testcs.WithNoFirstSuccessTime(),
			).Build(),
			expectedPowerState: hivev1.HibernatingClusterPowerState,
		},
		{
			name: "cluster due for hibernate, syncsets successfully applied",
			cd: cdBuilder.Build(
				testcd.WithHibernateAfter(8*time.Hour),
				testcd.WithCondition(hibernatingCondition(corev1.ConditionFalse, hivev1.RunningHibernationReason, 9*time.Hour)),
				testcd.InstalledTimestamp(time.Now().Add(-10*time.Hour))),
			cs: csBuilder.Options(
				testcs.WithFirstSuccessTime(time.Now()),
			).Build(),
			expectedPowerState: hivev1.HibernatingClusterPowerState,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockActuator := mock.NewMockHibernationActuator(ctrl)
			mockActuator.EXPECT().CanHandle(gomock.Any()).AnyTimes().Return(true)
			if test.setupActuator != nil {
				test.setupActuator(mockActuator)
			}
			mockBuilder := remoteclientmock.NewMockBuilder(ctrl)
			mockCSRHelper := mock.NewMockcsrHelper(ctrl)
			actuators = []HibernationActuator{mockActuator}
			c := fake.NewFakeClientWithScheme(scheme, test.cd, test.cs)

			reconciler := hibernationReconciler{
				Client: c,
				logger: log.WithField("controller", "hibernation"),
				remoteClientBuilder: func(cd *hivev1.ClusterDeployment) remoteclient.Builder {
					return mockBuilder
				},
				csrUtil: mockCSRHelper,
			}
			result, err := reconciler.Reconcile(context.TODO(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: namespace, Name: cdName},
			})

			if test.expectError {
				assert.Error(t, err, "expected error from reconcile")
			} else {
				assert.NoError(t, err, "expected no error from reconcile")
			}

			// Need to do fuzzy requeue after matching
			if test.expectRequeueAfter == 0 {
				assert.Zero(t, result.RequeueAfter)
			} else {
				assert.GreaterOrEqual(t, result.RequeueAfter.Seconds(), (test.expectRequeueAfter - 10*time.Second).Seconds(), "requeue after too small")
				assert.LessOrEqual(t, result.RequeueAfter.Seconds(), (test.expectRequeueAfter + 10*time.Second).Seconds(), "request after too large")
			}

			cd := &hivev1.ClusterDeployment{}
			err = c.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: cdName}, cd)
			require.NoError(t, err, "error looking up ClusterDeployment")
			assert.Equal(t, test.expectedPowerState, cd.Spec.PowerState, "unexpected PowerState")

		})
	}
}

func hibernatingCondition(status corev1.ConditionStatus, reason string, lastTransitionAgo time.Duration) hivev1.ClusterDeploymentCondition {
	return hivev1.ClusterDeploymentCondition{
		Type:               hivev1.ClusterHibernatingCondition,
		Status:             status,
		Message:            "unused",
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-lastTransitionAgo)),
	}
}

type clusterDeploymentOptions struct{}

func (*clusterDeploymentOptions) notInstalled(cd *hivev1.ClusterDeployment) {
	cd.Spec.Installed = false
}
func (*clusterDeploymentOptions) shouldHibernate(cd *hivev1.ClusterDeployment) {
	cd.Spec.PowerState = hivev1.HibernatingClusterPowerState
}
func (*clusterDeploymentOptions) shouldRun(cd *hivev1.ClusterDeployment) {
	cd.Spec.PowerState = hivev1.RunningClusterPowerState
}
func (*clusterDeploymentOptions) stopping(cd *hivev1.ClusterDeployment) {
	cd.Status.Conditions = append(cd.Status.Conditions, hivev1.ClusterDeploymentCondition{
		Type:   hivev1.ClusterHibernatingCondition,
		Reason: hivev1.StoppingHibernationReason,
		Status: corev1.ConditionTrue,
	})
}
func (*clusterDeploymentOptions) hibernating(cd *hivev1.ClusterDeployment) {
	cd.Status.Conditions = append(cd.Status.Conditions, hivev1.ClusterDeploymentCondition{
		Type:   hivev1.ClusterHibernatingCondition,
		Reason: hivev1.HibernatingHibernationReason,
		Status: corev1.ConditionTrue,
	})
}
func (*clusterDeploymentOptions) resuming(cd *hivev1.ClusterDeployment) {
	cd.Status.Conditions = append(cd.Status.Conditions, hivev1.ClusterDeploymentCondition{
		Type:   hivev1.ClusterHibernatingCondition,
		Reason: hivev1.ResumingHibernationReason,
		Status: corev1.ConditionTrue,
	})
}
func (*clusterDeploymentOptions) unsupported(cd *hivev1.ClusterDeployment) {
	cd.Status.Conditions = append(cd.Status.Conditions, hivev1.ClusterDeploymentCondition{
		Type:   hivev1.ClusterHibernatingCondition,
		Status: corev1.ConditionFalse,
		Reason: hivev1.UnsupportedHibernationReason,
	})
}

func getHibernatingCondition(cd *hivev1.ClusterDeployment) *hivev1.ClusterDeploymentCondition {
	for i := range cd.Status.Conditions {
		if cd.Status.Conditions[i].Type == hivev1.ClusterHibernatingCondition {
			return &cd.Status.Conditions[i]
		}
	}
	return nil
}

func readyNodes() []runtime.Object {
	nodes := make([]runtime.Object, 5)
	for i := 0; i < len(nodes); i++ {
		node := &corev1.Node{}
		node.Name = fmt.Sprintf("node-%d", i)
		node.Status.Conditions = []corev1.NodeCondition{
			{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			},
		}
		nodes[i] = node
	}
	return nodes
}

func unreadyNode() []runtime.Object {
	node := &corev1.Node{}
	node.Name = "unready"
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionFalse,
		},
	}
	return append(readyNodes(), node)
}

func csrs() []runtime.Object {
	result := make([]runtime.Object, 5)
	for i := 0; i < len(result); i++ {
		csr := &certsv1beta1.CertificateSigningRequest{}
		csr.Name = fmt.Sprintf("csr-%d", i)
		result[i] = csr
	}
	return result
}
