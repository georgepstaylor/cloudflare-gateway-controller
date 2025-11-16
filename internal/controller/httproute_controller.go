package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"gopkg.in/yaml.v3"

	"github.com/georgetaylor/cloudflare-gateway-controller/internal/cloudflare"
)

const (
	httprouteFinalizer = "george.dev/cloudflare-gateway-controller"
)

// HTTPRouteReconciler reconciles a HTTPRoute object
type HTTPRouteReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ClusterDomain string
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HTTPRoute instance
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if the HTTPRoute is being deleted
	if !route.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, route)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(route, httprouteFinalizer) {
		controllerutil.AddFinalizer(route, httprouteFinalizer)
		if err := r.Update(ctx, route); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Process each parent Gateway reference
	for i, parentRef := range route.Spec.ParentRefs {
		if err := r.reconcileParentGateway(ctx, route, parentRef, i); err != nil {
			logger.Error(err, "failed to reconcile parent gateway", "parent", parentRef.Name)
			// Continue processing other parents even if one fails
		}
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) reconcileParentGateway(ctx context.Context, route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference, parentIndex int) error {
	logger := log.FromContext(ctx)

	// Resolve parent Gateway
	gatewayNamespace := route.Namespace
	if parentRef.Namespace != nil {
		gatewayNamespace = string(*parentRef.Namespace)
	}

	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      string(parentRef.Name),
		Namespace: gatewayNamespace,
	}, gateway); err != nil {
		r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionAccepted, metav1.ConditionFalse,
			gatewayv1.RouteReasonNoMatchingParent, "Parent Gateway not found")
		_ = r.Status().Update(ctx, route)
		return fmt.Errorf("failed to get gateway: %w", err)
	}

	// Verify Gateway is owned by this controller
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, gatewayClass); err != nil {
		return fmt.Errorf("failed to get gatewayclass: %w", err)
	}

	if gatewayClass.Spec.ControllerName != controllerName {
		logger.Info("gateway not owned by this controller", "gateway", gateway.Name)
		return nil
	}

	// Validate HTTPRoute matches a listener
	if err := r.validateRouteMatchesListener(route, gateway, parentRef); err != nil {
		r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionAccepted, metav1.ConditionFalse,
			gatewayv1.RouteReasonNotAllowedByListeners, err.Error())
		_ = r.Status().Update(ctx, route)
		return err
	}

	// Get Cloudflare client
	cfClient, err := r.getCloudflareClient(ctx, gatewayClass)
	if err != nil {
		r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionAccepted, metav1.ConditionFalse,
			gatewayv1.RouteReasonNoMatchingParent, "Failed to get Cloudflare credentials")
		_ = r.Status().Update(ctx, route)
		return fmt.Errorf("failed to get cloudflare client: %w", err)
	}

	// Validate backend references by building ingress rules (don't actually use them)
	_, err = r.buildIngressRules(ctx, route)
	if err != nil {
		r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionResolvedRefs, metav1.ConditionFalse,
			gatewayv1.RouteReasonBackendNotFound, err.Error())
		_ = r.Status().Update(ctx, route)
		return fmt.Errorf("failed to resolve backend refs: %w", err)
	}

	// Create DNS records for each hostname
	if err := r.reconcileDNSRecords(ctx, route, gateway, cfClient); err != nil {
		logger.Error(err, "failed to reconcile DNS records")
		// Don't fail the reconciliation if DNS fails - it's not critical
	}

	// Update HTTPRoute status
	r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionAccepted, metav1.ConditionTrue,
		gatewayv1.RouteReasonAccepted, "Route accepted")
	r.setRouteCondition(route, parentIndex, gatewayv1.RouteConditionResolvedRefs, metav1.ConditionTrue,
		gatewayv1.RouteReasonResolvedRefs, "All references resolved")

	// Note: Gateway controller will update its ConfigMap when it reconciles
	// due to the watch on HTTPRoute changes

	return r.Status().Update(ctx, route)
}

func (r *HTTPRouteReconciler) buildIngressRules(ctx context.Context, route *gatewayv1.HTTPRoute) ([]cloudflare.IngressRule, error) {
	builder := cloudflare.NewIngressRuleBuilder()

	// Get hostnames from the route
	hostnames := route.Spec.Hostnames
	if len(hostnames) == 0 {
		// If no hostname specified, we can't create ingress rules
		return nil, fmt.Errorf("no hostnames specified in HTTPRoute")
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
			return nil, fmt.Errorf("failed to get backend service %s/%s: %w", serviceNamespace, serviceName, err)
		}

		// Determine the port
		var port int32
		if backendRef.Port != nil {
			port = int32(*backendRef.Port)
		} else if len(service.Spec.Ports) > 0 {
			port = service.Spec.Ports[0].Port
		} else {
			return nil, fmt.Errorf("no port found for service %s/%s", serviceNamespace, serviceName)
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
		originRequest := r.buildOriginRequestConfig(route, scheme)

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

func (r *HTTPRouteReconciler) reconcileDNSRecords(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, cfClient *cloudflare.Client) error {
	logger := log.FromContext(ctx)

	// Get tunnel hostname from Gateway status
	if len(gateway.Status.Addresses) == 0 {
		return fmt.Errorf("gateway has no addresses")
	}
	tunnelHostname := gateway.Status.Addresses[0].Value

	// Create DNS records for each hostname
	for _, hostname := range route.Spec.Hostnames {
		hostnameStr := string(hostname)

		// Extract zone from hostname (e.g., example.com from api.example.com)
		zoneName := extractZone(hostnameStr)

		// Get zone ID
		zoneID, err := cfClient.GetZoneIDByName(ctx, zoneName)
		if err != nil {
			logger.Error(err, "failed to get zone ID", "zone", zoneName)
			continue
		}

		// Check if DNS record already exists
		records, err := cfClient.ListDNSRecords(ctx, zoneID, hostnameStr, "CNAME")
		if err != nil {
			logger.Error(err, "failed to list DNS records", "hostname", hostnameStr)
			continue
		}

		if len(records) > 0 {
			// Record exists, check if it needs updating
			record := records[0]
			if record.Content != tunnelHostname {
				// Update the record
				err := cfClient.UpdateDNSRecord(ctx, zoneID, record.ID, cloudflare.DNSRecordParams{
					Name:    hostnameStr,
					Type:    "CNAME",
					Content: tunnelHostname,
					Proxied: true,
					TTL:     1, // Auto TTL when proxied
				})
				if err != nil {
					logger.Error(err, "failed to update DNS record", "hostname", hostnameStr)
				} else {
					logger.Info("updated DNS record", "hostname", hostnameStr, "target", tunnelHostname)
				}
			}
		} else {
			// Create new record
			_, err := cfClient.CreateDNSRecord(ctx, cloudflare.DNSRecordParams{
				ZoneID:  zoneID,
				Name:    hostnameStr,
				Type:    "CNAME",
				Content: tunnelHostname,
				Proxied: true,
				TTL:     1, // Auto TTL when proxied
			})
			if err != nil {
				logger.Error(err, "failed to create DNS record", "hostname", hostnameStr)
			} else {
				logger.Info("created DNS record", "hostname", hostnameStr, "target", tunnelHostname)
			}
		}
	}

	return nil
}

func (r *HTTPRouteReconciler) handleDeletion(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(route, httprouteFinalizer) {
		return ctrl.Result{}, nil
	}

	// TODO: Remove DNS records
	// TODO: Update Gateway ConfigMap to remove this route's rules

	logger.Info("cleaning up httproute", "name", route.Name)

	// Remove finalizer
	controllerutil.RemoveFinalizer(route, httprouteFinalizer)
	if err := r.Update(ctx, route); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) getCloudflareClient(ctx context.Context, gatewayClass *gatewayv1.GatewayClass) (*cloudflare.Client, error) {
	if gatewayClass.Spec.ParametersRef == nil {
		return nil, fmt.Errorf("gatewayClass has no parametersRef")
	}

	secretName := gatewayClass.Spec.ParametersRef.Name
	secretNamespace := "cloudflare-gateway-system"
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

func (r *HTTPRouteReconciler) setRouteCondition(route *gatewayv1.HTTPRoute, parentIndex int, condType gatewayv1.RouteConditionType,
	status metav1.ConditionStatus, reason gatewayv1.RouteConditionReason, message string) {

	// Ensure RouteStatus has enough parent statuses
	for len(route.Status.Parents) <= parentIndex {
		parentRef := route.Spec.ParentRefs[len(route.Status.Parents)]
		route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: controllerName,
			Conditions:     []metav1.Condition{},
		})
	}

	condition := metav1.Condition{
		Type:               string(condType),
		Status:             status,
		ObservedGeneration: route.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(reason),
		Message:            message,
	}

	meta.SetStatusCondition(&route.Status.Parents[parentIndex].Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForGateway),
		).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForService),
		).
		Complete(r)
}

// findRoutesForGateway finds all HTTPRoutes that reference a Gateway
func (r *HTTPRouteReconciler) findRoutesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway := obj.(*gatewayv1.Gateway)

	routeList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routeList, client.InNamespace(gateway.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, route := range routeList.Items {
		for _, parentRef := range route.Spec.ParentRefs {
			if string(parentRef.Name) == gateway.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      route.Name,
						Namespace: route.Namespace,
					},
				})
				break
			}
		}
	}

	return requests
}

// validateRouteMatchesListener checks if the HTTPRoute matches a listener on the Gateway
func (r *HTTPRouteReconciler) validateRouteMatchesListener(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, parentRef gatewayv1.ParentReference) error {
	// If parentRef specifies a sectionName, find that specific listener
	if parentRef.SectionName != nil {
		found := false
		for _, listener := range gateway.Spec.Listeners {
			if listener.Name == *parentRef.SectionName {
				found = true
				// Validate protocol compatibility
				if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
					return fmt.Errorf("listener %s has protocol %s which is incompatible with HTTPRoute", listener.Name, listener.Protocol)
				}
				// Validate hostnames if listener has hostname restrictions
				if err := r.validateHostnameMatch(route, listener); err != nil {
					return err
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("no listener with name %s found on gateway", *parentRef.SectionName)
		}
	} else if parentRef.Port != nil {
		// If parentRef specifies a port, find listener with that port
		found := false
		for _, listener := range gateway.Spec.Listeners {
			if listener.Port == *parentRef.Port {
				found = true
				// Validate protocol compatibility
				if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
					return fmt.Errorf("listener on port %d has protocol %s which is incompatible with HTTPRoute", listener.Port, listener.Protocol)
				}
				// Validate hostnames if listener has hostname restrictions
				if err := r.validateHostnameMatch(route, listener); err != nil {
					return err
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("no listener with port %d found on gateway", *parentRef.Port)
		}
	} else {
		// No sectionName or port specified - match any HTTP/HTTPS listener
		found := false
		for _, listener := range gateway.Spec.Listeners {
			if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType {
				// Validate hostnames if listener has hostname restrictions
				if err := r.validateHostnameMatch(route, listener); err != nil {
					continue // Try next listener
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no matching HTTP/HTTPS listener found on gateway")
		}
	}
	return nil
}

// validateHostnameMatch checks if route hostnames are allowed by listener hostname
func (r *HTTPRouteReconciler) validateHostnameMatch(route *gatewayv1.HTTPRoute, listener gatewayv1.Listener) error {
	if listener.Hostname == nil || *listener.Hostname == "" {
		// No hostname restriction on listener - all route hostnames allowed
		return nil
	}

	listenerHostname := string(*listener.Hostname)

	// If route has no hostnames, it would match all - check if listener allows that
	if len(route.Spec.Hostnames) == 0 {
		// This is problematic - route matches all but listener is restricted
		return fmt.Errorf("route has no hostnames but listener restricts to %s", listenerHostname)
	}

	// Check each route hostname matches listener hostname
	for _, routeHostname := range route.Spec.Hostnames {
		if !hostnameMatches(string(routeHostname), listenerHostname) {
			return fmt.Errorf("route hostname %s does not match listener hostname %s", routeHostname, listenerHostname)
		}
	}

	return nil
}

// hostnameMatches checks if a route hostname matches a listener hostname (supports wildcards)
func hostnameMatches(routeHost, listenerHost string) bool {
	// Exact match
	if routeHost == listenerHost {
		return true
	}

	// Wildcard matching: listener "*.example.com" matches route "api.example.com"
	if strings.HasPrefix(listenerHost, "*.") {
		domain := listenerHost[2:]
		return strings.HasSuffix(routeHost, "."+domain) || routeHost == domain
	}

	// Wildcard matching: route "*.example.com" must be more specific than listener
	if strings.HasPrefix(routeHost, "*.") {
		// Route wildcards must be subset of listener
		return routeHost == listenerHost
	}

	return false
}

// findRoutesForService finds all HTTPRoutes that reference a Service as a backend
func (r *HTTPRouteReconciler) findRoutesForService(ctx context.Context, obj client.Object) []reconcile.Request {
	service := obj.(*corev1.Service)

	routeList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routeList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, route := range routeList.Items {
		// Check if this route references the service as a backend
		for _, rule := range route.Spec.Rules {
			for _, backendRef := range rule.BackendRefs {
				// Determine backend namespace
				backendNamespace := route.Namespace
				if backendRef.Namespace != nil {
					backendNamespace = string(*backendRef.Namespace)
				}

				// Check if this backend references our service
				if string(backendRef.Name) == service.Name && backendNamespace == service.Namespace {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      route.Name,
							Namespace: route.Namespace,
						},
					})
					// Move to next route (avoid duplicates)
					goto nextRoute
				}
			}
		}
	nextRoute:
	}

	return requests
}

// buildOriginRequestConfig constructs OriginRequestConfig from HTTPRoute annotations
func (r *HTTPRouteReconciler) buildOriginRequestConfig(route *gatewayv1.HTTPRoute, scheme string) *cloudflare.OriginRequestConfig {
	annotations := route.Annotations
	if annotations == nil {
		return nil
	}

	// Check for YAML config annotation first
	if configYAML := annotations["cloudflare-gateway-controller.github.com/origin-config"]; configYAML != "" {
		config := &cloudflare.OriginRequestConfig{}
		if err := yaml.Unmarshal([]byte(configYAML), config); err == nil {
			return config
		}
		// If parsing fails, fall through to individual annotations
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

// extractZone extracts the base domain from a hostname
// e.g., api.example.com -> example.com
func extractZone(hostname string) string {
	parts := strings.Split(hostname, ".")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return hostname
}
