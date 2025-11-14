package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/georgetaylor/cloudflare-gateway-controller/internal/cloudflare"
)

const (
	controllerName   = "george.dev/cloudflare-gateway-controller"
	gatewayFinalizer = controllerName
)

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Gateway instance
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if the Gateway is being deleted
	if !gateway.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, gateway)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(gateway, gatewayFinalizer) {
		controllerutil.AddFinalizer(gateway, gatewayFinalizer)
		if err := r.Update(ctx, gateway); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, gatewayClass); err != nil {
		logger.Error(err, "failed to get GatewayClass")
		r.setGatewayCondition(gateway, gatewayv1.GatewayConditionAccepted, metav1.ConditionFalse,
			gatewayv1.GatewayReasonInvalid, "GatewayClass not found")
		_ = r.Status().Update(ctx, gateway)
		return ctrl.Result{}, err
	}

	// Verify this controller owns the GatewayClass
	if gatewayClass.Spec.ControllerName != controllerName {
		logger.Info("gateway class not owned by this controller", "controller", gatewayClass.Spec.ControllerName)
		return ctrl.Result{}, nil
	}

	// Get Cloudflare credentials from GatewayClass parameters
	cfClient, err := r.getCloudflareClient(ctx, gatewayClass)
	if err != nil {
		logger.Error(err, "failed to get Cloudflare client")
		r.setGatewayCondition(gateway, gatewayv1.GatewayConditionAccepted, metav1.ConditionFalse,
			gatewayv1.GatewayReasonInvalid, "Failed to get Cloudflare credentials")
		_ = r.Status().Update(ctx, gateway)
		return ctrl.Result{}, err
	}

	// Reconcile the Gateway
	if err := r.reconcileGateway(ctx, gateway, cfClient); err != nil {
		logger.Error(err, "failed to reconcile gateway")
		r.setGatewayCondition(gateway, gatewayv1.GatewayConditionProgrammed, metav1.ConditionFalse,
			gatewayv1.GatewayReasonInvalid, err.Error())
		_ = r.Status().Update(ctx, gateway)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) reconcileGateway(ctx context.Context, gateway *gatewayv1.Gateway, cfClient *cloudflare.Client) error {
	logger := log.FromContext(ctx)

	// Check if tunnel already exists (stored in annotation)
	tunnelID := gateway.Annotations["george.dev/tunnel-id"]

	if tunnelID == "" {
		// Create new tunnel
		tunnelName := fmt.Sprintf("%s-%s", gateway.Namespace, gateway.Name)
		tunnel, secret, err := cfClient.CreateTunnel(ctx, tunnelName)
		if err != nil {
			return fmt.Errorf("failed to create tunnel: %w", err)
		}

		tunnelID = tunnel.ID
		logger.Info("created cloudflare tunnel", "tunnelID", tunnelID, "name", tunnelName)

		// Store tunnel information in annotations
		if gateway.Annotations == nil {
			gateway.Annotations = make(map[string]string)
		}
		gateway.Annotations["george.dev/tunnel-id"] = tunnelID
		gateway.Annotations["george.dev/tunnel-secret"] = secret
		gateway.Annotations["george.dev/tunnel-name"] = tunnel.Name
		gateway.Annotations["george.dev/account-id"] = cfClient.AccountID()

		if err := r.Update(ctx, gateway); err != nil {
			// Try to clean up the tunnel if we can't update the gateway
			_ = cfClient.DeleteTunnel(ctx, tunnelID)
			return fmt.Errorf("failed to update gateway with tunnel info: %w", err)
		}
	}

	// Create or update credentials Secret
	if err := r.reconcileCredentialsSecret(ctx, gateway, cfClient); err != nil {
		return fmt.Errorf("failed to reconcile credentials secret: %w", err)
	}

	// Create or update ConfigMap with tunnel configuration
	if err := r.reconcileConfigMap(ctx, gateway); err != nil {
		return fmt.Errorf("failed to reconcile configmap: %w", err)
	}

	// Create or update cloudflared Deployment
	if err := r.reconcileDeployment(ctx, gateway); err != nil {
		return fmt.Errorf("failed to reconcile deployment: %w", err)
	}

	// Update Gateway status
	r.setGatewayCondition(gateway, gatewayv1.GatewayConditionAccepted, metav1.ConditionTrue,
		gatewayv1.GatewayReasonAccepted, "Gateway accepted")
	r.setGatewayCondition(gateway, gatewayv1.GatewayConditionProgrammed, metav1.ConditionTrue,
		gatewayv1.GatewayReasonProgrammed, "Gateway programmed")

	// Set the tunnel hostname as the gateway address
	tunnelID = gateway.Annotations["george.dev/tunnel-id"]
	tunnelHostname := fmt.Sprintf("%s.cfargotunnel.com", tunnelID)

	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{
			Type:  ptrTo(gatewayv1.HostnameAddressType),
			Value: tunnelHostname,
		},
	}

	return r.Status().Update(ctx, gateway)
}

func (r *GatewayReconciler) reconcileCredentialsSecret(ctx context.Context, gateway *gatewayv1.Gateway, cfClient *cloudflare.Client) error {
	secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: gateway.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}

		// Generate credentials JSON
		tunnelID := gateway.Annotations["george.dev/tunnel-id"]
		tunnelSecret := gateway.Annotations["george.dev/tunnel-secret"]
		accountID := gateway.Annotations["george.dev/account-id"]

		generator := cloudflare.NewConfigGenerator()
		credentialsJSON := generator.GenerateCredentialsJSON(accountID, tunnelID, tunnelSecret)

		secret.Data["credentials.json"] = []byte(credentialsJSON)
		secret.Type = corev1.SecretTypeOpaque

		return controllerutil.SetControllerReference(gateway, secret, r.Scheme)
	})

	return err
}

func (r *GatewayReconciler) reconcileConfigMap(ctx context.Context, gateway *gatewayv1.Gateway) error {
	configMapName := fmt.Sprintf("%s-tunnel-config", gateway.Name)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: gateway.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}

		// Only initialize config if it doesn't exist yet
		// HTTPRoute controller will update it with actual routes
		if _, exists := configMap.Data["config.yaml"]; !exists {
			// Generate initial config with just a 404 catch-all
			tunnelID := gateway.Annotations["george.dev/tunnel-id"]
			generator := cloudflare.NewConfigGenerator()
			config, err := generator.GenerateConfig(tunnelID, []cloudflare.IngressRule{})
			if err != nil {
				return err
			}

			configMap.Data["config.yaml"] = config
		}

		return controllerutil.SetControllerReference(gateway, configMap, r.Scheme)
	})

	return err
}

func (r *GatewayReconciler) reconcileDeployment(ctx context.Context, gateway *gatewayv1.Gateway) error {
	deploymentName := fmt.Sprintf("%s-cloudflared", gateway.Name)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: gateway.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		replicas := int32(2)
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":     "cloudflared",
					"gateway": gateway.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":     "cloudflared",
						"gateway": gateway.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "cloudflared",
							Image: "cloudflare/cloudflared:latest",
							Args: []string{
								"tunnel",
								"--config",
								"/etc/cloudflared/config.yaml",
								"--no-autoupdate",
								"run",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/etc/cloudflared/config.yaml",
									SubPath:   "config.yaml",
									ReadOnly:  true,
								},
								{
									Name:      "credentials",
									MountPath: "/etc/cloudflared/credentials.json",
									SubPath:   "credentials.json",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: fmt.Sprintf("%s-tunnel-config", gateway.Name),
									},
								},
							},
						},
						{
							Name: "credentials",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: fmt.Sprintf("%s-tunnel-credentials", gateway.Name),
								},
							},
						},
					},
				},
			},
		}

		return controllerutil.SetControllerReference(gateway, deployment, r.Scheme)
	})

	return err
}

func (r *GatewayReconciler) handleDeletion(ctx context.Context, gateway *gatewayv1.Gateway) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(gateway, gatewayFinalizer) {
		return ctrl.Result{}, nil
	}

	// Get tunnel ID from annotations
	tunnelID := gateway.Annotations["george.dev/tunnel-id"]
	if tunnelID != "" {
		// Get Cloudflare client
		gatewayClass := &gatewayv1.GatewayClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, gatewayClass); err == nil {
			if cfClient, err := r.getCloudflareClient(ctx, gatewayClass); err == nil {
				// Delete the tunnel
				if err := cfClient.DeleteTunnel(ctx, tunnelID); err != nil {
					logger.Error(err, "failed to delete cloudflare tunnel", "tunnelID", tunnelID)
					// Continue with finalizer removal even if tunnel deletion fails
				} else {
					logger.Info("deleted cloudflare tunnel", "tunnelID", tunnelID)
				}
			}
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(gateway, gatewayFinalizer)
	if err := r.Update(ctx, gateway); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) getCloudflareClient(ctx context.Context, gatewayClass *gatewayv1.GatewayClass) (*cloudflare.Client, error) {
	if gatewayClass.Spec.ParametersRef == nil {
		return nil, fmt.Errorf("gatewayClass has no parametersRef")
	}

	// Assuming parametersRef points to a Secret in the same namespace as the controller
	secretName := gatewayClass.Spec.ParametersRef.Name
	secretNamespace := "cloudflare-gateway-system" // TODO: make this configurable
	if gatewayClass.Spec.ParametersRef.Namespace != nil {
		secretNamespace = string(*gatewayClass.Spec.ParametersRef.Namespace)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret: %w", err)
	}

	apiToken := string(secret.Data["api-token"])
	accountID := string(secret.Data["account-id"])

	if apiToken == "" || accountID == "" {
		return nil, fmt.Errorf("credentials secret missing api-token or account-id")
	}

	return cloudflare.NewClient(apiToken, accountID)
}

func (r *GatewayReconciler) setGatewayCondition(gateway *gatewayv1.Gateway, condType gatewayv1.GatewayConditionType,
	status metav1.ConditionStatus, reason gatewayv1.GatewayConditionReason, message string) {

	condition := metav1.Condition{
		Type:               string(condType),
		Status:             status,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(reason),
		Message:            message,
	}

	meta.SetStatusCondition(&gateway.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func ptrTo[T any](v T) *T {
	return &v
}
