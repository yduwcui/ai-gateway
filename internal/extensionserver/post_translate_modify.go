// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extensionserver

import (
	"context"
	"strconv"
	"strings"
	"time"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	egextension "github.com/envoyproxy/gateway/proto/extension"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	upstream_codecv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/upstream_codec/v3"
	httpconnectionmanagerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwaiev1a2 "sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
)

const (
	extProcUDSClusterName = "ai-gateway-extproc-uds"
	aiGatewayExtProcName  = "envoy.filters.http.ext_proc/aigateway"
)

// PostTranslateModify allows an extension to modify the clusters and secrets in the xDS config
// after the initial translation is complete. This method is responsible for:
//
// 1. Modifying existing clusters (e.g., adding metadata, adjusting configurations)
// 2. Adding additional clusters needed for InferencePool support (EPP clusters)
// 3. Ensuring the AI Gateway external processor UDS cluster exists
//
// For InferencePool support, this method creates additional STRICT_DNS clusters that
// connect to the endpoint picker services specified in InferencePool resources.
func (s *Server) PostTranslateModify(_ context.Context, req *egextension.PostTranslateModifyRequest) (*egextension.PostTranslateModifyResponse, error) {
	var extProcUDSExist bool

	// Process existing clusters - may add metadata or modify configurations.
	for _, cluster := range req.Clusters {
		s.maybeModifyCluster(cluster)
		extProcUDSExist = extProcUDSExist || cluster.Name == extProcUDSClusterName
	}

	// Add external processor clusters for InferencePool backends.
	// These clusters connect to the endpoint picker services (EPP) specified in InferencePool resources.
	req.Clusters = append(req.Clusters, buildClustersForInferencePoolEndpointPickers(req.Clusters)...)

	// Modify listeners and routes to support InferencePool backends.
	s.maybeModifyListenerAndRoutes(req.Listeners, req.Routes)

	// Ensure the AI Gateway external processor UDS cluster exists.
	// This cluster is used for communication with the AI Gateway's main external processor.
	if !extProcUDSExist {
		po := &httpv3.HttpProtocolOptions{}
		po.UpstreamProtocolOptions = &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
			ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
				Http2ProtocolOptions: &corev3.Http2ProtocolOptions{
					// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/internal/xds/translator/listener.go#L50-L53
					InitialConnectionWindowSize: wrapperspb.UInt32(1048576),
					InitialStreamWindowSize:     wrapperspb.UInt32(65536),
				},
			},
		}}
		req.Clusters = append(req.Clusters, &clusterv3.Cluster{
			Name:                 extProcUDSClusterName,
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/api/v1alpha1/timeout_types.go#L25
			ConnectTimeout: &durationpb.Duration{Seconds: 10},
			TypedExtensionProtocolOptions: map[string]*anypb.Any{
				"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustToAny(po),
			},
			// Default is 32768 bytes == 32 KiB which seems small:
			// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/internal/xds/translator/cluster.go#L49
			//
			// So, we set it to 50MBi.
			PerConnectionBufferLimitBytes: wrapperspb.UInt32(52428800),
			LoadAssignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: extProcUDSClusterName,
				Endpoints: []*endpointv3.LocalityLbEndpoints{
					{
						LbEndpoints: []*endpointv3.LbEndpoint{
							{
								HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
									Endpoint: &endpointv3.Endpoint{
										Address: &corev3.Address{
											Address: &corev3.Address_Pipe{
												Pipe: &corev3.Pipe{
													Path: s.udsPath,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		})
		s.log.Info("Added extproc-uds cluster to the list of clusters")
	}
	response := &egextension.PostTranslateModifyResponse{Clusters: req.Clusters, Secrets: req.Secrets, Listeners: req.Listeners, Routes: req.Routes}
	return response, nil
}

// maybeModifyCluster modifies clusters generated from AIGatewayRoute resources to add
// necessary configurations for AI Gateway functionality. This function performs several key tasks:
//
//  1. Populates cluster endpoint metadata per backend - This is a workaround until
//     https://github.com/envoyproxy/gateway/issues/5523 is resolved and endpoint-level metadata
//     is supported in external processors.
//
//  2. Inserts the upstream external processor filter for request/response processing.
//     See: https://github.com/envoyproxy/gateway/issues/5881
//
// 3. Inserts the header mutation filter for dynamic header modifications.
//
// 4. Configures special handling for InferencePool clusters (ORIGINAL_DST type).
//
// The resulting configuration is similar to the envoy.yaml files in tests/extproc.
// Only clusters with names matching the AIGatewayRoute pattern are modified.
func (s *Server) maybeModifyCluster(cluster *clusterv3.Cluster) {
	// Parse cluster name to extract AIGatewayRoute information.
	// Expected format: "httproute/<namespace>/<name>/rule/<index_of_rule>".
	parts := strings.Split(cluster.Name, "/")
	if len(parts) != 5 || parts[0] != "httproute" {
		// This is not an AIGatewayRoute-generated cluster, skip modification.
		s.log.Info("non-ai-gateway cluster name", "cluster_name", cluster.Name)
		return
	}
	httpRouteNamespace := parts[1]
	httpRouteName := parts[2]
	httpRouteRuleIndexStr := parts[4]
	httpRouteRuleIndex, err := strconv.Atoi(httpRouteRuleIndexStr)
	if err != nil {
		s.log.Error(err, "failed to parse HTTPRoute rule index",
			"cluster_name", cluster.Name, "rule_index", httpRouteRuleIndexStr)
		return
	}

	// Check if this rule has InferencePool backends.
	pool := getInferencePoolByMetadata(cluster.Metadata)
	// Get the HTTPRoute object from the cluster name.
	var aigwRoute aigv1a1.AIGatewayRoute
	err = s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: httpRouteNamespace, Name: httpRouteName}, &aigwRoute)
	if err != nil {
		if apierrors.IsNotFound(err) {
			s.log.Info("Skipping user-created HTTPRoute cluster modification",
				"namespace", httpRouteNamespace, "name", httpRouteName)
			return
		}
		s.log.Error(err, "failed to get AIGatewayRoute object",
			"namespace", httpRouteNamespace, "name", httpRouteName)
		return
	}

	// Get the backend from the HTTPRoute object.
	if httpRouteRuleIndex >= len(aigwRoute.Spec.Rules) {
		s.log.Info("HTTPRoute rule index out of range",
			"cluster_name", cluster.Name, "rule_index", httpRouteRuleIndexStr)
		return
	}
	httpRouteRule := &aigwRoute.Spec.Rules[httpRouteRuleIndex]

	// Only process LoadAssignment for non-InferencePool backends.
	if pool == nil {
		if cluster.LoadAssignment == nil {
			s.log.Info("LoadAssignment is nil", "cluster_name", cluster.Name)
			return
		}
		if len(cluster.LoadAssignment.Endpoints) != len(httpRouteRule.BackendRefs) {
			s.log.Info("LoadAssignment endpoints length does not match backend refs length",
				"cluster_name", cluster.Name, "endpoints_length", len(cluster.LoadAssignment.Endpoints), "backend_refs_length", len(httpRouteRule.BackendRefs))
			return
		}
		// Populate the metadata for each endpoint in the LoadAssignment.
		for i, endpoints := range cluster.LoadAssignment.Endpoints {
			backendRef := httpRouteRule.BackendRefs[i]
			name := backendRef.Name
			namespace := aigwRoute.Namespace
			if backendRef.Priority != nil {
				endpoints.Priority = *backendRef.Priority
			}
			// We populate the same metadata for all endpoints in the LoadAssignment.
			// This is because currently, an extproc cannot retrieve the endpoint set level metadata.
			for _, endpoint := range endpoints.LbEndpoints {
				if endpoint.Metadata == nil {
					endpoint.Metadata = &corev3.Metadata{}
				}
				if endpoint.Metadata.FilterMetadata == nil {
					endpoint.Metadata.FilterMetadata = make(map[string]*structpb.Struct)
				}
				m, ok := endpoint.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
				if !ok {
					m = &structpb.Struct{}
					endpoint.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace] = m
				}
				if m.Fields == nil {
					m.Fields = make(map[string]*structpb.Value)
				}
				m.Fields[internalapi.InternalMetadataBackendNameKey] = structpb.NewStringValue(
					internalapi.PerRouteRuleRefBackendName(namespace, name, aigwRoute.Name, httpRouteRuleIndex, i),
				)
			}
		}
	} else {
		// we can only specify one backend in a rule for InferencePool.
		backendRef := httpRouteRule.BackendRefs[0]
		name := backendRef.Name
		namespace := aigwRoute.Namespace
		if cluster.Metadata == nil {
			cluster.Metadata = &corev3.Metadata{}
		}
		if cluster.Metadata.FilterMetadata == nil {
			cluster.Metadata.FilterMetadata = make(map[string]*structpb.Struct)
		}
		m, ok := cluster.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
		if !ok {
			m = &structpb.Struct{}
			cluster.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace] = m
		}
		if m.Fields == nil {
			m.Fields = make(map[string]*structpb.Value)
		}
		m.Fields[internalapi.InternalMetadataBackendNameKey] = structpb.NewStringValue(
			internalapi.PerRouteRuleRefBackendName(namespace, name, aigwRoute.Name, httpRouteRuleIndex, 0),
		)
	}

	if cluster.TypedExtensionProtocolOptions == nil {
		cluster.TypedExtensionProtocolOptions = make(map[string]*anypb.Any)
	}
	const httpProtocolOptions = "envoy.extensions.upstreams.http.v3.HttpProtocolOptions"
	var po *httpv3.HttpProtocolOptions
	if raw, ok := cluster.TypedExtensionProtocolOptions[httpProtocolOptions]; ok {
		po = &httpv3.HttpProtocolOptions{}
		if err = raw.UnmarshalTo(po); err != nil {
			s.log.Error(err, "failed to unmarshal HttpProtocolOptions", "cluster_name", cluster.Name)
			return
		}
	} else {
		po = &httpv3.HttpProtocolOptions{}
		po.UpstreamProtocolOptions = &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
			ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{},
		}}
	}

	for _, filter := range po.HttpFilters {
		if filter.Name == aiGatewayExtProcName {
			// Nothing to do, the filter is already there.
			return
		}
	}

	extProcConfig := &extprocv3.ExternalProcessor{}
	extProcConfig.MetadataOptions = &extprocv3.MetadataOptions{
		ReceivingNamespaces: &extprocv3.MetadataOptions_MetadataNamespaces{
			Untyped: []string{aigv1a1.AIGatewayFilterMetadataNamespace},
		},
	}
	extProcConfig.AllowModeOverride = true
	extProcConfig.RequestAttributes = []string{internalapi.XDSUpstreamHostMetadataKey, internalapi.XDSClusterMetadataKey}
	extProcConfig.ProcessingMode = &extprocv3.ProcessingMode{
		RequestHeaderMode: extprocv3.ProcessingMode_SEND,
		// At the upstream filter, it can access the original body in its memory, so it can perform the translation
		// as well as the authentication at the request headers. Hence, there's no need to send the request body to the extproc.
		RequestBodyMode: extprocv3.ProcessingMode_NONE,
		// Response will be handled at the router filter level so that we could avoid the shenanigans around the retry+the upstream filter.
		ResponseHeaderMode: extprocv3.ProcessingMode_SKIP,
		ResponseBodyMode:   extprocv3.ProcessingMode_NONE,
	}
	extProcConfig.GrpcService = &corev3.GrpcService{
		TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
			EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
				ClusterName: extProcUDSClusterName,
			},
		},
		Timeout: durationpb.New(30 * time.Second),
	}
	extProcFilter := &httpconnectionmanagerv3.HttpFilter{
		Name:       aiGatewayExtProcName,
		ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{TypedConfig: mustToAny(extProcConfig)},
	}

	headerMutFilter := &httpconnectionmanagerv3.HttpFilter{
		Name: "envoy.filters.http.header_mutation",
		ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{
			TypedConfig: mustToAny(&header_mutationv3.HeaderMutation{
				Mutations: &header_mutationv3.Mutations{
					RequestMutations: []*mutation_rulesv3.HeaderMutation{
						{
							Action: &mutation_rulesv3.HeaderMutation_Append{
								Append: &corev3.HeaderValueOption{
									AppendAction: corev3.HeaderValueOption_ADD_IF_ABSENT,
									Header: &corev3.HeaderValue{
										Key:   "content-length",
										Value: `%DYNAMIC_METADATA(` + aigv1a1.AIGatewayFilterMetadataNamespace + `:content_length)%`,
									},
								},
							},
						},
						{
							Action: &mutation_rulesv3.HeaderMutation_Append{
								Append: &corev3.HeaderValueOption{
									AppendAction: corev3.HeaderValueOption_ADD_IF_ABSENT,
									Header: &corev3.HeaderValue{
										Key:   "x-request-tx-duration",
										Value: "request-tx-duration",
									},
								},
							},
						},
					},
				},
			}),
		},
	}

	if len(po.HttpFilters) > 0 {
		// Insert the ext_proc filter before the last filter since the last one is always the upstream codec filter.
		last := po.HttpFilters[len(po.HttpFilters)-1]
		po.HttpFilters = po.HttpFilters[:len(po.HttpFilters)-1]
		po.HttpFilters = append(po.HttpFilters, extProcFilter, headerMutFilter, last)
	} else {
		po.HttpFilters = append(po.HttpFilters, extProcFilter, headerMutFilter)
		// We always need the upstream_code filter as a last filter.
		upstreamCodec := &httpconnectionmanagerv3.HttpFilter{}
		upstreamCodec.Name = "envoy.filters.http.upstream_codec"
		upstreamCodec.ConfigType = &httpconnectionmanagerv3.HttpFilter_TypedConfig{
			TypedConfig: mustToAny(&upstream_codecv3.UpstreamCodec{}),
		}
		po.HttpFilters = append(po.HttpFilters, upstreamCodec)
	}
	cluster.TypedExtensionProtocolOptions[httpProtocolOptions] = mustToAny(po)
}

// maybeModifyListenerAndRoutes modifies listeners and routes to support InferencePool backends.
// This function performs the following operations:
// 1. Identifies listeners and routes that use InferencePool backends
// 2. Adds endpoint picker (EPP) external processor filters to relevant listeners
// 3. Configures per-route filters to disable EPP processing for non-InferencePool routes
// This ensures that only routes targeting InferencePool backends go through the endpoint picker.
func (s *Server) maybeModifyListenerAndRoutes(listeners []*listenerv3.Listener, routes []*routev3.RouteConfiguration) {
	listenerNameToRouteNames := make(map[string][]string)
	listenerNameToListener := make(map[string]*listenerv3.Listener)
	for _, listener := range listeners {
		if strings.HasPrefix(listener.Name, "envoy-gateway") {
			continue
		}
		listenerNameToRouteNames[listener.Name] = findListenerRouteConfigs(listener)
		listenerNameToListener[listener.Name] = listener
	}

	// inferencePoolRoutes builds a matrix of route configs and the inference pools they use.
	routeNameToRoute := make(map[string]*routev3.RouteConfiguration)
	routeNameToVHRouteNameToInferencePool := make(map[string]map[string]*gwaiev1a2.InferencePool)
	for _, routeCfg := range routes {
		routeNameToRoute[routeCfg.Name] = routeCfg
		for _, vh := range routeCfg.VirtualHosts {
			for _, route := range vh.Routes {
				if pool := getInferencePoolByMetadata(route.Metadata); pool != nil {
					if routeNameToVHRouteNameToInferencePool[routeCfg.Name] == nil {
						routeNameToVHRouteNameToInferencePool[routeCfg.Name] = make(map[string]*gwaiev1a2.InferencePool)
					}
					routeNameToVHRouteNameToInferencePool[routeCfg.Name][route.Name] = pool
				}
			}
		}
	}

	// listenerToInferencePools builds a matrix of listeners and the inference pools they use.
	listenerToInferencePools := make(map[string][]*gwaiev1a2.InferencePool)
	for listener, routeCfgNames := range listenerNameToRouteNames {
		for _, name := range routeCfgNames {
			if routeNameToRoute[name] == nil {
				continue
			}
			if routeNameToVHRouteNameToInferencePool[name] == nil {
				continue
			}
			for _, pool := range routeNameToVHRouteNameToInferencePool[name] {
				if listenerToInferencePools[listener] == nil {
					listenerToInferencePools[listener] = make([]*gwaiev1a2.InferencePool, 0)
				}
				listenerToInferencePools[listener] = append(listenerToInferencePools[listener], pool)
			}
		}
	}

	// patch the listeners, the route configs and the virtual hosts with inference pool filters.
	for listener, pools := range listenerToInferencePools {
		s.log.Info("patching listener with inference pool filters", "listener", listener)
		s.patchListenerWithInferencePoolFilters(listenerNameToListener[listener], pools)
		routeCfgNames := listenerNameToRouteNames[listener]
		for _, routeCfgName := range routeCfgNames {
			routeCfg := routeNameToRoute[routeCfgName]
			if routeCfg == nil {
				continue
			}
			for _, vh := range routeCfg.VirtualHosts {
				s.log.Info("patching virtual host with inference pool filters", "listener", listener, "virtual_host", vh.Name)
				s.patchVirtualHostWithInferencePool(vh, pools)
			}
		}
	}

	for _, ln := range listeners {
		var enabled bool
		for _, name := range listenerNameToRouteNames[ln.Name] {
			routeCfg := routeNameToRoute[name]
			if routeCfg == nil {
				s.log.Info("skipping patching of non-existent route config", "route_config", name)
				continue
			}
			enabled = enabled || s.enableRouterLevelAIGatewayExtProcOnRoute(routeCfg)
		}
		if enabled {
			s.log.Info("inserting AI Gateway extproc filter into listener", "listener", ln.Name)
			s.insertRouterLevelAIGatewayExtProc(ln)
		}
	}
}

// patchListenerWithInferencePoolFilters adds the necessary HTTP filters to the listener to support InferencePool backends.
func (s *Server) patchListenerWithInferencePoolFilters(listener *listenerv3.Listener, inferencePools []*gwaiev1a2.InferencePool) {
	// First, get the filter chains from the listener.
	filterChains := listener.GetFilterChains()
	defaultFC := listener.DefaultFilterChain
	if defaultFC != nil {
		filterChains = append(filterChains, defaultFC)
	}
	// Go over all of the chains, and add the endpoint picker external processor filters.
	for _, currChain := range filterChains {
		httpConManager, hcmIndex, err := findHCM(currChain)
		if err != nil {
			s.log.Error(err, "failed to find an HCM in the current chain")
			continue
		}
		var poolFilters []*httpconnectionmanagerv3.HttpFilter
		for _, pool := range inferencePools {
			_, baIndex, searchErr := searchInferencePoolInFilterChain(pool, httpConManager.HttpFilters)
			if searchErr != nil {
				s.log.Error(searchErr, "failed to find an inference pool ext proc filter")
				continue
			}
			if baIndex == -1 {
				eppExtProc := buildInferencePoolHTTPFilter(pool)
				poolFilters = append(poolFilters, eppExtProc)
			}
		}
		if len(poolFilters) != 0 {
			length := len(httpConManager.HttpFilters)
			router := httpConManager.HttpFilters[length-1]
			httpConManager.HttpFilters = httpConManager.HttpFilters[:length-1]
			httpConManager.HttpFilters = append(httpConManager.HttpFilters, poolFilters...)
			httpConManager.HttpFilters = append(httpConManager.HttpFilters, router)
		}

		// Write the updated HCM back to the filter chain.
		anyConnectionMgr, err := anypb.New(httpConManager)
		if err != nil {
			s.log.Error(err, "failed to marshal the updated HCM")
			continue
		}
		currChain.Filters[hcmIndex].ConfigType = &listenerv3.Filter_TypedConfig{
			TypedConfig: anyConnectionMgr,
		}
	}
}

// patchVirtualHostWithInferencePool adds the necessary per-route configuration to disable.
func (s *Server) patchVirtualHostWithInferencePool(vh *routev3.VirtualHost, inferencePools []*gwaiev1a2.InferencePool) {
	inferenceMatrix := make(map[string]*gwaiev1a2.InferencePool)
	for _, pool := range inferencePools {
		inferenceMatrix[httpFilterNameForInferencePool(pool)] = pool
	}
	for _, route := range vh.Routes {
		override := &extprocv3.ExtProcPerRoute{
			Override: &extprocv3.ExtProcPerRoute_Disabled{
				Disabled: true,
			},
		}
		inferencePool := getInferencePoolByMetadata(route.Metadata)
		if inferencePool == nil {
			for key, pool := range inferenceMatrix {
				s.log.Info("disabling inference pool filter", "route", route.Name, "filter", key, "pool", pool.Name)
				if route.TypedPerFilterConfig == nil {
					route.TypedPerFilterConfig = make(map[string]*anypb.Any)
				}
				route.TypedPerFilterConfig[key] = mustToAny(override)
			}
		} else {
			for key, pool := range inferenceMatrix {
				if key != httpFilterNameForInferencePool(inferencePool) {
					s.log.Info("disabling inference pool filter", "route", route.Name, "filter", key, "pool", pool.Name)
					if route.TypedPerFilterConfig == nil {
						route.TypedPerFilterConfig = make(map[string]*anypb.Any)
					}
					route.TypedPerFilterConfig[key] = mustToAny(override)
				}
			}
		}
	}
}

// enableRouterLevelAIGatewayExtProcOnRoute checks if the extproc filter should be enabled for routes
// that are generated by AIGateway. It modifies the route configuration to enable the extproc filter
// for those routes. It returns true if any route was modified.
func (s *Server) enableRouterLevelAIGatewayExtProcOnRoute(routeConfig *routev3.RouteConfiguration) bool {
	enabled := false
	for _, vh := range routeConfig.VirtualHosts {
		for _, route := range vh.Routes {
			aiGatewayGenerated := s.isRouteGeneratedByAIGateway(route)
			if aiGatewayGenerated {
				enabled = true
				if route.TypedPerFilterConfig == nil {
					route.TypedPerFilterConfig = make(map[string]*anypb.Any)
				}
				// Enable the extproc filter for this route.
				route.TypedPerFilterConfig[aiGatewayExtProcName] = mustToAny(&routev3.FilterConfig{
					Config: &anypb.Any{},
				})
			}
		}
	}
	return enabled
}

// insertRouterLevelAIGatewayExtProcExtProc inserts the AI Gateway external processor filter into the listener's filter chains.
func (s *Server) insertRouterLevelAIGatewayExtProc(listener *listenerv3.Listener) {
	// First, get the filter chains from the listener.
	filterChains := listener.GetFilterChains()
	defaultFC := listener.DefaultFilterChain
	if defaultFC != nil {
		filterChains = append(filterChains, defaultFC)
	}
	// Go over all of the chains, and add the endpoint picker external processor filters.
	for _, currChain := range filterChains {
		httpConManager, hcmIndex, err := findHCM(currChain)
		if err != nil {
			s.log.Error(err, "failed to find an HCM in the current chain")
			return
		}
		// Check if the extproc filter is already present.
		if !shouldAIGatewayExtProcBeInserted(httpConManager.HttpFilters) {
			return // The filter is already present, nothing to do.
		}

		extProcFilter := &httpconnectionmanagerv3.HttpFilter{
			Name:     aiGatewayExtProcName,
			Disabled: true, // Disable the filter by default, it will be enabled per route where the route is generated by AIGateway.
			ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{TypedConfig: mustToAny(&extprocv3.ExternalProcessor{
				GrpcService: &corev3.GrpcService{
					TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
							ClusterName: extProcUDSClusterName,
						},
					},
				},
				MetadataOptions: &extprocv3.MetadataOptions{
					ReceivingNamespaces: &extprocv3.MetadataOptions_MetadataNamespaces{
						Untyped: []string{aigv1a1.AIGatewayFilterMetadataNamespace},
					},
				},
				ProcessingMode: &extprocv3.ProcessingMode{
					RequestHeaderMode:   extprocv3.ProcessingMode_SEND,
					RequestBodyMode:     extprocv3.ProcessingMode_BUFFERED,
					RequestTrailerMode:  extprocv3.ProcessingMode_SKIP,
					ResponseHeaderMode:  extprocv3.ProcessingMode_SEND,
					ResponseBodyMode:    extprocv3.ProcessingMode_BUFFERED,
					ResponseTrailerMode: extprocv3.ProcessingMode_SKIP,
				},
				MessageTimeout:    durationpb.New(10 * time.Second),
				FailureModeAllow:  false,
				AllowModeOverride: true,
			})},
		}

		// Insert the AI Gateway extproc filter as the first extproc filter.
		insertAIGatewayExtProcFilter(httpConManager, extProcFilter)
		// Write the updated HCM back to the filter chain.
		currChain.Filters[hcmIndex].ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: mustToAny(httpConManager)}
	}
}

func (s *Server) isRouteGeneratedByAIGateway(route *routev3.Route) bool {
	if s.isStandAloneMode {
		// In stand-alone mode, we don't have the route metadata, so we assume that all routes are generated by AIGateway.
		return true
	}

	// The route metadata should look like this:
	//
	//	filterMetadata:
	//	 envoy-gateway:
	//	   resources:
	//	   - annotations:
	//	       ai-gateway-generated: true
	//      ....
	//
	// where "ai-gateway-generated" is the annotation that indicates that the route was generated by AIGateway.
	// The below is to find it and enable the extproc filter for that route if it is found.
	if route.Metadata == nil || route.Metadata.FilterMetadata == nil {
		s.log.Info("no metadata found in the route, skipping", "route", route.Name)
		return false
	}

	eg, ok := route.Metadata.FilterMetadata["envoy-gateway"]
	if !ok {
		s.log.Info("no envoy-gateway metadata found in the route, skipping", "route", route.Name)
		return false
	}
	resources, ok := eg.Fields["resources"]
	if !ok {
		s.log.Info("no resources found in the envoy-gateway metadata, skipping", "route", route.Name)
		return false
	}
	if resources.GetListValue() == nil || len(resources.GetListValue().Values) == 0 {
		s.log.Info("no resources found in the envoy-gateway metadata, skipping", "route", route.Name)
		return false
	}

	// Walk through the resources to find the AIGateway-generated HTTPRoute annotation.
	for _, resource := range resources.GetListValue().Values {
		if resource.GetStructValue() == nil {
			s.log.Info("resource is not a struct, skipping", "route", route.Name, "resource", resource)
			continue
		}
		annotations, ok := resource.GetStructValue().Fields["annotations"]
		if !ok {
			s.log.Info("no annotations found in the resource, skipping", "route", route.Name, "resource", resource)
			continue
		}
		if annotations.GetStructValue() == nil {
			s.log.Info("annotations is not a struct, skipping", "route", route.Name, "resource", resource)
			continue
		}
		_, ok = annotations.GetStructValue().Fields[internalapi.AIGatewayGeneratedHTTPRouteAnnotation]
		if ok {
			return true
		}
	}
	return false
}

func shouldAIGatewayExtProcBeInserted(filters []*httpconnectionmanagerv3.HttpFilter) bool {
	for _, f := range filters {
		if f.Name == aiGatewayExtProcName ||
			// This is a backwards compatibility check for the old filter name generated by the EnvoyExtensionPolicy.
			// https://github.com/envoyproxy/ai-gateway/blob/v0.2.1/internal/controller/gateway.go#L133
			//
			// TODO: remove after v0.3 release.
			strings.Contains(f.Name, "ai-eg-eep-") {
			return false
		}
	}
	return true
}

// insertAIGatewayExtProcFilter inserts the AI Gateway extproc filter into the HTTP connection manager.
//
// The order is simple: make sure that the AI Gateway extproc filter is the very first extproc filter in the standard
// Envoy Gateway order. See:
// https://github.com/envoyproxy/gateway/blob/f1e6dab770fabc70d175237380eedfc1f9b1a9e5/internal/xds/translator/httpfilters.go#L93
func insertAIGatewayExtProcFilter(mgr *httpconnectionmanagerv3.HttpConnectionManager, filter *httpconnectionmanagerv3.HttpFilter) {
	insertIndex := -1
outer:
	for i, existingFilter := range mgr.HttpFilters { // TODO: Maybe searching backwards is faster, but not sure if it's worth the complexity.
		for _, prefix := range afterExtProcFilterPrefixes {
			if strings.HasPrefix(existingFilter.Name, prefix) {
				insertIndex = i
				break outer
			}
		}
	}
	if insertIndex == -1 {
		panic("BUG: No suitable insertion point found for AIGateway extproc filter")
	}
	mgr.HttpFilters = append(mgr.HttpFilters, filter)
	copy(mgr.HttpFilters[insertIndex+1:], mgr.HttpFilters[insertIndex:])
	mgr.HttpFilters[insertIndex] = filter
}

var afterExtProcFilterPrefixes = []string{
	egv1a1.EnvoyFilterExtProc.String(),
	egv1a1.EnvoyFilterWasm.String(),
	egv1a1.EnvoyFilterRBAC.String(),
	egv1a1.EnvoyFilterLocalRateLimit.String(),
	egv1a1.EnvoyFilterRateLimit.String(),
	egv1a1.EnvoyFilterCustomResponse.String(),
	egv1a1.EnvoyFilterCredentialInjector.String(),
	egv1a1.EnvoyFilterCompressor.String(),
	egv1a1.EnvoyFilterRouter.String(),
}

// findListenerRouteConfigs extracts route configuration names from the listener's filter chains.
func findListenerRouteConfigs(listener *listenerv3.Listener) []string {
	var names []string
	// First, get the filter chains from the listener.
	for _, filterChain := range listener.FilterChains {
		httpConManager, _, err := findHCM(filterChain)
		if err != nil {
			continue // Skip this filter chain if it doesn't have an HTTP connection manager.
		}
		rds := httpConManager.GetRds()
		if rds == nil {
			continue // Skip if no route discovery service is configured.
		}
		if rds.RouteConfigName != "" {
			names = append(names, rds.RouteConfigName)
		}
	}
	httpConManager, _, err := findHCM(listener.DefaultFilterChain)
	if err != nil {
		return names // Return names collected so far, even if default filter chain has no HCM.
	}
	rds := httpConManager.GetRds()
	if rds == nil {
		return names // Return names collected so far, even if no RDS in default filter chain.
	}
	return append(names, rds.RouteConfigName) // Add default filter chain's route config name.
}
