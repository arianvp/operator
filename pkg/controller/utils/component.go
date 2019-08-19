package utils

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	"github.com/tigera/operator/pkg/render"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type ComponentHandler interface {
	CreateOrUpdate(context.Context, render.Component) error
}

func NewComponentHandler(log logr.Logger, client client.Client, scheme *runtime.Scheme, cr metav1.Object) ComponentHandler {
	return &componentHandler{
		client: client,
		scheme: scheme,
		cr:     cr,
		log:    log,
	}
}

type componentHandler struct {
	client client.Client
	scheme *runtime.Scheme
	cr     metav1.Object
	log    logr.Logger
}

func (c componentHandler) CreateOrUpdate(ctx context.Context, component render.Component) error {
	// Before creating the component, make sure that it is ready. This provides a hook to do
	// dependency checking for the component.
	cmpLog := c.log.WithValues("component", reflect.TypeOf(component))
	cmpLog.V(2).Info("Checking if component is ready")
	if !component.Ready() {
		cmpLog.Info("Component is not ready, skipping")
		return nil
	}
	cmpLog.V(2).Info("Reconciling")

	// Iterate through each object that comprises the component and attempt to create it,
	// or update it if needed.
	for _, obj := range component.Objects() {
		// Set CR instance as the owner and controller.
		if err := controllerutil.SetControllerReference(c.cr, obj.(metav1.ObjectMetaAccessor).GetObjectMeta(), c.scheme); err != nil {
			return err
		}

		logCtx := ContextLoggerForResource(c.log, obj)
		var old runtime.Object = obj.DeepCopyObject()
		var key client.ObjectKey
		key, err := client.ObjectKeyFromObject(obj)
		if err != nil {
			return err
		}

		// Check to see if the object exists or not.
		err = c.client.Get(ctx, key, old)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				// Anything other than "Not found" we should retry.
				return err
			}

			// Otherwise, if it was not found, we should create it and move on.
			logCtx.V(2).Info("Object does not exist, creating it", "error", err)
			err = c.client.Create(ctx, obj)
			if err != nil {
				return err
			}
			continue
		}

		// The object exists. Update it, unless the user has marked it as "ignored".
		if IgnoreObject(old) {
			logCtx.Info("Ignoring annotated object")
			continue
		}
		logCtx.V(1).Info("Resource already exists, update it")
		err = c.client.Update(ctx, mergeState(obj, old))
		if err != nil {
			logCtx.WithValues("key", key).Info("Failed to update object.")
			return err
		}
		continue
	}
	cmpLog.Info("Done reconciling component")
	return nil
}

// mergeState returns the object to pass to Update given the current and desired object states.
func mergeState(desired, current runtime.Object) runtime.Object {
	switch desired.(type) {
	case *v1.Service:
		// Services are a special case since some fields (namely ClusterIP) are defaulted
		// and we need to maintain them on updates.
		oldRV := current.(metav1.ObjectMetaAccessor).GetObjectMeta().GetResourceVersion()
		desired.(metav1.ObjectMetaAccessor).GetObjectMeta().SetResourceVersion(oldRV)
		cs := current.(*v1.Service)
		ds := desired.(*v1.Service)
		ds.Spec.ClusterIP = cs.Spec.ClusterIP
		return ds
	case *batchv1.Job:
		// Jobs have controller-uid values added to spec.selector and spec.template.metadata.labels.
		// spec.selector and podtemplatespec are immutable so just copy real values over to desired state.
		oldRV := current.(metav1.ObjectMetaAccessor).GetObjectMeta().GetResourceVersion()
		desired.(metav1.ObjectMetaAccessor).GetObjectMeta().SetResourceVersion(oldRV)
		cj := current.(*batchv1.Job)
		dj := desired.(*batchv1.Job)
		dj.Spec.Selector = cj.Spec.Selector
		dj.Spec.Template = cj.Spec.Template
		return dj
	default:
		// Default to just using the desired state, with an updated RV.
		oldRV := current.(metav1.ObjectMetaAccessor).GetObjectMeta().GetResourceVersion()
		desired.(metav1.ObjectMetaAccessor).GetObjectMeta().SetResourceVersion(oldRV)
		return desired
	}
}