/*


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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	srov1beta1 "github.com/openshift/special-resource-operator/api/v1beta1"
	"github.com/openshift/special-resource-operator/internal/controllers/state"
	"github.com/openshift/special-resource-operator/pkg/clients"
	"github.com/openshift/special-resource-operator/pkg/cluster"
	"github.com/openshift/special-resource-operator/pkg/helmer"
	"github.com/openshift/special-resource-operator/pkg/preflight"
	"github.com/openshift/special-resource-operator/pkg/runtime"
)

const reconcileRequeueInSeconds = 60

// ClusterPreflightReconciler reconciles a PreflightValidation object
type PreflightValidationReconciler struct {
	ClusterAPI    cluster.Cluster
	Helmer        helmer.Helmer
	PreflightAPI  preflight.PreflightAPI
	RuntimeAPI    runtime.RuntimeAPI
	StatusUpdater state.StatusUpdater
	KubeClient    clients.ClientsInterface
}

func (r *PreflightValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("preflightvalidation").
		For(&srov1beta1.PreflightValidation{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
		}).
		Complete(r)
}

// Reconcile Reconiliation entry point
func (r *PreflightValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	reconRes := ctrl.Result{}

	log := ctrl.LoggerFrom(ctx)
	log.Info("ClusterPreflightController Request")

	pv := srov1beta1.PreflightValidation{}
	err := r.KubeClient.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pv)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("PreflightValidation not found - probably deleted. Not reconciling")
			return reconRes, nil
		} else {
			log.Error(err, "preflight validation reconcile failed to find object")
			return reconRes, err
		}
	}

	if pv.GetDeletionTimestamp() != nil {
		log.Info("CR is marked for deletion, skipping preflight")
		return reconRes, nil
	}

	reconCompleted, err := r.runPreflightValidation(ctx, &pv)
	if err != nil {
		log.Error(err, "runPreflightValidation failed")
		return reconRes, err
	}

	if reconCompleted {
		log.Info("Reconciliation successful for preflight")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: time.Second * reconcileRequeueInSeconds}, nil
}

func (r *PreflightValidationReconciler) runPreflightValidation(ctx context.Context, pv *srov1beta1.PreflightValidation) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	specialresources := &srov1beta1.SpecialResourceList{}

	srStatuses := r.getSRStatusesMap(pv)

	runInfo, err := r.PreflightAPI.PrepareRuntimeInfo(ctx, pv.Spec.UpdateImage)
	if err != nil {
		log.Info("Failed to get runtime info from image", "image", pv.Spec.UpdateImage)
		return false, err
	}

	err = r.KubeClient.List(ctx, specialresources)
	if err != nil {
		log.Error(err, "Failed to get list of all SRs")
		return false, fmt.Errorf("failed to get list of all SRs, %s", err)
	}

	err = r.presetStatusesForCRs(ctx, specialresources, pv)
	if err != nil {
		return false, err
	}

	for _, sr := range specialresources.Items {
		if sr.Name == "special-resource-preamble" {
			continue
		}
		if sr.GetDeletionTimestamp() != nil {
			log.Info("CR is marked for deletion, skipping preflight", "specialresource", sr.Name)
			continue
		}

		if status, ok := srStatuses[sr.Name]; ok {
			if status.VerificationStatus == srov1beta1.VerificationTrue {
				continue
			}
		}
		verified, message, err := r.PreflightAPI.PreflightUpgradeCheck(ctx, &sr, runInfo)
		if err != nil {
			log.Error(err, "Failure during Preflight Upgrade check")
		}

		r.updatePreflightStatus(ctx, pv, sr.Name, message, verified, err)
	}

	return r.checkPreflightCompletion(ctx, pv.Name, pv.Namespace)
}

func (r *PreflightValidationReconciler) getSRStatusesMap(pv *srov1beta1.PreflightValidation) map[string]srov1beta1.SRStatus {
	statusMap := make(map[string]srov1beta1.SRStatus, len(pv.Status.SRStatuses))
	for _, status := range pv.Status.SRStatuses {
		statusMap[status.Name] = status
	}

	return statusMap
}

func (r *PreflightValidationReconciler) updatePreflightStatus(ctx context.Context, pv *srov1beta1.PreflightValidation, crName, message string, verified bool, err error) {
	var verificationStatus string
	switch {
	case err != nil:
		verificationStatus = srov1beta1.VerificationError
	case verified:
		verificationStatus = srov1beta1.VerificationTrue
	default:
		verificationStatus = srov1beta1.VerificationFalse
	}
	srStatus := r.getPreflightSRStatus(pv, crName)
	errUpdate := r.StatusUpdater.SetVerificationStatus(ctx, pv, srStatus, verificationStatus, message)
	if errUpdate != nil {
		ctrl.LoggerFrom(ctx).Error(errUpdate, "failed to update the status of SR CR in preflight", "specialresource", crName)
	}
}

func (r *PreflightValidationReconciler) presetStatusesForCRs(ctx context.Context, specialresources *srov1beta1.SpecialResourceList, pv *srov1beta1.PreflightValidation) error {
	for _, sr := range specialresources.Items {
		if sr.Name == "special-resource-preamble" {
			continue
		}
		srStatus := r.getPreflightSRStatus(pv, sr.Name)
		if srStatus.VerificationStatus == "" {
			err := r.StatusUpdater.SetVerificationStatus(ctx, pv, srStatus, srov1beta1.VerificationUnknown, preflight.VerificationStatusReasonUnknown)
			if err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "failed to set SR CR status to unknown", "specialresource", sr.Name)
				return err
			}
		}
	}
	return nil
}

func (r *PreflightValidationReconciler) checkPreflightCompletion(ctx context.Context, name, namespace string) (bool, error) {
	pv := srov1beta1.PreflightValidation{}
	err := r.KubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pv)
	if err != nil {
		return false, err
	}

	for _, srStatus := range pv.Status.SRStatuses {
		if srStatus.VerificationStatus != srov1beta1.VerificationTrue {
			ctrl.LoggerFrom(ctx).Info("at least one SR CR is not verified yet", "specialresource", srStatus.Name, "status", srStatus.VerificationStatus)
			return false, nil
		}
	}

	return true, nil
}

func (r *PreflightValidationReconciler) getPreflightSRStatus(pv *srov1beta1.PreflightValidation, crName string) *srov1beta1.SRStatus {
	for i := range pv.Status.SRStatuses {
		if pv.Status.SRStatuses[i].Name == crName {
			return &pv.Status.SRStatuses[i]
		}
	}
	pv.Status.SRStatuses = append(pv.Status.SRStatuses, srov1beta1.SRStatus{Name: crName})
	return &pv.Status.SRStatuses[len(pv.Status.SRStatuses)-1]
}
