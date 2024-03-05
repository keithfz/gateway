// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package gatewayapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gwv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/envoyproxy/gateway/internal/ir"
	"github.com/envoyproxy/gateway/internal/status"
	"github.com/envoyproxy/gateway/internal/utils"
)

const (
	defaultRedirectURL  = "%REQ(x-forwarded-proto)%://%REQ(:authority)%/oauth2/callback"
	defaultRedirectPath = "/oauth2/callback"
	defaultLogoutPath   = "/logout"
)

func (t *Translator) ProcessSecurityPolicies(securityPolicies []*egv1a1.SecurityPolicy,
	gateways []*GatewayContext,
	routes []RouteContext,
	resources *Resources,
	xdsIR XdsIRMap) []*egv1a1.SecurityPolicy {
	var res []*egv1a1.SecurityPolicy

	// Sort based on timestamp
	sort.Slice(securityPolicies, func(i, j int) bool {
		return securityPolicies[i].CreationTimestamp.Before(&(securityPolicies[j].CreationTimestamp))
	})

	// First build a map out of the routes and gateways for faster lookup since users might have thousands of routes or more.
	// For gateways this probably isn't quite as necessary.
	routeMap := map[policyTargetRouteKey]*policyRouteTargetContext{}
	for _, route := range routes {
		key := policyTargetRouteKey{
			Kind:      string(GetRouteType(route)),
			Name:      route.GetName(),
			Namespace: route.GetNamespace(),
		}
		routeMap[key] = &policyRouteTargetContext{RouteContext: route}
	}
	gatewayMap := map[types.NamespacedName]*policyGatewayTargetContext{}
	for _, gw := range gateways {
		key := utils.NamespacedName(gw)
		gatewayMap[key] = &policyGatewayTargetContext{GatewayContext: gw}
	}

	// Map of Gateway to the routes attached to it
	gatewayRouteMap := make(map[string]sets.Set[string])

	// Translate
	// 1. First translate Policies targeting xRoutes
	// 2. Finally, the policies targeting Gateways

	// Process the policies targeting xRoutes
	for _, policy := range securityPolicies {
		if policy.Spec.TargetRef.Kind != KindGateway {
			policy := policy.DeepCopy()
			res = append(res, policy)

			// Negative statuses have already been assigned so its safe to skip
			route := resolveSecurityPolicyRouteTargetRef(policy, routeMap)
			if route == nil {
				continue
			}

			// Find the Gateway that the route belongs to and add it to the
			// gatewayRouteMap, which will be used to check policy overrides
			for _, p := range GetParentReferences(route) {
				if p.Kind == nil || *p.Kind == KindGateway {
					namespace := route.GetNamespace()
					if p.Namespace != nil {
						namespace = string(*p.Namespace)
					}
					gw := types.NamespacedName{
						Namespace: namespace,
						Name:      string(p.Name),
					}.String()

					if _, ok := gatewayRouteMap[gw]; !ok {
						gatewayRouteMap[gw] = make(sets.Set[string])
					}
					gatewayRouteMap[gw].Insert(utils.NamespacedName(route).String())
				}
			}

			err := t.translateSecurityPolicyForRoute(policy, route, resources, xdsIR)
			if err != nil {
				status.SetSecurityPolicyCondition(policy,
					gwv1a2.PolicyConditionAccepted,
					metav1.ConditionFalse,
					gwv1a2.PolicyReasonInvalid,
					status.Error2ConditionMsg(err),
				)
			} else {
				message := "SecurityPolicy has been accepted."
				status.SetSecurityPolicyAccepted(&policy.Status, message)
			}
		}
	}
	// Process the policies targeting Gateways
	for _, policy := range securityPolicies {
		if policy.Spec.TargetRef.Kind == KindGateway {
			policy := policy.DeepCopy()
			res = append(res, policy)

			// Negative statuses have already been assigned so its safe to skip
			gateway := resolveSecurityPolicyGatewayTargetRef(policy, gatewayMap)
			if gateway == nil {
				continue
			}

			err := t.translateSecurityPolicyForGateway(policy, gateway, resources, xdsIR)
			if err != nil {
				status.SetSecurityPolicyCondition(policy,
					gwv1a2.PolicyConditionAccepted,
					metav1.ConditionFalse,
					gwv1a2.PolicyReasonInvalid,
					status.Error2ConditionMsg(err),
				)
			} else {
				message := "SecurityPolicy has been accepted."
				status.SetSecurityPolicyAccepted(&policy.Status, message)
			}

			// Check if this policy is overridden by other policies targeting
			// at route level
			gw := utils.NamespacedName(gateway).String()
			if r, ok := gatewayRouteMap[gw]; ok {
				// Maintain order here to ensure status/string does not change with the same data
				routes := r.UnsortedList()
				sort.Strings(routes)
				message := fmt.Sprintf(
					"This policy is being overridden by other securityPolicies for these routes: %v",
					routes)
				status.SetSecurityPolicyCondition(policy,
					egv1a1.PolicyConditionOverridden,
					metav1.ConditionTrue,
					egv1a1.PolicyReasonOverridden,
					message,
				)
			}
		}
	}

	return res
}

func resolveSecurityPolicyGatewayTargetRef(
	policy *egv1a1.SecurityPolicy,
	gateways map[types.NamespacedName]*policyGatewayTargetContext) *GatewayContext {
	targetNs := policy.Spec.TargetRef.Namespace
	// If empty, default to namespace of policy
	if targetNs == nil {
		targetNs = ptr.To(gwv1b1.Namespace(policy.Namespace))
	}

	// Ensure Policy and target are in the same namespace
	if policy.Namespace != string(*targetNs) {

		message := fmt.Sprintf(
			"Namespace:%s TargetRef.Namespace:%s, SecurityPolicy can only target a resource in the same namespace.",
			policy.Namespace, *targetNs)
		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonInvalid,
			message,
		)
		return nil
	}

	// Find the Gateway
	key := types.NamespacedName{
		Name:      string(policy.Spec.TargetRef.Name),
		Namespace: string(*targetNs),
	}
	gateway, ok := gateways[key]

	// Gateway not found
	if !ok {
		message := fmt.Sprintf("Gateway:%s not found.", policy.Spec.TargetRef.Name)

		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonTargetNotFound,
			message,
		)
		return nil
	}

	// Check if another policy targeting the same Gateway exists
	if gateway.attached {
		message := "Unable to target Gateway, another SecurityPolicy has already attached to it"

		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonConflicted,
			message,
		)
		return nil
	}

	// Set context and save
	gateway.attached = true
	gateways[key] = gateway

	return gateway.GatewayContext
}

func resolveSecurityPolicyRouteTargetRef(
	policy *egv1a1.SecurityPolicy,
	routes map[policyTargetRouteKey]*policyRouteTargetContext) RouteContext {
	targetNs := policy.Spec.TargetRef.Namespace
	// If empty, default to namespace of policy
	if targetNs == nil {
		targetNs = ptr.To(gwv1b1.Namespace(policy.Namespace))
	}

	// Ensure Policy and target are in the same namespace
	if policy.Namespace != string(*targetNs) {

		message := fmt.Sprintf(
			"Namespace:%s TargetRef.Namespace:%s, SecurityPolicy can only target a resource in the same namespace.",
			policy.Namespace, *targetNs)
		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonInvalid,
			message,
		)
		return nil
	}

	// Check if the route exists
	key := policyTargetRouteKey{
		Kind:      string(policy.Spec.TargetRef.Kind),
		Name:      string(policy.Spec.TargetRef.Name),
		Namespace: string(*targetNs),
	}
	route, ok := routes[key]

	// Route not found
	if !ok {
		message := fmt.Sprintf(
			"%s/%s/%s not found.",
			policy.Spec.TargetRef.Kind,
			string(*targetNs), policy.Spec.TargetRef.Name)

		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonTargetNotFound,
			message,
		)
		return nil
	}

	// Check if another policy targeting the same xRoute exists
	if route.attached {
		message := fmt.Sprintf(
			"Unable to target %s, another SecurityPolicy has already attached to it",
			string(policy.Spec.TargetRef.Kind))

		status.SetSecurityPolicyCondition(policy,
			gwv1a2.PolicyConditionAccepted,
			metav1.ConditionFalse,
			gwv1a2.PolicyReasonConflicted,
			message,
		)
		return nil
	}

	// Set context and save
	route.attached = true
	routes[key] = route

	return route.RouteContext
}

func (t *Translator) translateSecurityPolicyForRoute(
	policy *egv1a1.SecurityPolicy, route RouteContext,
	resources *Resources, xdsIR XdsIRMap) error {
	// Build IR
	var (
		cors      *ir.CORS
		jwt       *ir.JWT
		oidc      *ir.OIDC
		basicAuth *ir.BasicAuth
		extAuth   *ir.ExtAuth
		err, errs error
	)

	if policy.Spec.CORS != nil {
		cors = t.buildCORS(policy.Spec.CORS)
	}

	if policy.Spec.JWT != nil {
		jwt = t.buildJWT(policy.Spec.JWT)
	}

	if policy.Spec.OIDC != nil {
		if oidc, err = t.buildOIDC(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if policy.Spec.BasicAuth != nil {
		if basicAuth, err = t.buildBasicAuth(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if policy.Spec.ExtAuth != nil {
		if extAuth, err = t.buildExtAuth(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	// Apply IR to all relevant routes
	// Note: there are multiple features in a security policy, even if some of them
	// are invalid, we still want to apply the valid ones.
	prefix := irRoutePrefix(route)
	for _, ir := range xdsIR {
		for _, http := range ir.HTTP {
			for _, r := range http.Routes {
				// Apply if there is a match
				// TODO zhaohuabing: extract a utils function to check if an HTTP
				// route is associated with a Gateway API xRoute
				if strings.HasPrefix(r.Name, prefix) {
					r.CORS = cors
					r.JWT = jwt
					r.OIDC = oidc
					r.BasicAuth = basicAuth
					r.ExtAuth = extAuth
				}
			}
		}
	}
	return errs
}

func (t *Translator) translateSecurityPolicyForGateway(
	policy *egv1a1.SecurityPolicy, gateway *GatewayContext,
	resources *Resources, xdsIR XdsIRMap) error {
	// Build IR
	var (
		cors      *ir.CORS
		jwt       *ir.JWT
		oidc      *ir.OIDC
		basicAuth *ir.BasicAuth
		extAuth   *ir.ExtAuth
		err, errs error
	)

	if policy.Spec.CORS != nil {
		cors = t.buildCORS(policy.Spec.CORS)
	}

	if policy.Spec.JWT != nil {
		jwt = t.buildJWT(policy.Spec.JWT)
	}

	if policy.Spec.OIDC != nil {
		if oidc, err = t.buildOIDC(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if policy.Spec.BasicAuth != nil {
		if basicAuth, err = t.buildBasicAuth(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if policy.Spec.ExtAuth != nil {
		if extAuth, err = t.buildExtAuth(policy, resources); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	// Apply IR to all the routes within the specific Gateway that originated
	// from the gateway to which this security policy was attached.
	// If the feature is already set, then skip it, since it must have be
	// set by a policy attaching to the route
	//
	// Note: there are multiple features in a security policy, even if some of them
	// are invalid, we still want to apply the valid ones.
	irKey := t.getIRKey(gateway.Gateway)
	// Should exist since we've validated this
	ir := xdsIR[irKey]

	policyTarget := irStringKey(
		string(ptr.Deref(policy.Spec.TargetRef.Namespace, gwv1a2.Namespace(policy.Namespace))),
		string(policy.Spec.TargetRef.Name),
	)
	for _, http := range ir.HTTP {
		gatewayName := http.Name[0:strings.LastIndex(http.Name, "/")]
		if t.MergeGateways && gatewayName != policyTarget {
			continue
		}
		for _, r := range http.Routes {
			// Apply if not already set
			if r.CORS == nil {
				r.CORS = cors
			}
			if r.JWT == nil {
				r.JWT = jwt
			}
			if r.OIDC == nil {
				r.OIDC = oidc
			}
			if r.BasicAuth == nil {
				r.BasicAuth = basicAuth
			}
			if r.ExtAuth == nil {
				r.ExtAuth = extAuth
			}
		}
	}
	return errs
}

func (t *Translator) buildCORS(cors *egv1a1.CORS) *ir.CORS {
	var allowOrigins []*ir.StringMatch

	for _, origin := range cors.AllowOrigins {
		origin := origin
		if isWildcard(string(origin)) {
			regexStr := wildcard2regex(string(origin))
			allowOrigins = append(allowOrigins, &ir.StringMatch{
				SafeRegex: &regexStr,
			})
		} else {
			allowOrigins = append(allowOrigins, &ir.StringMatch{
				Exact: (*string)(&origin),
			})
		}
	}

	return &ir.CORS{
		AllowOrigins:     allowOrigins,
		AllowMethods:     cors.AllowMethods,
		AllowHeaders:     cors.AllowHeaders,
		ExposeHeaders:    cors.ExposeHeaders,
		MaxAge:           cors.MaxAge,
		AllowCredentials: cors.AllowCredentials != nil && *cors.AllowCredentials,
	}
}

func isWildcard(s string) bool {
	return strings.ContainsAny(s, "*")
}

func wildcard2regex(wildcard string) string {
	regexStr := strings.ReplaceAll(wildcard, ".", "\\.")
	regexStr = strings.ReplaceAll(regexStr, "*", ".*")
	return regexStr
}

func (t *Translator) buildJWT(jwt *egv1a1.JWT) *ir.JWT {
	return &ir.JWT{
		Providers: jwt.Providers,
	}
}

func (t *Translator) buildOIDC(
	policy *egv1a1.SecurityPolicy,
	resources *Resources) (*ir.OIDC, error) {
	var (
		oidc         = policy.Spec.OIDC
		clientSecret *v1.Secret
		provider     *ir.OIDCProvider
		err          error
	)

	from := crossNamespaceFrom{
		group:     egv1a1.GroupName,
		kind:      KindSecurityPolicy,
		namespace: policy.Namespace,
	}
	if clientSecret, err = t.validateSecretRef(
		false, from, oidc.ClientSecret, resources); err != nil {
		return nil, err
	}

	clientSecretBytes, ok := clientSecret.Data[egv1a1.OIDCClientSecretKey]
	if !ok || len(clientSecretBytes) == 0 {
		return nil, fmt.Errorf(
			"client secret not found in secret %s/%s",
			clientSecret.Namespace, clientSecret.Name)
	}

	// Discover the token and authorization endpoints from the issuer's
	// well-known url if not explicitly specified
	if provider, err = discoverEndpointsFromIssuer(&oidc.Provider); err != nil {
		return nil, err
	}

	if err = validateTokenEndpoint(provider.TokenEndpoint); err != nil {
		return nil, err
	}
	scopes := appendOpenidScopeIfNotExist(oidc.Scopes)

	var (
		redirectURL  = defaultRedirectURL
		redirectPath = defaultRedirectPath
		logoutPath   = defaultLogoutPath
	)

	if oidc.RedirectURL != nil {
		path, err := extractRedirectPath(*oidc.RedirectURL)
		if err != nil {
			return nil, err
		}
		redirectURL = *oidc.RedirectURL
		redirectPath = path
	}
	if oidc.LogoutPath != nil {
		logoutPath = *oidc.LogoutPath
	}

	// Generate a unique cookie suffix for oauth filters
	suffix := utils.Digest32(string(policy.UID))

	return &ir.OIDC{
		Provider:     *provider,
		ClientID:     oidc.ClientID,
		ClientSecret: clientSecretBytes,
		Scopes:       scopes,
		RedirectURL:  redirectURL,
		RedirectPath: redirectPath,
		LogoutPath:   logoutPath,
		CookieSuffix: suffix,
	}, nil
}

func extractRedirectPath(redirectURL string) (string, error) {
	schemeDelimiter := strings.Index(redirectURL, "://")
	if schemeDelimiter <= 0 {
		return "", fmt.Errorf("invalid redirect URL %s", redirectURL)
	}
	scheme := redirectURL[:schemeDelimiter]
	if scheme != "http" && scheme != "https" && scheme != "%REQ(x-forwarded-proto)%" {
		return "", fmt.Errorf("invalid redirect URL %s", redirectURL)
	}
	hostDelimiter := strings.Index(redirectURL[schemeDelimiter+3:], "/")
	if hostDelimiter <= 0 {
		return "", fmt.Errorf("invalid redirect URL %s", redirectURL)
	}
	path := redirectURL[schemeDelimiter+3+hostDelimiter:]
	if path == "/" {
		return "", fmt.Errorf("invalid redirect URL %s", redirectURL)
	}
	return path, nil
}

// appendOpenidScopeIfNotExist appends the openid scope to the provided scopes
// if it is not already present.
// `openid` is a required scope for OIDC.
// see https://openid.net/specs/openid-connect-core-1_0.html#ScopeClaims
func appendOpenidScopeIfNotExist(scopes []string) []string {
	const authScopeOpenID = "openid"

	hasOpenIDScope := false
	for _, scope := range scopes {
		if scope == authScopeOpenID {
			hasOpenIDScope = true
		}
	}
	if !hasOpenIDScope {
		scopes = append(scopes, authScopeOpenID)
	}
	return scopes
}

type OpenIDConfig struct {
	TokenEndpoint         string `json:"token_endpoint"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
}

// discoverEndpointsFromIssuer discovers the token and authorization endpoints from the issuer's well-known url
// return error if failed to fetch the well-known configuration
func discoverEndpointsFromIssuer(provider *egv1a1.OIDCProvider) (*ir.OIDCProvider, error) {
	if provider.TokenEndpoint == nil || provider.AuthorizationEndpoint == nil {
		tokenEndpoint, authorizationEndpoint, err := fetchEndpointsFromIssuer(provider.Issuer)
		if err != nil {
			return nil, fmt.Errorf("error fetching endpoints from issuer: %w", err)
		}
		return &ir.OIDCProvider{
			TokenEndpoint:         tokenEndpoint,
			AuthorizationEndpoint: authorizationEndpoint,
		}, nil
	}

	return &ir.OIDCProvider{
		TokenEndpoint:         *provider.TokenEndpoint,
		AuthorizationEndpoint: *provider.AuthorizationEndpoint,
	}, nil
}

func fetchEndpointsFromIssuer(issuerURL string) (string, string, error) {
	// Fetch the OpenID configuration from the issuer URL
	resp, err := http.Get(fmt.Sprintf("%s/.well-known/openid-configuration", issuerURL))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	// Parse the OpenID configuration response
	var config OpenIDConfig
	err = json.NewDecoder(resp.Body).Decode(&config)
	if err != nil {
		return "", "", err
	}

	return config.TokenEndpoint, config.AuthorizationEndpoint, nil
}

// validateTokenEndpoint validates the token endpoint URL
func validateTokenEndpoint(tokenEndpoint string) error {
	parsedURL, err := url.Parse(tokenEndpoint)
	if err != nil {
		return fmt.Errorf("error parsing token endpoint URL: %w", err)
	}

	if ip, err := netip.ParseAddr(parsedURL.Hostname()); err == nil {
		if ip.Unmap().Is4() {
			return fmt.Errorf("token endpoint URL must be a domain name: %s", tokenEndpoint)
		}
	}

	if parsedURL.Port() != "" {
		_, err = strconv.Atoi(parsedURL.Port())
		if err != nil {
			return fmt.Errorf("error parsing token endpoint URL port: %w", err)
		}
	}
	return nil
}

func (t *Translator) buildBasicAuth(
	policy *egv1a1.SecurityPolicy,
	resources *Resources) (*ir.BasicAuth, error) {
	var (
		basicAuth   = policy.Spec.BasicAuth
		usersSecret *v1.Secret
		err         error
	)

	from := crossNamespaceFrom{
		group:     egv1a1.GroupName,
		kind:      KindSecurityPolicy,
		namespace: policy.Namespace,
	}
	if usersSecret, err = t.validateSecretRef(
		false, from, basicAuth.Users, resources); err != nil {
		return nil, err
	}

	usersSecretBytes, ok := usersSecret.Data[egv1a1.BasicAuthUsersSecretKey]
	if !ok || len(usersSecretBytes) == 0 {
		return nil, fmt.Errorf(
			"users secret not found in secret %s/%s",
			usersSecret.Namespace, usersSecret.Name)
	}

	return &ir.BasicAuth{Users: usersSecretBytes}, nil
}

func (t *Translator) buildExtAuth(
	policy *egv1a1.SecurityPolicy,
	resources *Resources) (*ir.ExtAuth, error) {
	var (
		http       = policy.Spec.ExtAuth.HTTP
		grpc       = policy.Spec.ExtAuth.GRPC
		backendRef *gwapiv1.BackendObjectReference
		protocol   ir.AppProtocol
		ds         *ir.DestinationSetting
		authority  string
		err        error
	)

	switch {
	// These are sanity checks, they should never happen because the API server
	// should have caught them
	case http == nil && grpc == nil:
		return nil, errors.New("one of grpc or http must be specified")
	case http != nil && grpc != nil:
		return nil, errors.New("only one of grpc or http can be specified")
	case http != nil:
		backendRef = &http.BackendRef
		protocol = ir.HTTP
	case grpc != nil:
		backendRef = &grpc.BackendRef
		protocol = ir.GRPC
	}

	if err = t.validateExtServiceBackendReference(
		backendRef,
		policy.Namespace,
		resources); err != nil {
		return nil, err
	}
	authority = fmt.Sprintf(
		"%s.%s:%d",
		backendRef.Name,
		NamespaceDerefOr(backendRef.Namespace, policy.Namespace),
		*backendRef.Port)

	if ds, err = t.processExtServiceDestination(
		backendRef,
		policy.Namespace,
		protocol,
		resources); err != nil {
		return nil, err
	}
	rd := ir.RouteDestination{
		Name:     irExtServiceDestinationName(policy, string(backendRef.Name)),
		Settings: []*ir.DestinationSetting{ds},
	}

	extAuth := &ir.ExtAuth{
		HeadersToExtAuth: policy.Spec.ExtAuth.HeadersToExtAuth,
	}

	if http != nil {
		extAuth.HTTP = &ir.HTTPExtAuthService{
			Destination:      rd,
			Authority:        authority,
			Path:             ptr.Deref(http.Path, ""),
			HeadersToBackend: http.HeadersToBackend,
		}
	} else {
		extAuth.GRPC = &ir.GRPCExtAuthService{
			Destination: rd,
			Authority:   authority,
		}
	}
	return extAuth, nil
}

// TODO: zhaohuabing combine this function with the one in the route translator
func (t *Translator) processExtServiceDestination(
	backendRef *gwapiv1.BackendObjectReference,
	ownerNamespace string,
	protocol ir.AppProtocol,
	resources *Resources) (*ir.DestinationSetting, error) {
	var (
		endpoints   []*ir.DestinationEndpoint
		addrType    *ir.DestinationAddressType
		servicePort v1.ServicePort
	)

	serviceNamespace := NamespaceDerefOr(backendRef.Namespace, ownerNamespace)
	service := resources.GetService(serviceNamespace, string(backendRef.Name))
	for _, port := range service.Spec.Ports {
		if port.Port == int32(*backendRef.Port) {
			servicePort = port
			break
		}
	}

	if servicePort.AppProtocol != nil &&
		*servicePort.AppProtocol == "kubernetes.io/h2c" {
		protocol = ir.HTTP2
	}

	// Route to endpoints by default
	if !t.EndpointRoutingDisabled {
		endpointSlices := resources.GetEndpointSlicesForBackend(
			serviceNamespace, string(backendRef.Name), KindService)
		endpoints, addrType = getIREndpointsFromEndpointSlices(
			endpointSlices, servicePort.Name, servicePort.Protocol)
	} else {
		// Fall back to Service ClusterIP routing
		ep := ir.NewDestEndpoint(
			service.Spec.ClusterIP,
			uint32(*backendRef.Port))
		endpoints = append(endpoints, ep)
	}

	// TODO: support mixed endpointslice address type for the same backendRef
	if !t.EndpointRoutingDisabled && addrType != nil && *addrType == ir.MIXED {
		return nil, errors.New(
			"mixed endpointslice address type for the same backendRef is not supported")
	}

	return &ir.DestinationSetting{
		Weight:      ptr.To(uint32(1)),
		Protocol:    protocol,
		Endpoints:   endpoints,
		AddressType: addrType,
	}, nil
}

func irExtServiceDestinationName(policy *egv1a1.SecurityPolicy, service string) string {
	return strings.ToLower(fmt.Sprintf(
		"%s/%s/%s/%s",
		KindSecurityPolicy,
		policy.GetNamespace(),
		policy.GetName(),
		service))
}
