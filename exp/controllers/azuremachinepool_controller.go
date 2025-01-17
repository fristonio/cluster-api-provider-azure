/*
Copyright 2020 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"time"

	"sigs.k8s.io/cluster-api/util/conditions"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/label"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	capierrors "sigs.k8s.io/cluster-api/errors"
	capiv1exp "sigs.k8s.io/cluster-api/exp/api/v1alpha4"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	"sigs.k8s.io/cluster-api-provider-azure/azure/scope"
	infracontroller "sigs.k8s.io/cluster-api-provider-azure/controllers"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-azure/util/reconciler"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
)

type (
	// AzureMachinePoolReconciler reconciles a AzureMachinePool object
	AzureMachinePoolReconciler struct {
		client.Client
		Log                           logr.Logger
		Scheme                        *runtime.Scheme
		Recorder                      record.EventRecorder
		ReconcileTimeout              time.Duration
		WatchFilterValue              string
		createAzureMachinePoolService azureMachinePoolServiceCreator
	}

	// annotationReaderWriter provides an interface to read and write annotations
	annotationReaderWriter interface {
		GetAnnotations() map[string]string
		SetAnnotations(annotations map[string]string)
	}
)

type azureMachinePoolServiceCreator func(machinePoolScope *scope.MachinePoolScope) (*azureMachinePoolService, error)

// NewAzureMachinePoolReconciler returns a new AzureMachinePoolReconciler instance
func NewAzureMachinePoolReconciler(client client.Client, log logr.Logger, recorder record.EventRecorder, reconcileTimeout time.Duration, watchFilterValue string) *AzureMachinePoolReconciler {
	ampr := &AzureMachinePoolReconciler{
		Client:           client,
		Log:              log,
		Recorder:         recorder,
		ReconcileTimeout: reconcileTimeout,
		WatchFilterValue: watchFilterValue,
	}

	ampr.createAzureMachinePoolService = newAzureMachinePoolService

	return ampr
}

// SetupWithManager initializes this controller with a manager.
func (r *AzureMachinePoolReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := r.Log.WithValues("controller", "AzureMachinePool")
	// create mapper to transform incoming AzureClusters into AzureMachinePool requests
	azureClusterMapper, err := AzureClusterToAzureMachinePoolsMapper(ctx, r.Client, mgr.GetScheme(), log)
	if err != nil {
		return errors.Wrap(err, "failed to create AzureCluster to AzureMachinePools mapper")
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1exp.AzureMachinePool{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		// watch for changes in CAPI MachinePool resources
		Watches(
			&source.Kind{Type: &capiv1exp.MachinePool{}},
			handler.EnqueueRequestsFromMapFunc(MachinePoolToInfrastructureMapFunc(infrav1exp.GroupVersion.WithKind("AzureMachinePool"), log)),
		).
		// watch for changes in AzureCluster resources
		Watches(
			&source.Kind{Type: &infrav1.AzureCluster{}},
			handler.EnqueueRequestsFromMapFunc(azureClusterMapper),
		).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	azureMachinePoolMapper, err := util.ClusterToObjectsMapper(r.Client, &infrav1exp.AzureMachinePoolList{}, mgr.GetScheme())
	if err != nil {
		return errors.Wrap(err, "failed to create mapper for Cluster to AzureMachines")
	}

	// Add a watch on clusterv1.Cluster object for unpause & ready notifications.
	if err := c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(azureMachinePoolMapper),
		predicates.ClusterUnpausedAndInfrastructureReady(log),
	); err != nil {
		return errors.Wrap(err, "failed adding a watch for ready clusters")
	}

	return nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=azuremachinepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=azuremachinepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools;machinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// Reconcile idempotently gets, creates, and updates a machine pool.
func (r *AzureMachinePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx, cancel := context.WithTimeout(ctx, reconciler.DefaultedLoopTimeout(r.ReconcileTimeout))
	defer cancel()
	logger := r.Log.WithValues("namespace", req.Namespace, "azureMachinePool", req.Name)

	ctx, span := tele.Tracer().Start(ctx, "controllers.AzureMachinePoolReconciler.Reconcile",
		trace.WithAttributes(
			label.String("namespace", req.Namespace),
			label.String("name", req.Name),
			label.String("kind", "AzureMachinePool"),
		))
	defer span.End()

	azMachinePool := &infrav1exp.AzureMachinePool{}
	err := r.Get(ctx, req.NamespacedName, azMachinePool)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Fetch the CAPI MachinePool.
	machinePool, err := infracontroller.GetOwnerMachinePool(ctx, r.Client, azMachinePool.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}
	if machinePool == nil {
		logger.Info("MachinePool Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("machinePool", machinePool.Name)

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machinePool.ObjectMeta)
	if err != nil {
		logger.Info("MachinePool is missing cluster label or cluster does not exist")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("cluster", cluster.Name)

	// Return early if the object or Cluster is paused.
	if annotations.IsPaused(cluster, azMachinePool) {
		logger.Info("AzureMachinePool or linked Cluster is marked as paused. Won't reconcile")
		return ctrl.Result{}, nil
	}

	azureClusterName := client.ObjectKey{
		Namespace: azMachinePool.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	azureCluster := &infrav1.AzureCluster{}
	if err := r.Client.Get(ctx, azureClusterName, azureCluster); err != nil {
		logger.Info("AzureCluster is not available yet")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("AzureCluster", azureCluster.Name)

	// Create the cluster scope
	clusterScope, err := scope.NewClusterScope(ctx, scope.ClusterScopeParams{
		Client:       r.Client,
		Logger:       logger,
		Cluster:      cluster,
		AzureCluster: azureCluster,
	})
	if err != nil {
		return reconcile.Result{}, err
	}

	// Create the machine pool scope
	machinePoolScope, err := scope.NewMachinePoolScope(scope.MachinePoolScopeParams{
		Logger:           logger,
		Client:           r.Client,
		MachinePool:      machinePool,
		AzureMachinePool: azMachinePool,
		ClusterScope:     clusterScope,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any AzureMachine changes.
	defer func() {
		if err := machinePoolScope.Close(ctx); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted machine pools
	if !azMachinePool.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, machinePoolScope, clusterScope)
	}

	// Handle non-deleted machine pools
	return r.reconcileNormal(ctx, machinePoolScope, clusterScope)
}

func (r *AzureMachinePoolReconciler) reconcileNormal(ctx context.Context, machinePoolScope *scope.MachinePoolScope, clusterScope *scope.ClusterScope) (_ reconcile.Result, reterr error) {
	ctx, span := tele.Tracer().Start(ctx, "controllers.AzureMachinePoolReconciler.reconcileNormal")
	defer span.End()

	machinePoolScope.Info("Reconciling AzureMachinePool")
	// If the AzureMachine is in an error state, return early.
	if machinePoolScope.AzureMachinePool.Status.FailureReason != nil || machinePoolScope.AzureMachinePool.Status.FailureMessage != nil {
		machinePoolScope.Info("Error state detected, skipping reconciliation")
		return reconcile.Result{}, nil
	}

	// If the AzureMachine doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(machinePoolScope.AzureMachinePool, capiv1exp.MachinePoolFinalizer)
	// Register the finalizer immediately to avoid orphaning Azure resources on delete
	if err := machinePoolScope.PatchObject(ctx); err != nil {
		return reconcile.Result{}, err
	}

	if !clusterScope.Cluster.Status.InfrastructureReady {
		machinePoolScope.Info("Cluster infrastructure is not ready yet")
		return reconcile.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	if machinePoolScope.MachinePool.Spec.Template.Spec.Bootstrap.DataSecretName == nil {
		machinePoolScope.Info("Bootstrap data secret reference is not yet available")
		return reconcile.Result{}, nil
	}

	ams, err := r.createAzureMachinePoolService(machinePoolScope)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed creating a newAzureMachinePoolService")
	}

	if err := ams.Reconcile(ctx); err != nil {
		// Handle transient and terminal errors
		var reconcileError azure.ReconcileError
		if errors.As(err, &reconcileError) {
			if reconcileError.IsTerminal() {
				machinePoolScope.Error(err, "failed to reconcile AzureMachinePool", "name", machinePoolScope.Name())
				return reconcile.Result{}, nil
			}

			if reconcileError.IsTransient() {
				machinePoolScope.Error(err, "failed to reconcile AzureMachinePool", "name", machinePoolScope.Name())
				return reconcile.Result{RequeueAfter: reconcileError.RequeueAfter()}, nil
			}

			return reconcile.Result{}, errors.Wrap(err, "failed to reconcile AzureMachinePool")
		}

		return reconcile.Result{}, err
	}

	switch machinePoolScope.ProvisioningState() {
	case infrav1.Succeeded:
		machinePoolScope.V(2).Info("Scale Set is running", "id", machinePoolScope.ProviderID())
		conditions.MarkTrue(machinePoolScope.AzureMachinePool, infrav1.ScaleSetRunningCondition)
		machinePoolScope.SetReady()
	case infrav1.Creating:
		machinePoolScope.V(2).Info("Scale Set is creating", "id", machinePoolScope.ProviderID())
		conditions.MarkFalse(machinePoolScope.AzureMachinePool, infrav1.ScaleSetRunningCondition, infrav1.ScaleSetCreatingReason, clusterv1.ConditionSeverityInfo, "")
		machinePoolScope.SetNotReady()
	case infrav1.Updating:
		machinePoolScope.V(2).Info("Scale Set is updating", "id", machinePoolScope.ProviderID())
		conditions.MarkFalse(machinePoolScope.AzureMachinePool, infrav1.ScaleSetRunningCondition, infrav1.ScaleSetUpdatingReason, clusterv1.ConditionSeverityInfo, "")
		machinePoolScope.SetNotReady()
		// we may still be scaling up, so check back in a bit
		return reconcile.Result{
			RequeueAfter: 30 * time.Second,
		}, nil
	case infrav1.Deleting:
		machinePoolScope.Info("Unexpected scale set deletion", "id", machinePoolScope.ProviderID())
		r.Recorder.Eventf(machinePoolScope.AzureMachinePool, corev1.EventTypeWarning, "UnexpectedVMDeletion", "Unexpected Azure scale set deletion")
		conditions.MarkFalse(machinePoolScope.AzureMachinePool, infrav1.VMRunningCondition, infrav1.ScaleSetDeletingReason, clusterv1.ConditionSeverityWarning, "")
		machinePoolScope.SetNotReady()
	case infrav1.Failed:
		machinePoolScope.SetNotReady()
		machinePoolScope.Error(errors.New("Failed to create or update scale set"), "Scale Set is in failed state", "id", machinePoolScope.ProviderID())
		r.Recorder.Eventf(machinePoolScope.AzureMachinePool, corev1.EventTypeWarning, "FailedVMState", "Azure scale set is in failed state")
		machinePoolScope.SetFailureReason(capierrors.UpdateMachineError)
		machinePoolScope.SetFailureMessage(errors.Errorf("Azure VM state is %s", machinePoolScope.ProvisioningState()))
		conditions.MarkFalse(machinePoolScope.AzureMachinePool, infrav1.ScaleSetRunningCondition, infrav1.ScaleSetProvisionFailedReason, clusterv1.ConditionSeverityError, "")
		// If scale set failed provisioning, delete it so it can be recreated
		err := ams.Delete(ctx)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to delete scale set in a failed state")
		}
		return reconcile.Result{}, errors.Wrap(err, "Scale set deleted, retry creating in next reconcile")
	default:
		machinePoolScope.SetNotReady()
		conditions.MarkUnknown(machinePoolScope.AzureMachinePool, infrav1.ScaleSetRunningCondition, "", "")
		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

func (r *AzureMachinePoolReconciler) reconcileDelete(ctx context.Context, machinePoolScope *scope.MachinePoolScope, clusterScope *scope.ClusterScope) (_ reconcile.Result, reterr error) {
	ctx, span := tele.Tracer().Start(ctx, "controllers.AzureMachinePoolReconciler.reconcileDelete")
	defer span.End()

	machinePoolScope.Info("Handling deleted AzureMachinePool")

	if infracontroller.ShouldDeleteIndividualResources(ctx, clusterScope) {
		amps, err := r.createAzureMachinePoolService(machinePoolScope)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed creating a new AzureMachinePoolService")
		}

		if err := amps.Delete(ctx); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "error deleting AzureCluster %s/%s", clusterScope.Namespace(), clusterScope.ClusterName())
		}
	}

	defer func() {
		if reterr == nil {
			// VM is deleted so remove the finalizer.
			controllerutil.RemoveFinalizer(machinePoolScope.AzureMachinePool, capiv1exp.MachinePoolFinalizer)
		}
	}()

	return reconcile.Result{}, nil
}
