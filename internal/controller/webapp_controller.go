// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

// Package controller implements the WebApp reconciler.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
)

// Finalizer is set on every WebApp so the operator can take the app out of
// rotation (Ingress, then Service) before garbage collection removes its pods.
const Finalizer = "apps.rijngshaffen.github.io/finalizer"

// conflictRetryDelay is how long to wait before retrying after a write was
// rejected because the object had changed in the meantime.
const conflictRetryDelay = time.Second

// WebAppReconciler reconciles a WebApp into a Deployment, Service and Ingress.
type WebAppReconciler struct {
	// Client reads from the manager's cache, which holds only the child
	// objects that carry the managed-by label (see CacheOptions).
	client.Client
	// APIReader reads straight from the API server. It is used to look at
	// objects the cache cannot see.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder

	// MaxConcurrentReconciles is the number of WebApps reconciled in
	// parallel. Zero means one.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=apps.rijngshaffen.github.io,resources=webapps,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=apps.rijngshaffen.github.io,resources=webapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.rijngshaffen.github.io,resources=webapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves the cluster towards the state described by a WebApp. It is
// level-triggered and idempotent: every call recomputes the full desired state.
func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// Something changed between being read from the cache and being
		// written back. That is routine, not a failure: try again shortly,
		// by which time the cache has caught up.
		logf.FromContext(ctx).V(1).Info("Retrying after a write conflict", "conflict", err.Error())
		return ctrl.Result{RequeueAfter: conflictRetryDelay}, nil
	}
	return ctrl.Result{}, err
}

func (r *WebAppReconciler) reconcile(ctx context.Context, req ctrl.Request) error {
	app := &webappv1alpha1.WebApp{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		return client.IgnoreNotFound(err)
	}

	if !app.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, app)
	}
	if controllerutil.AddFinalizer(app, Finalizer) {
		if err := r.Update(ctx, app); err != nil {
			return err
		}
	}

	before := app.DeepCopy()
	dep, reconcileErr := r.reconcileChildren(ctx, app)
	if apierrors.IsConflict(reconcileErr) {
		return reconcileErr
	}
	setStatus(app, dep, reconcileErr)
	if !equality.Semantic.DeepEqual(before.Status, app.Status) {
		// The optimistic lock makes the write fail if our copy of the WebApp
		// is stale, instead of overwriting a newer status with one computed
		// from old data.
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, app, patch); err != nil && reconcileErr == nil {
			return fmt.Errorf("updating status: %w", err)
		}
	}
	return reconcileErr
}

// reconcileChildren creates or updates everything a WebApp owns and returns
// the live Deployment so that status can be derived from it.
func (r *WebAppReconciler) reconcileChildren(ctx context.Context, app *webappv1alpha1.WebApp) (*appsv1.Deployment, error) {
	objMeta := metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}

	dep := &appsv1.Deployment{ObjectMeta: objMeta}
	if err := r.apply(ctx, app, dep, func() { mutateDeployment(app, dep) }); err != nil {
		return nil, err
	}

	svc := &corev1.Service{ObjectMeta: objMeta}
	if err := r.apply(ctx, app, svc, func() { mutateService(app, svc) }); err != nil {
		return nil, err
	}

	ing := &networkingv1.Ingress{ObjectMeta: objMeta}
	if app.Spec.Ingress == nil {
		if err := r.deleteIfOwned(ctx, app, ing); err != nil {
			return nil, err
		}
	} else if err := r.apply(ctx, app, ing, func() { mutateIngress(app, ing) }); err != nil {
		return nil, err
	}

	return dep, nil
}

// apply creates obj or updates it in place, and makes app its controller so
// that it is garbage collected with the WebApp and changes to it trigger a
// reconcile. It refuses to take over an object controlled by something else.
func (r *WebAppReconciler) apply(ctx context.Context, app *webappv1alpha1.WebApp, obj client.Object, mutate func()) error {
	kind := kindOf(obj)
	var before string
	mutateFn := func() error {
		before = obj.GetResourceVersion()
		mutate()
		return controllerutil.SetControllerReference(app, obj, r.Scheme)
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, mutateFn)
	if apierrors.IsAlreadyExists(err) {
		op, err = r.updateUncached(ctx, app, obj, mutateFn)
	}
	if err != nil {
		if !apierrors.IsConflict(err) {
			r.Recorder.Eventf(app, obj, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile",
				"Failed to reconcile %s %s: %v", kind, obj.GetName(), err)
		}
		return fmt.Errorf("reconciling %s: %w", kind, err)
	}
	// An update is sent whenever the desired object differs from the live
	// one, which includes differences the API server defaults away again
	// (e.g. a fieldRef without apiVersion). Such an update changes nothing and
	// leaves the resourceVersion as it was, so it is not reported.
	if op == controllerutil.OperationResultUpdated && obj.GetResourceVersion() == before {
		op = controllerutil.OperationResultNone
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("Reconciled child resource", "kind", kind, "operation", op)
		r.Recorder.Eventf(app, obj, corev1.EventTypeNormal, "Reconciled", "Reconcile",
			"%s %s %s", kind, obj.GetName(), op)
	}
	return nil
}

// updateUncached handles an object that exists in the cluster but not in the
// cache. Either the cache has not caught up with a create yet, or the object
// lacks the managed-by label: because someone removed it from one of our
// children, in which case the update puts it back, or because the name is
// taken by an object that is not ours, which is left alone.
func (r *WebAppReconciler) updateUncached(ctx context.Context, app *webappv1alpha1.WebApp, obj client.Object, mutateFn controllerutil.MutateFn) (controllerutil.OperationResult, error) {
	// Start from a blank object. Depending on the wire format, decoding into
	// one that already has fields set (such as the owner reference added
	// before the failed create) can leave them in place where the live object
	// has none, and the ownership check below must not see those.
	key := client.ObjectKeyFromObject(obj)
	reflect.ValueOf(obj).Elem().SetZero()
	if err := r.APIReader.Get(ctx, key, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	if !metav1.IsControlledBy(obj, app) {
		return controllerutil.OperationResultNone,
			fmt.Errorf("%s %q already exists and is not controlled by this WebApp", kindOf(obj), key.Name)
	}
	if err := mutateFn(); err != nil {
		return controllerutil.OperationResultNone, err
	}
	if err := r.Update(ctx, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}

// deleteIfOwned deletes the object with obj's name and namespace, provided it
// exists and is controlled by app.
func (r *WebAppReconciler) deleteIfOwned(ctx context.Context, app *webappv1alpha1.WebApp, obj client.Object) error {
	kind := kindOf(obj)
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, app) {
		return nil
	}
	if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting %s: %w", kind, err)
	}
	r.Recorder.Eventf(app, obj, corev1.EventTypeNormal, "Deleted", "Delete",
		"%s %s deleted", kind, obj.GetName())
	return nil
}

// finalize takes the app out of rotation and then releases the WebApp. The
// Deployment is left to the garbage collector via its owner reference.
func (r *WebAppReconciler) finalize(ctx context.Context, app *webappv1alpha1.WebApp) error {
	if !controllerutil.ContainsFinalizer(app, Finalizer) {
		return nil
	}

	objMeta := metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}
	for _, obj := range []client.Object{
		&networkingv1.Ingress{ObjectMeta: objMeta},
		&corev1.Service{ObjectMeta: objMeta},
	} {
		if err := r.deleteIfOwned(ctx, app, obj); err != nil {
			return err
		}
	}

	controllerutil.RemoveFinalizer(app, Finalizer)
	if err := r.Update(ctx, app); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// setStatus derives the WebApp status from the outcome of a reconcile and the
// observed state of its Deployment.
func setStatus(app *webappv1alpha1.WebApp, dep *appsv1.Deployment, reconcileErr error) {
	setCondition := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: app.Generation,
		})
	}

	if reconcileErr != nil {
		setCondition(webappv1alpha1.ConditionDegraded, metav1.ConditionTrue, "ReconcileFailed", reconcileErr.Error())
		return
	}
	setCondition(webappv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileSucceeded", "All child resources are in sync")

	var desired int32 = 1
	if app.Spec.Replicas != nil {
		desired = *app.Spec.Replicas
	}
	app.Status.ObservedGeneration = app.Generation
	app.Status.ReadyReplicas = dep.Status.ReadyReplicas
	app.Status.Selector = labels.SelectorFromSet(selectorLabels(app)).String()
	app.Status.URL = appURL(app)

	// The Deployment status is only meaningful once its controller has seen
	// the spec we just wrote.
	observed := dep.Status.ObservedGeneration >= dep.Generation
	depCondition := func(t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
		for i := range dep.Status.Conditions {
			if dep.Status.Conditions[i].Type == t {
				return &dep.Status.Conditions[i]
			}
		}
		return nil
	}

	available := depCondition(appsv1.DeploymentAvailable)
	switch {
	case desired == 0:
		setCondition(webappv1alpha1.ConditionAvailable, metav1.ConditionFalse, "ScaledToZero", "The app is scaled to zero replicas")
	case available != nil && available.Status == corev1.ConditionTrue:
		setCondition(webappv1alpha1.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable",
			fmt.Sprintf("%d/%d replicas are ready", dep.Status.ReadyReplicas, desired))
	default:
		setCondition(webappv1alpha1.ConditionAvailable, metav1.ConditionFalse, "MinimumReplicasUnavailable",
			fmt.Sprintf("%d/%d replicas are ready", dep.Status.ReadyReplicas, desired))
	}

	progressing := depCondition(appsv1.DeploymentProgressing)
	switch {
	case observed && progressing != nil && progressing.Reason == "ProgressDeadlineExceeded":
		setCondition(webappv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ProgressDeadlineExceeded", progressing.Message)
	case !observed ||
		dep.Status.UpdatedReplicas < desired ||
		dep.Status.ReadyReplicas < desired ||
		dep.Status.Replicas > desired:
		setCondition(webappv1alpha1.ConditionProgressing, metav1.ConditionTrue, "RolloutInProgress",
			fmt.Sprintf("%d/%d replicas are updated and %d are ready", dep.Status.UpdatedReplicas, desired, dep.Status.ReadyReplicas))
	default:
		setCondition(webappv1alpha1.ConditionProgressing, metav1.ConditionFalse, "RolloutComplete", "All replicas are updated and ready")
	}
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *networkingv1.Ingress:
		return "Ingress"
	}
	return fmt.Sprintf("%T", obj)
}

// SetupWithManager registers the controller. Besides WebApps it watches the
// objects they own, so manual edits or deletions of a child are reverted.
func (r *WebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&webappv1alpha1.WebApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Named("webapp").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
