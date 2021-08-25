// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2020-2021 Intel Corporation

package daemon

import (
	"context"
	"github.com/sirupsen/logrus"
	"reflect"
	"time"

	dh "github.com/otcshare/openshift-operator/common/pkg/drainhelper"
	"github.com/otcshare/openshift-operator/common/pkg/utils"
	sriovv2 "github.com/otcshare/openshift-operator/sriov-fec/api/v2"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ConfigurationConditionReason string

const (
	resyncPeriod                                           = time.Minute
	ConditionConfigured       string                       = "Configured"
	ConfigurationUnknown      ConfigurationConditionReason = "Unknown"
	ConfigurationInProgress   ConfigurationConditionReason = "InProgress"
	ConfigurationFailed       ConfigurationConditionReason = "Failed"
	ConfigurationNotRequested ConfigurationConditionReason = "NotRequested"
	ConfigurationSucceeded    ConfigurationConditionReason = "Succeeded"
)

type NodeConfigReconciler struct {
	client.Client
	log              *logrus.Logger
	nodeName         string
	namespace        string
	drainHelper      *dh.DrainHelper
	nodeConfigurator *NodeConfigurator
}

var (
	configPath            = "/sriov_config/config/accelerators.json"
	getSriovInventory     = GetSriovInventory
	supportedAccelerators utils.AcceleratorDiscoveryConfig
)

func NewNodeConfigReconciler(c client.Client, clientSet *clientset.Clientset, log *logrus.Logger,
	nodeName, ns string) (*NodeConfigReconciler, error) {

	var err error
	supportedAccelerators, err = utils.LoadDiscoveryConfig(configPath)
	if err != nil {
		return nil, err
	}

	kk, err := createKernelController(log)
	if err != nil {
		return nil, err
	}

	ncLogger := utils.NewLogger()
	nc := &NodeConfigurator{Log: ncLogger, kernelController: kk}

	return &NodeConfigReconciler{
		Client:           c,
		log:              log,
		nodeName:         nodeName,
		namespace:        ns,
		drainHelper:      dh.NewDrainHelper(log, clientSet, nodeName, ns),
		nodeConfigurator: nc,
	}, nil
}

func (r *NodeConfigReconciler) updateStatus(nc *sriovv2.SriovFecNodeConfig, status metav1.ConditionStatus,
	reason ConfigurationConditionReason, msg string) {

	condition := metav1.Condition{
		Type:               ConditionConfigured,
		Status:             status,
		Reason:             string(reason),
		Message:            msg,
		ObservedGeneration: nc.GetGeneration(),
	}

	inv, err := getSriovInventory(r.log)
	if err != nil {
		r.log.WithError(err).WithField("reason", condition.Reason).WithField("message", condition.Message).
			Error("failed to obtain sriov inventory for the node")
	}
	nodeStatus := sriovv2.SriovFecNodeConfigStatus{Inventory: *inv}
	meta.SetStatusCondition(&nodeStatus.Conditions, condition)

	nc.Status = nodeStatus

	if err := r.Status().Update(context.Background(), nc); err != nil {
		r.log.WithError(err).WithField("reason", condition.Reason).WithField("message", condition.Message).
			Error("failed to update SriovFecNode status")
	}

}

func (r *NodeConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return ctrl.NewControllerManagedBy(mgr).
		For(&sriovv2.SriovFecNodeConfig{}).
		WithEventFilter(
			predicate.And(
				ResourceNamePredicate{
					requiredName: r.nodeName,
					log:          r.log,
				},
				predicate.GenerationChangedPredicate{},
			),
		).Complete(r)
}

type ResourceNamePredicate struct {
	predicate.Funcs
	requiredName string
	log          *logrus.Logger
}

// Update implements default UpdateEvent filter for validating generation change
func (r ResourceNamePredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectNew.GetName() != r.requiredName {
		r.log.WithField("expected name", r.requiredName).Info("CR intended for another node - ignoring")
		return false
	}
	return true
}

func (r ResourceNamePredicate) Create(e event.CreateEvent) bool {
	if e.Object.GetName() != r.requiredName {
		r.log.WithField("expected name", r.requiredName).Info("CR intended for another node - ignoring")
		return false
	}
	return true
}

func (r *NodeConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nodeConfig := &sriovv2.SriovFecNodeConfig{}
	if err := r.Client.Get(ctx, req.NamespacedName, nodeConfig); err != nil {
		if k8serrors.IsNotFound(err) {
			r.log.Info("not found - creating")
			return reconcile.Result{}, r.CreateEmptyNodeConfigIfNeeded(r.Client)
		}
		r.log.WithError(err).Error("Get() failed")
		return reconcile.Result{}, err
	}

	skipStatusUpdate := false

	inv, err := getSriovInventory(r.log)
	if err != nil {
		r.log.WithError(err).Error("failed to obtain sriov inventory for the node")
		r.updateStatus(nodeConfig, metav1.ConditionFalse, ConfigurationFailed, err.Error())
		return reconcile.Result{}, err
	}

	currentCondition := meta.FindStatusCondition(nodeConfig.Status.Conditions, ConditionConfigured)
	if currentCondition != nil {
		if !reflect.DeepEqual(*inv, nodeConfig.Status.Inventory) {
			r.log.Info("updating inventory")
			r.updateStatus(nodeConfig, metav1.ConditionTrue, ConfigurationConditionReason(currentCondition.Reason), currentCondition.Message)
			return reconcile.Result{RequeueAfter: resyncPeriod}, nil
		}

		if currentCondition.ObservedGeneration == nodeConfig.GetGeneration() {
			return reconcile.Result{RequeueAfter: resyncPeriod}, nil
		}

		r.updateStatus(nodeConfig, metav1.ConditionFalse, ConfigurationInProgress, "Configuration started")
	}

	if len(nodeConfig.Spec.PhysicalFunctions) == 0 {
		r.log.Info("Nothing to do")
		r.updateStatus(nodeConfig, metav1.ConditionFalse, ConfigurationNotRequested, "Inventory up to date")
		return reconcile.Result{RequeueAfter: resyncPeriod}, nil
	}

	var configurationErr, dhErr error

	dhErr = r.drainHelper.Run(func(c context.Context) bool {
		missingParams, err := r.nodeConfigurator.isAnyKernelParamsMissing()
		if err != nil {
			r.log.WithError(err).Error("failed to check for missing params")
			configurationErr = err
			return true
		}

		if missingParams {
			r.log.Info("missing kernel params")

			err := r.nodeConfigurator.addMissingKernelParams()
			if err != nil {
				r.log.WithError(err).Error("failed to add missing params")
				configurationErr = err
				return true
			}

			r.log.Info("added kernel params - rebooting")
			if err := r.nodeConfigurator.rebootNode(); err != nil {
				r.log.WithError(err).Error("failed to request a node reboot")
				configurationErr = err
				return true
			}
			skipStatusUpdate = true
			return false // leave node cordoned & keep the leadership
		}
		if err := r.nodeConfigurator.applyConfig(nodeConfig.Spec); err != nil {
			r.log.WithError(err).Error("failed applying new PF/VF configuration")
			configurationErr = err
			return true
		}

		configurationErr = r.restartDevicePlugin()
		return true
	}, !nodeConfig.Spec.DrainSkip)

	if skipStatusUpdate {
		r.log.Info("status update skipped - CR will be handled again after node reboot")
		return reconcile.Result{}, nil
	}

	if dhErr != nil {
		r.log.WithError(dhErr).Error("drainhelper returned an error")
		r.updateStatus(nodeConfig, metav1.ConditionFalse, ConfigurationUnknown, dhErr.Error())
		return reconcile.Result{}, err
	}

	if configurationErr != nil {
		r.log.WithError(configurationErr).Error("error during configuration")
		r.updateStatus(nodeConfig, metav1.ConditionFalse, ConfigurationFailed, configurationErr.Error())
		return reconcile.Result{}, err
	}

	nodeConfigCurrent := &sriovv2.SriovFecNodeConfig{}
	if err := r.Client.Get(ctx, req.NamespacedName, nodeConfigCurrent); err != nil {
		r.log.WithError(err).Error("Get() failed")
		return reconcile.Result{}, err
	}

	r.updateStatus(nodeConfig, metav1.ConditionTrue, ConfigurationSucceeded, "Configured successfully")
	r.log.Info("Reconciled")

	return reconcile.Result{RequeueAfter: resyncPeriod}, nil
}

func (r *NodeConfigReconciler) restartDevicePlugin() error {
	pods := &corev1.PodList{}
	err := r.Client.List(context.TODO(), pods,
		client.InNamespace(r.namespace),
		&client.MatchingLabels{"app": "sriov-device-plugin-daemonset"})

	if err != nil {
		return errors.Wrap(err, "failed to get pods")
	}
	if len(pods.Items) == 0 {
		return errors.New("restartDevicePlugin: No pods found")
	}

	for _, p := range pods.Items {
		if p.Spec.NodeName != r.nodeName {
			continue
		}
		d := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: p.Namespace,
				Name:      p.Name,
			},
		}
		if err := r.Delete(context.TODO(), d, &client.DeleteOptions{}); err != nil {
			return errors.Wrap(err, "failed to delete sriov-device-plugin-daemonset pod")
		}

	}

	return nil
}

// CreateEmptyNodeConfigIfNeeded creates empty CR to be Reconciled in near future and filled with Status.
// If invoked before manager's Start, it'll need a direct API client
// (Manager's/Controller's client is cached and cache is not initialized yet).
func (r *NodeConfigReconciler) CreateEmptyNodeConfigIfNeeded(c client.Client) error {
	nodeConfig := &sriovv2.SriovFecNodeConfig{}
	err := c.Get(context.Background(),
		client.ObjectKey{
			Name:      r.nodeName,
			Namespace: r.namespace,
		},
		nodeConfig)

	if err == nil {
		r.log.Info("already exists")
		return nil
	}

	if !k8serrors.IsNotFound(err) {
		return err
	}

	r.log.Info("not found - creating")

	nodeConfig = &sriovv2.SriovFecNodeConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.nodeName,
			Namespace: r.namespace,
		},
		Spec: sriovv2.SriovFecNodeConfigSpec{
			PhysicalFunctions: []sriovv2.PhysicalFunctionConfigExt{},
		},
	}

	if createErr := c.Create(context.Background(), nodeConfig); createErr != nil {
		r.log.WithError(createErr).Error("failed to create")
		return createErr
	}

	updateErr := c.Status().Update(context.Background(), nodeConfig)
	if updateErr != nil {
		r.log.WithError(updateErr).Error(updateErr, "failed to update status")
	}
	return updateErr

}
