package controller

import (
	"com.dm0275/secret-replicator-controller/utils"
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strconv"
	"strings"
	"time"
)

var (
	annotationKey             = "secret-replicator.fussionlabs.com"
	replicatedFromKey         = "replicated-from"
	replicationAllowedKey     = "replication-allowed"
	allowedNamespacesKey      = "allowed-namespaces"
	excludedNamespacesKey     = "excluded-namespaces"
	reconciliationIntervalKey = "reconcile-interval"
	defaultReconcileInterval  = time.Duration(5 * time.Minute)
)

type SecretReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	SecretList []types.NamespacedName
}

func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Secret{}).
		Complete(r)
}

func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var secret v1.Secret
	if err := r.Get(ctx, req.NamespacedName, &secret); err != nil {
		// Check if the secret is deleted
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Validate configmap configuration
	err := r.validateConfiguration(&secret)
	if err != nil {
		logger.Error(err, "invalid secret annotation configuration")
		return ctrl.Result{}, err
	}

	if r.replicateEnabled(&secret) {
		// If replication is enabled, add the secret to the SecretList
		r.SecretList = utils.AppendListItem(r.SecretList, req.NamespacedName)
	} else {
		return ctrl.Result{}, nil
	}

	reconciliationInterval := r.getReconciliationInterval(ctx, &secret)
	allowedNamespaces := r.getAllowedNamespaces(&secret)
	if len(allowedNamespaces) > 0 {
		for _, namespace := range allowedNamespaces {
			r.createSecret(ctx, secret, namespace)
		}
	} else {
		var namespaces v1.NamespaceList
		err = r.Client.List(ctx, &namespaces)
		if err != nil {
			logger.Error(err, "error listing namespaces")
			return ctrl.Result{RequeueAfter: reconciliationInterval}, err
		}

		excludedNamespaces := r.getExcludedNamespaces(&secret)
		for _, namespace := range namespaces.Items {
			if secret.Namespace == namespace.Name {
				logger.Info(fmt.Sprintf("secret %s in the %s namespace is a source secret", secret.Name, secret.Namespace))
				continue
			} else if utils.ListContains(excludedNamespaces, namespace.Name) {
				logger.Info(fmt.Sprintf("not replicating secret %s to namespace %s, namespace %s is an excluded namespace", secret.Name, namespace.Name, namespace.Name))
				continue
			} else {
				r.createSecret(ctx, secret, namespace.Name)
			}
		}
	}

	return ctrl.Result{RequeueAfter: reconciliationInterval}, nil
}

func (r *SecretReconciler) replicateEnabled(secret *v1.Secret) bool {
	replicationAllowed, ok := secret.Annotations[fmt.Sprintf("%s/%s", annotationKey, replicationAllowedKey)]
	if !ok {
		return false
	}

	replicationAllowedBool, err := strconv.ParseBool(replicationAllowed)
	if err != nil {
		return false
	}

	return replicationAllowedBool
}

func (r *SecretReconciler) getAllowedNamespaces(secret *v1.Secret) []string {
	allowedNamespaces, ok := secret.Annotations[fmt.Sprintf("%s/%s", annotationKey, allowedNamespacesKey)]
	if !ok {
		return []string{}
	}

	return strings.Split(allowedNamespaces, ",")
}

func (r *SecretReconciler) getExcludedNamespaces(secret *v1.Secret) []string {
	excludedNamespaces, ok := secret.Annotations[fmt.Sprintf("%s/%s", annotationKey, excludedNamespacesKey)]
	if !ok {
		return []string{}
	}

	return strings.Split(excludedNamespaces, ",")
}

func (r *SecretReconciler) validateConfiguration(secret *v1.Secret) error {
	allowedNamespaces := r.getAllowedNamespaces(secret)
	excludedNamespaces := r.getExcludedNamespaces(secret)

	if utils.SlicesOverlap(allowedNamespaces, excludedNamespaces) {
		return fmt.Errorf("unable to replicate secret %s, cannot have overlaps between allowedNamespaces and excludedNamespaces", secret.Name)
	}

	return nil
}

func (r *SecretReconciler) getReconciliationInterval(ctx context.Context, secret *v1.Secret) time.Duration {
	logger := log.FromContext(ctx)
	reconciliationInterval, ok := secret.Annotations[fmt.Sprintf("%s/%s", annotationKey, reconciliationIntervalKey)]
	if !ok {
		return defaultReconcileInterval
	}

	interval, err := time.ParseDuration(reconciliationInterval)
	if err != nil {
		logger.Error(err, "invalid reconciliation interval")
		return defaultReconcileInterval
	}

	return interval
}

func (r *SecretReconciler) createSecret(ctx context.Context, sourceSecret v1.Secret, ns string) {
	logger := log.FromContext(ctx)

	var secret v1.Secret
	getErr := r.Client.Get(ctx, client.ObjectKey{Name: sourceSecret.Name, Namespace: ns}, &secret)
	if getErr != nil && errors.IsNotFound(getErr) {
		newSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sourceSecret.Name,
				Namespace: ns,
				Annotations: map[string]string{
					fmt.Sprintf("%s/%s", annotationKey, replicatedFromKey): fmt.Sprintf("%s_%s", sourceSecret.Namespace, sourceSecret.Name),
				},
			},
			Data: sourceSecret.Data,
		}

		createErr := r.Client.Create(ctx, newSecret)
		if createErr != nil {
			logger.Error(createErr, fmt.Sprintf("error replicating secret %s to namespace %s", newSecret.Name, newSecret.Namespace))
			return
		}
	} else if getErr == nil {
		// Check if the secret is up to date
		if reflect.DeepEqual(sourceSecret.Data, secret.Data) {
			logger.Info(fmt.Sprintf("secret %s is already up-to-date in namespace %s", secret.Name, ns))
			return
		}

		secret.Data = sourceSecret.Data

		updateErr := r.Client.Update(ctx, &secret)
		if updateErr != nil {
			logger.Error(updateErr, fmt.Sprintf("error updating secret %s in namespace %s", secret.Name, secret.Namespace))
			return
		}

		logger.Info(fmt.Sprintf("updated secret %s in namespace %s", secret.Name, secret.Namespace))
		return
	} else {
		logger.Error(getErr, fmt.Sprintf("error checking if secret %s exists in namespace %s", secret.Name, secret.Namespace))
	}
	return
}
