/*
Copyright 2020 The KubeSphere Authors.

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

package tekton

import (
	"context"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	tknclient "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devopsv2alpha1 "kubesphere.io/devops/pkg/api/devops/v2alpha1"
)

// PipelineRunReconciler reconciles a PipelineRun object
type PipelineRunReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	TknClientset *tknclient.Clientset
}

//+kubebuilder:rbac:groups=devops.kubesphere.io,resources=pipelineruns,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=devops.kubesphere.io,resources=pipelineruns/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=devops.kubesphere.io,resources=pipelineruns/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PipelineRunReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	// First, we get the pipelinerun resource
	pipelineRun := &devopsv2alpha1.PipelineRun{}
	if err := r.Get(ctx, req.NamespacedName, pipelineRun); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Second, we check whether the pipeline object is being deleted,
	// by examining DeletionTimestamp field in it.
	pipelineRunFinalizerName := devopsv2alpha1.PipelineRunFinalizerName
	if pipelineRun.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(pipelineRun.GetFinalizers(), pipelineRunFinalizerName) {
			controllerutil.AddFinalizer(pipelineRun, pipelineRunFinalizerName)
			if err := r.Update(ctx, pipelineRun); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted.
		if containsString(pipelineRun.GetFinalizers(), pipelineRunFinalizerName) {
			// Our finalizer is present, so lets handle any external dependency.
			if err := r.deleteExternalResources(ctx, pipelineRun); err != nil {
				// If fail to delete the external dependency here, return with error.
				// So that it can be retried.
				return ctrl.Result{}, err
			}

			// Remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(pipelineRun, pipelineRunFinalizerName)
			if err := r.Update(ctx, pipelineRun); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Third, we continue to the core logic of creating Tekton PipelineRun.
	if err := r.reconcileTektonCrd(ctx, req.Namespace, pipelineRun); err != nil {
		klog.Error(err, "Failed to reconcile Tekton PipelineRun resources.")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devopsv2alpha1.PipelineRun{}).
		Complete(r)
}

// deleteExternalResources deletes any external resources associated with the devopsv2alpha1.Pipeline
func (r *PipelineRunReconciler) deleteExternalResources(ctx context.Context, pipelineRun *devopsv2alpha1.PipelineRun) error {
	tknPipelineRunName := pipelineRun.Spec.Name
	klog.Infof("PipelineRun [%s] is under deletion.", tknPipelineRunName)

	// We will first find the target Tekton PipelineRun CRD resources in the given
	// namespace. If we do not find it, we will return directly.
	if _, err := r.TknClientset.TektonV1beta1().
		PipelineRuns(pipelineRun.Namespace).
		Get(ctx, tknPipelineRunName, metav1.GetOptions{}); err != nil {
		// Tekton PipelineRun resource does not exist, so we just do nothing here.
		klog.V(5).Infof("unable to find Tekton PipelineRun [%s] in namespace %s", tknPipelineRunName, pipelineRun.Namespace)
		return nil
	}

	// If we find that target Tekton PipelineRun resource exists,
	// we should delete it and its corresponding resources,
	// e.g. Tekton TaskRuns and Pods created by it.
	if err := r.TknClientset.TektonV1beta1().
		PipelineRuns(pipelineRun.Namespace).
		Delete(ctx, tknPipelineRunName, metav1.DeleteOptions{}); err != nil {
		// When we failed to delete tekton pipelinerun, return with an error.
		klog.Errorf("unable to delete Tekton PipelineRun [%s]", tknPipelineRunName)
		return err
	}

	klog.Infof("PipelineRun [%s] was deleted successfully.", tknPipelineRunName)

	return nil
}

// reconcileTektonCrd translates our crd to Tekton crd
func (r *PipelineRunReconciler) reconcileTektonCrd(ctx context.Context, namespace string, pipelineRun *devopsv2alpha1.PipelineRun) error {
	return r.reconcileTektonPipelineRun(ctx, namespace, &pipelineRun.Spec)
}

// reconcileTektonPipelineRun translates our PipelineRun to Tekton PipelineRun
func (r *PipelineRunReconciler) reconcileTektonPipelineRun(ctx context.Context, namespace string, pipelineRun *devopsv2alpha1.PipelineRunSpec) error {
	// translate PipelineRun to Tekton PipelineRun
	tPipelineRun := &tektonv1.PipelineRun{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pipelineRun.Name}, tPipelineRun); err != nil {
		// This means the current Tekton PipelineRun does not exist in the given namespace,
		// i.e. we can safely create it.

		// set up struct of tekton pipelinerun
		tektonPipelineRun := &tektonv1.PipelineRun{
			TypeMeta:   metav1.TypeMeta{Kind: "PipelineRun", APIVersion: "tekton.dev/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: pipelineRun.Name, Namespace: namespace},
			Spec: tektonv1.PipelineRunSpec{
				PipelineRef: &tektonv1.PipelineRef{Name: pipelineRun.PipelineRef},
			},
		}

		// create tekton pipelinerun resource
		if err := r.Create(ctx, tektonPipelineRun); err != nil {
			return err
		}

		// log if create successfully
		klog.Infof("Tekton PipelineRun [%s] was created successfully.", pipelineRun.Name)
	} else {
		// This means that a Tekton PipelineRun resource has already exists in the given namespace,
		// which can be a problem.
		klog.Infof("Tekton PipelineRun [%s] already exists!", pipelineRun.Name)
	}

	return nil
}
