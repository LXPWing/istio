// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gateway

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	k8s "sigs.k8s.io/gateway-api/apis/v1alpha1"

	istio "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model/kstatus"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/pkg/log"
)

const (
	ControllerName = "istio.io/gateway-controller"
)

type KubernetesResources struct {
	GatewayClass  []config.Config
	Gateway       []config.Config
	HTTPRoute     []config.Config
	TCPRoute      []config.Config
	TLSRoute      []config.Config
	BackendPolicy []config.Config
	Namespaces    map[string]*corev1.Namespace

	// Domain for the cluster. Typically cluster.local
	Domain string
}

// isRouteMatch checks if a route should bind to a gateway.
// This takes into account selection config on both the gateway and xRoute objects
func isRouteMatch(cfg config.Config, gateway config.Meta,
	routes k8s.RouteBindingSelector, namespaces map[string]*corev1.Namespace) bool {
	// This entire function is a series of condition `return false`s. If everything passes, we consider
	// it a match.

	// First check this config is the right type
	if routes.Kind != cfg.GroupVersionKind.Kind {
		return false
	}

	grp := routes.Group
	if grp == "" { // Default group in the spec
		grp = "networking.x-k8s.io"
	}
	if grp != cfg.GroupVersionKind.Group {
		return false
	}

	// Next, check the label selector matches the gateway
	ls, err := metav1.LabelSelectorAsSelector(&routes.Selector)
	if err != nil {
		log.Errorf("failed to create route selector: %v", err)
		return false
	}
	if !ls.Matches(klabels.Set(cfg.Labels)) {
		return false
	}

	// Check the gateway's namespace selector
	namespaceSelector := routes.Namespaces

	if namespaceSelector.From == "" {
		// Setup default if not provided
		namespaceSelector.From = k8s.RouteSelectSame
	}
	switch namespaceSelector.From {
	case k8s.RouteSelectAll:
		// Always matches, continue
	case k8s.RouteSelectSame:
		if gateway.Namespace != cfg.Namespace {
			return false
		}
	case k8s.RouteSelectSelector:
		ns, err := metav1.LabelSelectorAsSelector(&namespaceSelector.Selector)
		if err != nil {
			log.Errorf("failed to create namespace selector: %v", err)
			return false
		}
		namespace := namespaces[cfg.Namespace]
		if namespace == nil {
			log.Errorf("missing namespace %v for route %v, skipping", cfg.Namespace, cfg.Name)
			return false
		}
		if !ns.Matches(klabels.Set(namespace.Labels)) {
			return false
		}
	}

	gatewaySelector := getGatewaySelectorFromSpec(cfg.Spec)
	if gatewaySelector == nil {
		gatewaySelector = &k8s.RouteGateways{Allow: k8s.GatewayAllowSameNamespace}
	}
	switch gatewaySelector.Allow {
	case k8s.GatewayAllowAll:
	// Always matches, continue
	case k8s.GatewayAllowFromList:
		found := false
		for _, gw := range gatewaySelector.GatewayRefs {
			if gw.Name == gateway.Name && gw.Namespace == gateway.Namespace {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	case k8s.GatewayAllowSameNamespace:
		if gateway.Namespace != cfg.Namespace {
			return false
		}
	}

	return true
}

func getGatewaySelectorFromSpec(spec config.Spec) *k8s.RouteGateways {
	switch s := spec.(type) {
	case *k8s.HTTPRouteSpec:
		return &s.Gateways
	case *k8s.TCPRouteSpec:
		return &s.Gateways
	case *k8s.TLSRouteSpec:
		return &s.Gateways
	default:
		return nil
	}
}

func (r *KubernetesResources) fetchHTTPRoutes(gateway config.Meta, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, http := range r.HTTPRoute {
		if isRouteMatch(http, gateway, routes, r.Namespaces) {
			result = append(result, http)
		}
	}
	return result
}

func (r *KubernetesResources) fetchTCPRoutes(gateway config.Meta, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, tcp := range r.TCPRoute {
		if isRouteMatch(tcp, gateway, routes, r.Namespaces) {
			result = append(result, tcp)
		}
	}
	return result
}

func (r *KubernetesResources) fetchTLSRoutes(gateway config.Meta, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, tls := range r.TLSRoute {
		if isRouteMatch(tls, gateway, routes, r.Namespaces) {
			result = append(result, tls)
		}
	}
	return result
}

type OutputResources struct {
	Gateway         []config.Config
	VirtualService  []config.Config
	DestinationRule []config.Config
}

func convertResources(r *KubernetesResources) OutputResources {
	result := OutputResources{}
	gw, routeMap := convertGateway(r)
	result.Gateway = gw
	result.VirtualService = convertVirtualService(r, routeMap)
	result.DestinationRule = convertDestinationRule(r)
	return result
}

// Unique key to identify a route
type RouteKey struct {
	Gvk       config.GroupVersionKind
	Name      string
	Namespace string
}

func toRouteKey(c config.Config) RouteKey {
	return RouteKey{
		c.GroupVersionKind,
		c.Name,
		c.Namespace,
	}
}

func convertDestinationRule(r *KubernetesResources) []config.Config {
	result := []config.Config{}
	for _, obj := range r.BackendPolicy {
		bp := obj.Spec.(*k8s.BackendPolicySpec)

		// We have no status updates to write. The only condition defined is ConditionNoSuchBackend;
		// since we translate to opaque host names we don't actually know
		// TODO(https://github.com/kubernetes-sigs/gateway-api/issues/590) consider more fields in the API

		for i, ref := range bp.BackendRefs {
			var serviceName string
			if emptyOrEqual(ref.Group, gvk.Service.CanonicalGroup()) && emptyOrEqual(ref.Kind, gvk.Service.Kind) {
				serviceName = fmt.Sprintf("%s.%s.svc.%s", ref.Name, obj.Namespace, r.Domain)
			} else {
				log.Warnf("unsupported backendRef: %+v", ref)
				continue
			}
			dr := &istio.DestinationRule{
				Host:          serviceName,
				TrafficPolicy: &istio.TrafficPolicy{},
			}
			if bp.TLS != nil && bp.TLS.CertificateAuthorityRef != nil {
				tls := &istio.ClientTLSSettings{
					// Currently, only simple is supported
					CredentialName: buildSecretReference(*bp.TLS.CertificateAuthorityRef),
					Mode:           istio.ClientTLSSettings_SIMPLE,
				}
				if ref.Port != nil {
					dr.TrafficPolicy.PortLevelSettings = append(dr.TrafficPolicy.PortLevelSettings, &istio.TrafficPolicy_PortTrafficPolicy{
						Port: &istio.PortSelector{Number: uint32(*ref.Port)},
						Tls:  tls,
					})
				} else {
					dr.TrafficPolicy.Tls = tls
				}
			}
			drConfig := config.Config{
				Meta: config.Meta{
					CreationTimestamp: obj.CreationTimestamp,
					GroupVersionKind:  gvk.DestinationRule,
					Name:              fmt.Sprintf("%s-%d-%s", obj.Name, i, constants.KubernetesGatewayName),
					Namespace:         obj.Namespace,
					Domain:            r.Domain,
				},
				Spec: dr,
			}
			result = append(result, drConfig)
		}
	}
	return result
}

func convertVirtualService(r *KubernetesResources, routeMap map[RouteKey][]string) []config.Config {
	result := []config.Config{}
	for _, obj := range r.TCPRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		vsConfig := buildTCPVirtualService(obj, gateways, r.Domain)
		result = append(result, vsConfig)
	}

	for _, obj := range r.TLSRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		vsConfig := buildTLSVirtualService(obj, gateways, r.Domain)
		result = append(result, vsConfig)
	}

	for _, obj := range r.HTTPRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		result = append(result, buildHTTPVirtualServices(obj, gateways, r.Domain)...)
	}
	return result
}

func buildHTTPVirtualServices(obj config.Config, gateways []string, domain string) []config.Config {
	result := []config.Config{}

	route := obj.Spec.(*k8s.HTTPRouteSpec)
	obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
		rs := s.(*k8s.HTTPRouteStatus)
		// TODO report skipped routes
		rs.Gateways = createRouteStatus(gateways, obj)
		return rs
	})

	name := fmt.Sprintf("%s-%s", obj.Name, constants.KubernetesGatewayName)

	httproutes := []*istio.HTTPRoute{}
	hosts := hostnameToStringList(route.Hostnames)
	for _, r := range route.Rules {
		// TODO: implement redirect, rewrite, timeout, mirror, corspolicy, retries
		vs := &istio.HTTPRoute{}
		for _, match := range r.Matches {
			vs.Match = append(vs.Match, &istio.HTTPMatchRequest{
				Uri:     createURIMatch(match),
				Headers: createHeadersMatch(match),
			})
		}
		for _, filter := range r.Filters {
			switch filter.Type {
			case k8s.HTTPRouteFilterRequestHeaderModifier:
				vs.Headers = createHeadersFilter(filter.RequestHeaderModifier)
			default:
				log.Warnf("unsupported filter type %q", filter.Type)
			}
		}

		vs.Route = buildHTTPDestination(r.ForwardTo, obj.Namespace, domain)
		httproutes = append(httproutes, vs)
	}
	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              name,
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			Hosts:    hosts,
			Gateways: gateways,
			Http:     httproutes,
		},
	}
	result = append(result, vsConfig)
	return result
}

func createRouteStatus(gateways []string, obj config.Config) []k8s.RouteGatewayStatus {
	gws := make([]k8s.RouteGatewayStatus, 0, len(gateways))
	// TODO(https://github.com/kubernetes-sigs/gateway-api/issues/591) this assumes full ownership of route
	for _, gw := range gateways {
		ref := k8s.GatewayReference{}
		if gw == constants.IstioMeshGateway {
			ref.Name = experimentalMeshGatewayName
			// TODO this is not namespaced but a namespace is required
			ref.Namespace = "default"
		} else {
			s := strings.Split(gw, "/")
			ref.Name = s[1]
			ref.Namespace = s[0]
		}
		gws = append(gws, k8s.RouteGatewayStatus{
			GatewayRef: ref,
			Conditions: []metav1.Condition{{
				Type:               string(k8s.ConditionRouteAdmitted),
				Status:             kstatus.StatusTrue,
				ObservedGeneration: obj.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "RouteAdmitted",
				Message:            "Route admitted",
			}},
		})
	}
	return gws
}

func hostnameToStringList(h []k8s.Hostname) []string {
	res := make([]string, 0, len(h))
	for _, i := range h {
		res = append(res, string(i))
	}
	return res
}

func buildTCPVirtualService(obj config.Config, gateways []string, domain string) config.Config {
	route := obj.Spec.(*k8s.TCPRouteSpec)

	obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
		rs := s.(*k8s.TCPRouteStatus)
		// TODO report skipped routes
		rs.Gateways = createRouteStatus(gateways, obj)
		return rs
	})

	routes := []*istio.TCPRoute{}
	for _, r := range route.Rules {
		ir := &istio.TCPRoute{
			Match: buildTCPMatch(r.Matches),
			Route: buildTCPDestination(r.ForwardTo, obj.Namespace, domain),
		}
		routes = append(routes, ir)
	}

	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              fmt.Sprintf("%s-tcp-%s", obj.Name, constants.KubernetesGatewayName),
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			// TODO investigate if we should/must constrain this to avoid conflicts
			Hosts:    []string{"*"},
			Gateways: gateways,
			Tcp:      routes,
		},
	}
	return vsConfig
}

func buildTLSVirtualService(obj config.Config, gateways []string, domain string) config.Config {
	route := obj.Spec.(*k8s.TLSRouteSpec)

	obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
		rs := s.(*k8s.TLSRouteStatus)
		// TODO report skipped routes
		rs.Gateways = createRouteStatus(gateways, obj)
		return rs
	})

	routes := []*istio.TLSRoute{}
	for _, r := range route.Rules {
		ir := &istio.TLSRoute{
			Match: buildTLSMatch(r.Matches),
			Route: buildTCPDestination(r.ForwardTo, obj.Namespace, domain),
		}
		routes = append(routes, ir)
	}

	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              fmt.Sprintf("%s-tls-%s", obj.Name, constants.KubernetesGatewayName),
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			// TODO investigate if we should/must constrain this to avoid conflicts
			Hosts:    []string{"*"},
			Gateways: gateways,
			Tls:      routes,
		},
	}
	return vsConfig
}

func buildTCPDestination(action []k8s.RouteForwardTo, ns, domain string) []*istio.RouteDestination {
	if len(action) == 0 {
		return nil
	}

	weights := []int{}
	for _, w := range action {
		weights = append(weights, int(w.Weight))
	}
	weights = standardizeWeights(weights)
	res := []*istio.RouteDestination{}
	for i, fwd := range action {
		dst := buildGenericDestination(fwd, ns, domain)
		res = append(res, &istio.RouteDestination{
			Destination: dst,
			Weight:      int32(weights[i]),
		})
	}
	return res
}

func buildTCPMatch([]k8s.TCPRouteMatch) []*istio.L4MatchAttributes {
	// Currently the spec only supports extensions, which are not currently implemented by Istio.
	return nil
}

func buildTLSMatch(match []k8s.TLSRouteMatch) []*istio.TLSMatchAttributes {
	if len(match) == 0 {
		// Istio validation doesn't like empty match, instead do a match all explicitly
		return []*istio.TLSMatchAttributes{{
			SniHosts: []string{"*"},
		}}
	}
	res := make([]*istio.TLSMatchAttributes, 0, len(match))
	for _, m := range match {
		res = append(res, &istio.TLSMatchAttributes{
			SniHosts: hostnamesToStringlist(m.SNIs),
		})
	}
	return res
}

func hostnamesToStringlist(h []k8s.Hostname) []string {
	res := make([]string, 0, len(h))
	for _, i := range h {
		res = append(res, string(i))
	}
	return res
}

func intSum(n []int) int {
	r := 0
	for _, i := range n {
		r += i
	}
	return r
}

func buildHTTPDestination(action []k8s.HTTPRouteForwardTo, ns string, domain string) []*istio.HTTPRouteDestination {
	if action == nil {
		return nil
	}

	weights := []int{}
	for _, w := range action {
		weights = append(weights, int(w.Weight))
	}
	weights = standardizeWeights(weights)
	res := []*istio.HTTPRouteDestination{}
	for i, fwd := range action {
		dst := buildDestination(fwd, ns, domain)
		rd := &istio.HTTPRouteDestination{
			Destination: dst,
			Weight:      int32(weights[i]),
		}
		for _, filter := range fwd.Filters {
			switch filter.Type {
			case k8s.HTTPRouteFilterRequestHeaderModifier:
				rd.Headers = createHeadersFilter(filter.RequestHeaderModifier)
			default:
				log.Warnf("unsupported filter type %q", filter.Type)
			}
		}
		res = append(res, rd)
	}
	return res
}

func buildDestination(to k8s.HTTPRouteForwardTo, ns, domain string) *istio.Destination {
	res := &istio.Destination{}
	if to.Port != nil {
		// TODO: "If unspecified, the destination port in the request is used when forwarding to a backendRef or serviceName."
		// We need to link up with the gateway and construct a per gateway virtual service. This is not actually
		// possible with targetPort in some scenarios; need to reconsider the API.
		res.Port = &istio.PortSelector{Number: uint32(*to.Port)}
	}
	if to.ServiceName != nil {
		res.Host = fmt.Sprintf("%s.%s.svc.%s", *to.ServiceName, ns, domain)
	} else if to.BackendRef != nil {
		// TODO support this
		log.Errorf("referencing unsupported destination; backendRef is not supported")
	}
	return res
}

func buildGenericDestination(to k8s.RouteForwardTo, ns, domain string) *istio.Destination {
	res := &istio.Destination{}
	if to.Port != nil {
		// TODO: "If unspecified, the destination port in the request is used when forwarding to a backendRef or serviceName."
		// We need to link up with the gateway and construct a per gateway virtual service. This is not actually
		// possible with targetPort in some scenarios; need to reconsider the API.
		res.Port = &istio.PortSelector{Number: uint32(*to.Port)}
	}
	if to.ServiceName != nil {
		res.Host = fmt.Sprintf("%s.%s.svc.%s", *to.ServiceName, ns, domain)
	} else if to.BackendRef != nil {
		// TODO support this
		log.Errorf("referencing unsupported destination; backendRef is not supported")
	}
	return res
}

// standardizeWeights migrates a list of weights from relative weights, to weights out of 100
// In the event we cannot cleanly move to 100 denominator, we will round up weights in order. See test for details.
// TODO in the future we should probably just make VirtualService support relative weights directly
func standardizeWeights(weights []int) []int {
	if len(weights) == 1 {
		// Instead of setting weight=100 for a single destination, we will not set weight at all
		return []int{0}
	}
	total := intSum(weights)
	if total == 0 {
		// All empty, fallback to even weight
		for i := range weights {
			weights[i] = 1
		}
		total = len(weights)
	}
	results := make([]int, 0, len(weights))
	remainders := make([]float64, 0, len(weights))
	for _, w := range weights {
		perc := float64(w) / float64(total)
		rounded := int(perc * 100)
		remainders = append(remainders, (perc*100)-float64(rounded))
		results = append(results, rounded)
	}
	remaining := 100 - intSum(results)
	order := argsort(remainders)
	for _, idx := range order {
		if remaining <= 0 {
			break
		}
		remaining--
		results[idx]++
	}
	return results
}

type argSlice struct {
	sort.Interface
	idx []int
}

func (s argSlice) Swap(i, j int) {
	s.Interface.Swap(i, j)
	s.idx[i], s.idx[j] = s.idx[j], s.idx[i]
}

func argsort(n []float64) []int {
	s := &argSlice{Interface: sort.Float64Slice(n), idx: make([]int, len(n))}
	for i := range s.idx {
		s.idx[i] = i
	}
	sort.Sort(sort.Reverse(s))
	return s.idx
}

func createHeadersFilter(filter *k8s.HTTPRequestHeaderFilter) *istio.Headers {
	if filter == nil {
		return nil
	}
	return &istio.Headers{
		Request: &istio.Headers_HeaderOperations{
			Add:    filter.Add,
			Remove: filter.Remove,
			Set:    filter.Set,
		},
	}
}

func createHeadersMatch(match k8s.HTTPRouteMatch) map[string]*istio.StringMatch {
	if match.Headers == nil {
		return nil
	}
	res := map[string]*istio.StringMatch{}
	if match.Headers.Type == "" ||
		match.Headers.Type == k8s.HeaderMatchExact ||
		match.Headers.Type == k8s.HeaderMatchImplementationSpecific {
		for k, v := range match.Headers.Values {
			res[k] = &istio.StringMatch{
				MatchType: &istio.StringMatch_Exact{Exact: v},
			}
		}
	} else {
		log.Warnf("unknown type: %v is not supported Header type", match.Headers.Type)
		return nil
	}
	return res
}

// prefixMatchRegex optionally matches "/..." at the end of a path.
// regex taken from https://github.com/projectcontour/contour/blob/2b3376449bedfea7b8cea5fbade99fb64009c0f6/internal/envoy/v3/route.go#L59
const prefixMatchRegex = `((\/).*)?`

func createURIMatch(match k8s.HTTPRouteMatch) *istio.StringMatch {
	if match.Path.Type == "" || match.Path.Type == k8s.PathMatchImplementationSpecific || match.Path.Type == k8s.PathMatchPrefix {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Regex{Regex: regexp.QuoteMeta(strings.TrimSuffix(match.Path.Value, "/")) + prefixMatchRegex},
		}
	} else if match.Path.Type == k8s.PathMatchExact {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Exact{Exact: match.Path.Value},
		}
	} else if match.Path.Type == k8s.PathMatchRegularExpression {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Regex{Regex: match.Path.Value},
		}
	} else {
		log.Warnf("unknown type: %s is not supported Path match type", match.Path.Type)
		return nil
	}
}

// getGatewayClass finds all gateway class that are owned by Istio
func getGatewayClasses(r *KubernetesResources) map[string]struct{} {
	classes := map[string]struct{}{}
	for _, obj := range r.GatewayClass {
		gwc := obj.Spec.(*k8s.GatewayClassSpec)
		if gwc.Controller == ControllerName {
			// TODO we can add any settings we need here needed for the controller
			// For now, we have none, so just add a struct
			classes[obj.Name] = struct{}{}

			obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
				gcs := s.(*k8s.GatewayClassStatus)
				gcs.Conditions = kstatus.ConditionallyUpdateCondition(gcs.Conditions, metav1.Condition{
					Type:               string(k8s.GatewayClassConditionStatusAdmitted),
					Status:             kstatus.StatusTrue,
					ObservedGeneration: obj.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "Handled",
					Message:            "Handled by Istio controller",
				})
				return gcs
			})
		}
	}
	return classes
}

func convertGateway(r *KubernetesResources) ([]config.Config, map[RouteKey][]string) {
	result := []config.Config{}
	routeToGateway := map[RouteKey][]string{}
	classes := getGatewayClasses(r)
	for _, obj := range r.Gateway {
		kgw := obj.Spec.(*k8s.GatewaySpec)
		if _, f := classes[kgw.GatewayClassName]; !f {
			// No gateway class found, this may be meant for another controller; should be skipped.
			continue
		}
		obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
			gs := s.(*k8s.GatewayStatus)
			// TODO implement addresses
			gs.Addresses = []k8s.GatewayAddress{}
			// We expect one listener status per listener
			if len(gs.Listeners) != len(kgw.Listeners) {
				gs.Listeners = make([]k8s.ListenerStatus, len(kgw.Listeners))
			}
			return gs
		})
		name := obj.Name + "-" + constants.KubernetesGatewayName
		var servers []*istio.Server
		for i, l := range kgw.Listeners {
			obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
				gs := s.(*k8s.GatewayStatus)
				cond := gs.Listeners[i].Conditions
				cond = kstatus.ConditionallyUpdateCondition(cond, metav1.Condition{
					Type:               string(k8s.ListenerConditionReady),
					Status:             kstatus.StatusTrue,
					ObservedGeneration: obj.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "ListenerReady",
					Message:            "No error found",
				})
				// TODO: implement ResolvedRefs if we cannot find all refs
				// TODO: implement Detached if we find something we cannot support
				gs.Listeners[i] = k8s.ListenerStatus{
					Port:       l.Port,
					Protocol:   l.Protocol,
					Hostname:   l.Hostname,
					Conditions: cond,
				}
				return gs
			})
			server := &istio.Server{
				// Allow all hosts here. Specific routing will be determined by the virtual services
				Hosts: buildHostnameMatch(l.Hostname),
				Port: &istio.Port{
					Number: uint32(l.Port),
					// TODO currently we 1:1 support protocols in the API. If this changes we may
					// need more logic here.
					Protocol: string(l.Protocol),
					Name:     fmt.Sprintf("%v-%v-gateway-%s-%s", strings.ToLower(string(l.Protocol)), l.Port, obj.Name, obj.Namespace),
				},
				// TODO support RouteOverride
				Tls: buildTLS(l.TLS),
			}

			servers = append(servers, server)

			// TODO support VirtualService direct reference
			for _, http := range r.fetchHTTPRoutes(obj.Meta, l.Routes) {
				k := toRouteKey(http)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
			for _, tcp := range r.fetchTCPRoutes(obj.Meta, l.Routes) {
				k := toRouteKey(tcp)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
			for _, tls := range r.fetchTLSRoutes(obj.Meta, l.Routes) {
				k := toRouteKey(tls)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
		}
		gatewayConfig := config.Config{
			Meta: config.Meta{
				CreationTimestamp: obj.CreationTimestamp,
				GroupVersionKind:  gvk.Gateway,
				Name:              name,
				Namespace:         obj.Namespace,
				Domain:            r.Domain,
			},
			Spec: &istio.Gateway{
				Servers: servers,
				// TODO derive this from gatewayclass param ref
				Selector: labels.Instance{constants.IstioLabel: "ingressgateway"},
			},
		}
		obj.Status.(*kstatus.WrappedStatus).Mutate(func(s config.Status) config.Status {
			gs := s.(*k8s.GatewayStatus)
			// TODO: report invalid configurations
			gs.Conditions = kstatus.ConditionallyUpdateCondition(gs.Conditions, metav1.Condition{
				Type:               string(k8s.GatewayConditionReady),
				Status:             kstatus.StatusTrue,
				ObservedGeneration: obj.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "ListenersValid",
				Message:            "Listeners valid",
			})
			// TODO: when we implement "address" support in status, we should report unscheduled
			// if there is no associated Service.
			gs.Conditions = kstatus.ConditionallyUpdateCondition(gs.Conditions, metav1.Condition{
				Type:               string(k8s.GatewayConditionScheduled),
				Status:             kstatus.StatusTrue,
				ObservedGeneration: obj.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "ResourcesAvailable",
				Message:            "Resources available",
			})
			return gs
		})
		result = append(result, gatewayConfig)
	}
	for _, k := range r.fetchMeshRoutes() {
		routeToGateway[k] = append(routeToGateway[k], experimentalMeshGatewayName)
	}
	return result, routeToGateway
}

// experimentalMeshGatewayName defines the magic mesh gateway name.
// TODO: replace this with a more suitable API. This is just added now to allow early adopters to experiment with the API
const experimentalMeshGatewayName = "mesh"

func (r *KubernetesResources) fetchMeshRoutes() []RouteKey {
	keys := []RouteKey{}
	// We only look at HTTP routes for now
	// TODO(https://github.com/kubernetes-sigs/gateway-api/issues) add TLS. We can do it today, but its a bit annoying
	// TODO: add TCP. Need an annotation or API change to associate a route with a service (hostname).
	for _, hr := range r.HTTPRoute {
		gatewaySelector := getGatewaySelectorFromSpec(hr.Spec)
		if gatewaySelector == nil || len(gatewaySelector.GatewayRefs) == 0 {
			continue
		}
		for _, ref := range gatewaySelector.GatewayRefs {
			if ref.Name == experimentalMeshGatewayName { // we ignore namespace. it is required in the spec though
				keys = append(keys, toRouteKey(hr))
			}
		}
	}
	return keys
}

func buildTLS(tls *k8s.GatewayTLSConfig) *istio.ServerTLSSettings {
	if tls == nil {
		return nil
	}
	// Explicitly not supported: file mounted
	// Not yet implemented: TLS mode, https redirect, max protocol version, SANs, CipherSuites, VerifyCertificate

	// TODO: "The SNI server_name must match a route host name for the Gateway to route the TLS request."
	// Do we need to do something smarter here to support ^ ?
	out := &istio.ServerTLSSettings{
		HttpsRedirect: false,
	}
	switch tls.Mode {
	case "", k8s.TLSModeTerminate:
		out.Mode = istio.ServerTLSSettings_SIMPLE
		if tls.CertificateRef == nil {
			// This is required in the API, should be rejected in validation
			log.Warnf("invalid tls certificate ref: %v", tls)
			return nil
		}
		out.CredentialName = buildSecretReference(*tls.CertificateRef)
	case k8s.TLSModePassthrough:
		out.Mode = istio.ServerTLSSettings_PASSTHROUGH
	}
	return out
}

func buildSecretReference(ref k8s.LocalObjectReference) string {
	if !emptyOrEqual(ref.Group, gvk.Secret.CanonicalGroup()) || !emptyOrEqual(ref.Kind, gvk.Secret.Kind) {
		log.Errorf("invalid certificate reference %v, only secret is allowed", ref)
		return ""
	}
	return ref.Name
}

func buildHostnameMatch(hostname *k8s.Hostname) []string {
	// gateway-api hostname semantics match ours, so pass directly. The one
	// exception is they allow unset, which is equivalent to * for us
	if hostname == nil || *hostname == "" {
		return []string{"*"}
	}
	return []string{string(*hostname)}
}

func emptyOrEqual(have, expected string) bool {
	return have == "" || have == expected
}
