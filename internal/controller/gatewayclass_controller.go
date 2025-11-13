package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/georgetaylor/cloudflare-gateway-controller/internal/cloudflare"
)

// GatewayClassReconciler reconciles a GatewayClass object
type GatewayClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GatewayClass instance
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, gatewayClass); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if this GatewayClass is for this controller
	if gatewayClass.Spec.ControllerName != controllerName {
		logger.Info("gatewayclass not owned by this controller", "controller", gatewayClass.Spec.ControllerName)
		return ctrl.Result{}, nil
	}

	// Validate ParametersRef if present
	if gatewayClass.Spec.ParametersRef != nil {
		if err := r.validateParametersRef(ctx, gatewayClass); err != nil {
			logger.Error(err, "invalid parametersRef")
			r.setGatewayClassCondition(gatewayClass, metav1.ConditionFalse,
				gatewayv1.GatewayClassReasonInvalidParameters, err.Error())
			_ = r.Status().Update(ctx, gatewayClass)
			return ctrl.Result{}, err
		}
	} else {
		// No parametersRef means no credentials
		r.setGatewayClassCondition(gatewayClass, metav1.ConditionFalse,
			gatewayv1.GatewayClassReasonInvalidParameters, "ParametersRef is required for Cloudflare credentials")
		_ = r.Status().Update(ctx, gatewayClass)
		return ctrl.Result{}, fmt.Errorf("parametersRef is required")
	}

	// Mark as accepted
	r.setGatewayClassCondition(gatewayClass, metav1.ConditionTrue,
		gatewayv1.GatewayClassReasonAccepted, "GatewayClass accepted")

	if err := r.Status().Update(ctx, gatewayClass); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GatewayClassReconciler) validateParametersRef(ctx context.Context, gatewayClass *gatewayv1.GatewayClass) error {
	logger := log.FromContext(ctx)

	// Check that parametersRef is a Secret
	if gatewayClass.Spec.ParametersRef.Group != "" && gatewayClass.Spec.ParametersRef.Group != "core" && gatewayClass.Spec.ParametersRef.Group != "v1" {
		return fmt.Errorf("parametersRef must reference a Secret (group: core or v1)")
	}

	if gatewayClass.Spec.ParametersRef.Kind != "Secret" {
		return fmt.Errorf("parametersRef must reference a Secret (kind: Secret)")
	}

	// Get the Secret
	secretName := gatewayClass.Spec.ParametersRef.Name
	secretNamespace := "cloudflare-gateway-system" // Default namespace
	if gatewayClass.Spec.ParametersRef.Namespace != nil {
		secretNamespace = string(*gatewayClass.Spec.ParametersRef.Namespace)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}, secret); err != nil {
		return fmt.Errorf("failed to get credentials secret: %w", err)
	}

	// Validate required fields
	apiToken := string(secret.Data["api-token"])
	accountID := string(secret.Data["account-id"])

	if apiToken == "" {
		return fmt.Errorf("credentials secret missing 'api-token' field")
	}

	if accountID == "" {
		return fmt.Errorf("credentials secret missing 'account-id' field")
	}

	// Test the credentials by creating a Cloudflare client
	cfClient, err := cloudflare.NewClient(apiToken, accountID)
	if err != nil {
		return fmt.Errorf("failed to create Cloudflare client: %w", err)
	}

	// Perform a simple API call to verify credentials work
	// We'll try to get the zone list (or account info)
	// For now, we'll just check if the client was created successfully
	_ = cfClient // TODO: Add actual validation API call

	logger.Info("validated Cloudflare credentials", "secretName", secretName, "namespace", secretNamespace)

	return nil
}

func (r *GatewayClassReconciler) setGatewayClassCondition(gatewayClass *gatewayv1.GatewayClass,
	status metav1.ConditionStatus, reason gatewayv1.GatewayClassConditionReason, message string) {

	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             status,
		ObservedGeneration: gatewayClass.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(reason),
		Message:            message,
	}

	meta.SetStatusCondition(&gatewayClass.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{}).
		Complete(r)
}
