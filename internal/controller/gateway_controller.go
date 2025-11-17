package controller

import (
	"context"
	"crypto/md5"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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
	Scheme        *runtime.Scheme
	ClusterDomain string
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
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Fetch the latest version of the Gateway
			latestGateway := &gatewayv1.Gateway{}
			if err := r.Get(ctx, req.NamespacedName, latestGateway); err != nil {
				return err
			}
			if !controllerutil.ContainsFinalizer(latestGateway, gatewayFinalizer) {
				controllerutil.AddFinalizer(latestGateway, gatewayFinalizer)
				return r.Update(ctx, latestGateway)
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch the gateway after update
		if err := r.Get(ctx, req.NamespacedName, gateway); err != nil {
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

	// Validate listeners configuration
	if err := r.validateListeners(gateway); err != nil {
		logger.Error(err, "invalid listeners configuration")
		r.setGatewayCondition(gateway, gatewayv1.GatewayConditionAccepted, metav1.ConditionFalse,
			gatewayv1.GatewayReasonListenersNotValid, err.Error())
		_ = r.Status().Update(ctx, gateway)
		return ctrl.Result{}, err
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

	// Check if tunnel already exists by checking if Secret exists
	secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)
	existingSecret := &corev1.Secret{}
	secretExists := true
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: gateway.Namespace}, existingSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing tunnel secret: %w", err)
		}
		secretExists = false
	}

	// Create tunnel if it doesn't exist
	var tunnelID, tunnelSecret, tunnelName, accountID string
	if !secretExists {
		// Create new tunnel
		name := fmt.Sprintf("%s-%s", gateway.Namespace, gateway.Name)
		tunnel, secret, err := cfClient.CreateTunnel(ctx, name)
		if err != nil {
			return fmt.Errorf("failed to create tunnel: %w", err)
		}

		tunnelID = tunnel.ID
		tunnelSecret = secret
		tunnelName = tunnel.Name
		accountID = cfClient.AccountID()

		logger.Info("created cloudflare tunnel", "tunnelID", tunnelID, "name", tunnelName)
	} else {
		// Read existing tunnel info from Secret
		tunnelID = string(existingSecret.Data["tunnel-id"])
		tunnelSecret = string(existingSecret.Data["tunnel-secret"])
		tunnelName = string(existingSecret.Data["tunnel-name"])
		accountID = string(existingSecret.Data["account-id"])
	}

	// Create or update credentials Secret
	if err := r.reconcileCredentialsSecret(ctx, gateway, tunnelID, tunnelSecret, tunnelName, accountID); err != nil {
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

	// Update Gateway ConfigMap with all HTTPRoute rules
	if err := r.updateGatewayConfigFromRoutes(ctx, gateway); err != nil {
		logger.Error(err, "failed to update gateway config from routes")
		// Don't fail reconciliation - config will be updated when routes change
	}

	// Set the tunnel hostname for status update
	tunnelHostname := fmt.Sprintf("%s.cfargotunnel.com", tunnelID)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version of the Gateway for status update
		latestGateway := &gatewayv1.Gateway{}
		if err := r.Get(ctx, types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}, latestGateway); err != nil {
			return err
		}

		// Update status fields
		r.setGatewayCondition(latestGateway, gatewayv1.GatewayConditionAccepted, metav1.ConditionTrue,
			gatewayv1.GatewayReasonAccepted, "Gateway accepted")
		r.setGatewayCondition(latestGateway, gatewayv1.GatewayConditionProgrammed, metav1.ConditionTrue,
			gatewayv1.GatewayReasonProgrammed, "Gateway programmed")

		// Update listener status
		if err := r.updateListenerStatus(ctx, latestGateway); err != nil {
			logger.Error(err, "failed to update listener status")
			// Don't fail on listener status errors
		}

		latestGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  ptrTo(gatewayv1.HostnameAddressType),
				Value: tunnelHostname,
			},
		}

		return r.Status().Update(ctx, latestGateway)
	})
}

// updateListenerStatus counts attached routes per listener and updates status
func (r *GatewayReconciler) updateListenerStatus(ctx context.Context, gateway *gatewayv1.Gateway) error {
	// Get all HTTPRoutes for this Gateway
	routes, err := r.getAllHTTPRoutesForGateway(ctx, gateway)
	if err != nil {
		return err
	}

	// Initialize listener status
	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))

	for i, listener := range gateway.Spec.Listeners {
		// Count routes attached to this listener
		attachedCount := int32(0)
		for _, route := range routes {
			if r.routeMatchesListener(&route, gateway, listener) {
				attachedCount++
			}
		}

		gateway.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: ptrTo(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: attachedCount,
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
			},
		}
	}

	return nil
}

// routeMatchesListener checks if an HTTPRoute matches a specific listener
func (r *GatewayReconciler) routeMatchesListener(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, listener gatewayv1.Listener) bool {
	// Check if route references this gateway
	for _, parentRef := range route.Spec.ParentRefs {
		parentNamespace := route.Namespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}

		// Must reference this gateway
		if string(parentRef.Name) != gateway.Name || parentNamespace != gateway.Namespace {
			continue
		}

		// If sectionName specified, must match listener name
		if parentRef.SectionName != nil {
			if *parentRef.SectionName == listener.Name {
				return true
			}
			continue
		}

		// If port specified, must match listener port
		if parentRef.Port != nil {
			if *parentRef.Port == listener.Port {
				return true
			}
			continue
		}

		// No sectionName or port - matches any HTTP/HTTPS listener
		if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType {
			return true
		}
	}

	return false
}

func (r *GatewayReconciler) reconcileCredentialsSecret(ctx context.Context, gateway *gatewayv1.Gateway, tunnelID, tunnelSecret, tunnelName, accountID string) error {
	secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)

	// Use retry logic to handle concurrent modifications
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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

			// Store all tunnel metadata in Secret
			secret.Data["tunnel-id"] = []byte(tunnelID)
			secret.Data["tunnel-secret"] = []byte(tunnelSecret)
			secret.Data["account-id"] = []byte(accountID)
			secret.Data["tunnel-name"] = []byte(tunnelName)

			// Generate credentials JSON
			generator := cloudflare.NewConfigGenerator()
			credentialsJSON := generator.GenerateCredentialsJSON(accountID, tunnelID, tunnelSecret)
			secret.Data["credentials.json"] = []byte(credentialsJSON)
			secret.Type = corev1.SecretTypeOpaque

			return controllerutil.SetControllerReference(gateway, secret, r.Scheme)
		})

		return err
	})
}

func (r *GatewayReconciler) reconcileConfigMap(ctx context.Context, gateway *gatewayv1.Gateway) error {
	configMapName := fmt.Sprintf("%s-tunnel-config", gateway.Name)

	// Use retry logic to handle concurrent modifications
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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
				// Get tunnel ID from Secret
				secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)
				tunnelSecret := &corev1.Secret{}
				if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: gateway.Namespace}, tunnelSecret); err != nil {
					return fmt.Errorf("failed to get tunnel secret: %w", err)
				}
				tunnelID := string(tunnelSecret.Data["tunnel-id"])
				if tunnelID == "" {
					return fmt.Errorf("tunnel-id not found in secret")
				}

				// Generate initial config with just a 404 catch-all
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
	})
}

func (r *GatewayReconciler) reconcileDeployment(ctx context.Context, gateway *gatewayv1.Gateway) error {
	deploymentName := fmt.Sprintf("%s-cloudflared", gateway.Name)

	// Use retry logic to handle concurrent modifications
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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
	})
}

// getAllHTTPRoutesForGateway lists all HTTPRoutes that reference this Gateway
func (r *GatewayReconciler) getAllHTTPRoutesForGateway(ctx context.Context, gateway *gatewayv1.Gateway) ([]gatewayv1.HTTPRoute, error) {
	// List all HTTPRoutes across all namespaces
	routeList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routeList); err != nil {
		return nil, err
	}

	var matchingRoutes []gatewayv1.HTTPRoute

	for _, route := range routeList.Items {
		// Check if this route references our gateway
		for _, parentRef := range route.Spec.ParentRefs {
			parentNamespace := route.Namespace // default to route's namespace
			if parentRef.Namespace != nil {
				parentNamespace = string(*parentRef.Namespace)
			}
			if string(parentRef.Name) == gateway.Name && parentNamespace == gateway.Namespace {
				matchingRoutes = append(matchingRoutes, route)
				break
			}
		}
	}

	return matchingRoutes, nil
}

// buildIngressRulesForRoute builds cloudflared ingress rules for a single HTTPRoute
func (r *GatewayReconciler) buildIngressRulesForRoute(ctx context.Context, route *gatewayv1.HTTPRoute) ([]cloudflare.IngressRule, error) {
	builder := cloudflare.NewIngressRuleBuilder()

	// Get hostnames from the route
	hostnames := route.Spec.Hostnames
	if len(hostnames) == 0 {
		// Skip routes with no hostnames
		return nil, nil
	}

	// Process each rule in the HTTPRoute
	for _, rule := range route.Spec.Rules {
		// Get the first backend (for simplicity, we only support one backend per rule)
		if len(rule.BackendRefs) == 0 {
			continue
		}

		backendRef := rule.BackendRefs[0]

		// Resolve backend Service
		serviceName := string(backendRef.Name)
		serviceNamespace := route.Namespace
		if backendRef.Namespace != nil {
			serviceNamespace = string(*backendRef.Namespace)
		}

		// Get Service to verify it exists
		service := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      serviceName,
			Namespace: serviceNamespace,
		}, service); err != nil {
			// Skip this rule if service doesn't exist
			continue
		}

		// Determine the port
		var port int32
		if backendRef.Port != nil {
			port = int32(*backendRef.Port)
		} else if len(service.Spec.Ports) > 0 {
			port = service.Spec.Ports[0].Port
		} else {
			continue
		}

		serviceURL := cloudflare.ServiceURL(serviceName, serviceNamespace, port, r.ClusterDomain)

		// Check if service uses HTTPS (via annotation or port)
		scheme := "http"
		if route.Annotations["cloudflare-gateway-controller.github.com/backend-protocol"] == "https" {
			scheme = "https"
		} else if port == 443 || port == 8443 {
			// Auto-detect HTTPS for common HTTPS ports
			scheme = "https"
		}

		if scheme == "https" {
			serviceURL = cloudflare.ServiceURLWithScheme("https", serviceName, serviceNamespace, port, r.ClusterDomain)
		}

		// Build origin request config from annotations
		originRequest := r.buildOriginRequestConfigForRoute(route, scheme)

		// Create ingress rules for each hostname and path match
		for _, hostname := range hostnames {
			if len(rule.Matches) == 0 {
				// No matches means match all paths for this hostname
				builder.AddRule(string(hostname), "", serviceURL, originRequest)
			} else {
				// Create a rule for each path match
				for _, match := range rule.Matches {
					path := ""
					if match.Path != nil && match.Path.Value != nil {
						path = *match.Path.Value
					}
					builder.AddRule(string(hostname), path, serviceURL, originRequest)
				}
			}
		}
	}

	return builder.Build(), nil
}

// updateGatewayConfigFromRoutes aggregates all HTTPRoutes and updates the Gateway's ConfigMap
func (r *GatewayReconciler) updateGatewayConfigFromRoutes(ctx context.Context, gateway *gatewayv1.Gateway) error {
	logger := log.FromContext(ctx)

	// Get all HTTPRoutes for this Gateway
	routes, err := r.getAllHTTPRoutesForGateway(ctx, gateway)
	if err != nil {
		return fmt.Errorf("failed to list HTTPRoutes: %w", err)
	}

	// Detect conflicts and build ingress rules from non-conflicted routes
	allRules, conflicts := r.detectConflictsAndBuildRules(ctx, routes)

	// Update conflicted routes' status
	for routeKey, conflict := range conflicts {
		for _, route := range routes {
			key := fmt.Sprintf("%s/%s", route.Namespace, route.Name)
			if key == routeKey {
				// This route is conflicted - update its status
				r.updateConflictedRouteStatus(ctx, &route, conflict)
				break
			}
		}
	}

	// Get tunnel ID from Secret
	secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)
	tunnelSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: gateway.Namespace}, tunnelSecret); err != nil {
		return fmt.Errorf("failed to get tunnel secret: %w", err)
	}
	tunnelID := string(tunnelSecret.Data["tunnel-id"])
	if tunnelID == "" {
		return fmt.Errorf("tunnel-id not found in secret")
	}

	// Generate new config
	generator := cloudflare.NewConfigGenerator()
	config, err := generator.GenerateConfig(tunnelID, allRules)
	if err != nil {
		return fmt.Errorf("failed to generate config: %w", err)
	}

	// Update ConfigMap
	configMapName := fmt.Sprintf("%s-tunnel-config", gateway.Name)
	configMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: gateway.Namespace,
	}, configMap); err != nil {
		return fmt.Errorf("failed to get configmap: %w", err)
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version of the ConfigMap
		latestConfigMap := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      configMapName,
			Namespace: gateway.Namespace,
		}, latestConfigMap); err != nil {
			return err
		}
		latestConfigMap.Data["config.yaml"] = config
		return r.Update(ctx, latestConfigMap)
	}); err != nil {
		return fmt.Errorf("failed to update configmap: %w", err)
	}

	logger.Info("updated gateway config", "routes", len(routes), "rules", len(allRules))

	// Update deployment annotation to trigger rollout when config changes
	if err := r.updateDeploymentConfigHash(ctx, gateway, config); err != nil {
		logger.Error(err, "failed to update deployment config hash")
		// Don't fail the reconciliation - config is updated, deployment will pick it up eventually
	}

	return nil
}

// updateDeploymentConfigHash updates the cloudflared deployment with a hash of the config
// This triggers an automatic rollout when the config changes
func (r *GatewayReconciler) updateDeploymentConfigHash(ctx context.Context, gateway *gatewayv1.Gateway, config string) error {
	deploymentName := fmt.Sprintf("%s-cloudflared", gateway.Name)

	// Calculate hash of the config
	configHash := fmt.Sprintf("%x", md5.Sum([]byte(config)))[:10]

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      deploymentName,
			Namespace: gateway.Namespace,
		}, deployment); err != nil {
			return err
		}

		// Update the pod template annotation to trigger a rollout
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		deployment.Spec.Template.Annotations["cloudflare-gateway-controller/config-hash"] = configHash

		return r.Update(ctx, deployment)
	})
}

// routeConflict represents a conflict between HTTPRoutes
type routeConflict struct {
	hostname     string
	path         string
	conflictWith string // namespace/name of conflicting route
}

// detectConflictsAndBuildRules builds rules and detects conflicts
// Returns rules from non-conflicted routes and a map of conflicts
func (r *GatewayReconciler) detectConflictsAndBuildRules(ctx context.Context, routes []gatewayv1.HTTPRoute) ([]cloudflare.IngressRule, map[string]routeConflict) {
	logger := log.FromContext(ctx)

	// Track hostname+path to route mapping
	type routeKey struct {
		hostname string
		path     string
	}
	routeMap := make(map[routeKey][]string)     // maps hostname+path to list of "namespace/name"
	conflicts := make(map[string]routeConflict) // maps "namespace/name" to conflict info

	var allRules []cloudflare.IngressRule

	// First pass: build rules and detect conflicts
	for _, route := range routes {
		routeName := fmt.Sprintf("%s/%s", route.Namespace, route.Name)
		rules, err := r.buildIngressRulesForRoute(ctx, &route)
		if err != nil {
			logger.Error(err, "failed to build ingress rules for route", "route", route.Name)
			continue
		}

		// Track which hostname+path combinations this route claims
		for _, rule := range rules {
			key := routeKey{
				hostname: rule.Hostname,
				path:     rule.Path,
			}
			routeMap[key] = append(routeMap[key], routeName)
		}
	}

	// Second pass: identify conflicts (any hostname+path with >1 route)
	for key, routeNames := range routeMap {
		if len(routeNames) > 1 {
			// Conflict! Mark all routes claiming this hostname+path as conflicted
			for _, routeName := range routeNames {
				otherRoutes := make([]string, 0, len(routeNames)-1)
				for _, other := range routeNames {
					if other != routeName {
						otherRoutes = append(otherRoutes, other)
					}
				}
				conflicts[routeName] = routeConflict{
					hostname:     key.hostname,
					path:         key.path,
					conflictWith: strings.Join(otherRoutes, ", "),
				}
				logger.Info("route conflict detected - all conflicting routes will be rejected",
					"route", routeName,
					"conflictsWith", otherRoutes,
					"hostname", key.hostname,
					"path", key.path)
			}
		}
	}

	// Third pass: add rules only from non-conflicted routes
	for _, route := range routes {
		routeName := fmt.Sprintf("%s/%s", route.Namespace, route.Name)
		if _, isConflicted := conflicts[routeName]; !isConflicted {
			rules, err := r.buildIngressRulesForRoute(ctx, &route)
			if err != nil {
				continue
			}
			allRules = append(allRules, rules...)
		}
	}

	return allRules, conflicts
}

// updateConflictedRouteStatus updates the status of a conflicted route
func (r *GatewayReconciler) updateConflictedRouteStatus(ctx context.Context, route *gatewayv1.HTTPRoute, conflict routeConflict) {
	logger := log.FromContext(ctx)

	// Find the parent ref index (just use first one for simplicity)
	if len(route.Spec.ParentRefs) == 0 {
		return
	}

	// Get the latest version of the route
	latestRoute := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      route.Name,
		Namespace: route.Namespace,
	}, latestRoute); err != nil {
		logger.Error(err, "failed to get route for status update", "route", route.Name)
		return
	}

	// Set conflicted status condition
	message := fmt.Sprintf("Route rejected due to conflict with %s for hostname=%s path=%s. All conflicting routes are rejected until the conflict is resolved.",
		conflict.conflictWith, conflict.hostname, conflict.path)

	// Update status for first parent (simplified - should update all parents)
	if len(latestRoute.Status.Parents) == 0 {
		latestRoute.Status.Parents = make([]gatewayv1.RouteParentStatus, 1)
		latestRoute.Status.Parents[0] = gatewayv1.RouteParentStatus{
			ParentRef:      latestRoute.Spec.ParentRefs[0],
			ControllerName: controllerName,
		}
	}

	condition := metav1.Condition{
		Type:               string(gatewayv1.RouteConditionAccepted),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: latestRoute.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "Conflicted",
		Message:            message,
	}

	meta.SetStatusCondition(&latestRoute.Status.Parents[0].Conditions, condition)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch to get the latest version
		currentRoute := &gatewayv1.HTTPRoute{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      route.Name,
			Namespace: route.Namespace,
		}, currentRoute); err != nil {
			return err
		}

		// Re-apply the status update
		if len(currentRoute.Status.Parents) == 0 {
			currentRoute.Status.Parents = make([]gatewayv1.RouteParentStatus, 1)
			currentRoute.Status.Parents[0] = gatewayv1.RouteParentStatus{
				ParentRef:      latestRoute.Spec.ParentRefs[0],
				ControllerName: controllerName,
			}
		}
		meta.SetStatusCondition(&currentRoute.Status.Parents[0].Conditions, condition)
		return r.Status().Update(ctx, currentRoute)
	}); err != nil {
		logger.Error(err, "failed to update conflicted route status", "route", route.Name)
	}
}

// buildOriginRequestConfigForRoute constructs OriginRequestConfig from HTTPRoute annotations
func (r *GatewayReconciler) buildOriginRequestConfigForRoute(route *gatewayv1.HTTPRoute, scheme string) *cloudflare.OriginRequestConfig {
	annotations := route.Annotations
	if annotations == nil {
		return nil
	}

	// Legacy individual annotation support
	config := &cloudflare.OriginRequestConfig{}
	hasConfig := false

	// Simple helper to set string fields
	setString := func(dest *string, key string) {
		if val := annotations[key]; val != "" {
			*dest = val
			hasConfig = true
		}
	}

	// Simple helper to set bool fields
	setBool := func(dest *bool, key string) {
		if annotations[key] == "true" {
			*dest = true
			hasConfig = true
		}
	}

	// TLS Settings
	setString(&config.OriginServerName, "cloudflare-gateway-controller.github.com/origin-server-name")
	setBool(&config.MatchSNItoHost, "cloudflare-gateway-controller.github.com/match-sni-to-host")
	setString(&config.CAPool, "cloudflare-gateway-controller.github.com/ca-pool")
	setString(&config.TLSTimeout, "cloudflare-gateway-controller.github.com/tls-timeout")
	setBool(&config.HTTP2Origin, "cloudflare-gateway-controller.github.com/http2-origin")

	if annotations["cloudflare-gateway-controller.github.com/no-tls-verify"] == "true" {
		config.NoTLSVerify = true
		hasConfig = true
	} else if scheme == "https" && annotations["cloudflare-gateway-controller.github.com/no-tls-verify"] == "" {
		// Default to noTLSVerify for HTTPS backends (self-signed certs)
		config.NoTLSVerify = true
		hasConfig = true
	}

	// HTTP Settings
	setString(&config.HTTPHostHeader, "cloudflare-gateway-controller.github.com/http-host-header")
	setBool(&config.DisableChunkedEncoding, "cloudflare-gateway-controller.github.com/disable-chunked-encoding")

	// Connection Settings
	setString(&config.ConnectTimeout, "cloudflare-gateway-controller.github.com/connect-timeout")
	setBool(&config.NoHappyEyeballs, "cloudflare-gateway-controller.github.com/no-happy-eyeballs")
	setString(&config.KeepAliveTimeout, "cloudflare-gateway-controller.github.com/keep-alive-timeout")
	setBool(&config.TCPKeepAlive, "cloudflare-gateway-controller.github.com/tcp-keep-alive")

	// Access Settings
	setBool(&config.Required, "cloudflare-gateway-controller.github.com/access-required")

	if !hasConfig {
		return nil
	}

	return config
}

func (r *GatewayReconciler) handleDeletion(ctx context.Context, gateway *gatewayv1.Gateway) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(gateway, gatewayFinalizer) {
		return ctrl.Result{}, nil
	}

	// Get tunnel ID from Secret
	secretName := fmt.Sprintf("%s-tunnel-credentials", gateway.Name)
	tunnelSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: gateway.Namespace}, tunnelSecret); err == nil {
		tunnelID := string(tunnelSecret.Data["tunnel-id"])
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
	}

	// Remove finalizer with retry
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version of the Gateway
		latestGateway := &gatewayv1.Gateway{}
		if err := r.Get(ctx, types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}, latestGateway); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(latestGateway, gatewayFinalizer) {
			controllerutil.RemoveFinalizer(latestGateway, gatewayFinalizer)
			return r.Update(ctx, latestGateway)
		}
		return nil
	}); err != nil {
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

func (r *GatewayReconciler) validateListeners(gateway *gatewayv1.Gateway) error {
	if len(gateway.Spec.Listeners) == 0 {
		return fmt.Errorf("gateway must have at least one listener")
	}

	for i, listener := range gateway.Spec.Listeners {
		// Validate protocol - only HTTP and HTTPS are supported for Cloudflare Tunnels
		switch listener.Protocol {
		case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
			// Supported protocols
		case gatewayv1.TLSProtocolType, gatewayv1.TCPProtocolType, gatewayv1.UDPProtocolType:
			return fmt.Errorf("listener[%d]: protocol %s not supported (only HTTP and HTTPS)", i, listener.Protocol)
		default:
			return fmt.Errorf("listener[%d]: unknown protocol %s", i, listener.Protocol)
		}

		// Validate port is set
		if listener.Port == 0 {
			return fmt.Errorf("listener[%d]: port must be specified", i)
		}

		// HTTPS listeners should have TLS configuration
		if listener.Protocol == gatewayv1.HTTPSProtocolType {
			if listener.TLS == nil {
				return fmt.Errorf("listener[%d]: HTTPS protocol requires TLS configuration", i)
			}
			// Note: For Cloudflare Tunnels, TLS is terminated at the edge
			// But we validate the config is present for spec compliance
		}

		// Validate listener name is set
		if listener.Name == "" {
			return fmt.Errorf("listener[%d]: name must be specified", i)
		}
	}

	return nil
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

// findGatewaysForHTTPRoute finds all Gateways that an HTTPRoute references
func (r *GatewayReconciler) findGatewaysForHTTPRoute(ctx context.Context, obj client.Object) []ctrl.Request {
	route := obj.(*gatewayv1.HTTPRoute)

	var requests []ctrl.Request
	for _, parentRef := range route.Spec.ParentRefs {
		// Determine Gateway namespace
		gatewayNamespace := route.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}

		// Add reconcile request for this Gateway
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      string(parentRef.Name),
				Namespace: gatewayNamespace,
			},
		})
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.findGatewaysForHTTPRoute),
		).
		Complete(r)
}

func ptrTo[T any](v T) *T {
	return &v
}
