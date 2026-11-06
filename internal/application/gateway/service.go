package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/application/keys"
	"github.com/XuLaiQ/all2api/internal/domain/scope"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/XuLaiQ/all2api/internal/scheduler"
)

type Error struct {
	Status        int
	Code          string
	Message       string
	Headers       map[string]string
	Channel       string
	UpstreamModel string
	RouteAlias    string
	FallbackDepth int
}

func (e *Error) Error() string { return e.Message }

type Service struct {
	keys         *keys.Service
	adapters     map[string]ports.Adapter
	recorder     ports.RequestRecorder
	pool         *scheduler.Pool
	channelState ports.ChannelStateSource
	routes       ports.RouteSource
	mappings     ports.ModelMappingSource
	now          func() time.Time
	modelMu      sync.RWMutex
	modelCache   map[string]modelCacheEntry
	mappingMu    sync.RWMutex
	mappingCache map[string]map[string]string // channel -> public_id -> upstream_id
	mappingRead  time.Time
}

type modelCacheEntry struct {
	models    []ports.ModelDescriptor
	updatedAt time.Time
}

const modelCacheTTL = 5 * time.Minute

func NewService(keyService *keys.Service, adapters map[string]ports.Adapter) *Service {
	return NewServiceWithRecorder(keyService, adapters, nil)
}

func NewServiceWithRecorder(keyService *keys.Service, adapters map[string]ports.Adapter, recorder ports.RequestRecorder) *Service {
	return NewServiceWithRecorderAndPool(keyService, adapters, recorder, nil)
}

func NewServiceWithRecorderAndPool(keyService *keys.Service, adapters map[string]ports.Adapter, recorder ports.RequestRecorder, pool *scheduler.Pool) *Service {
	copyAdapters := make(map[string]ports.Adapter, len(adapters))
	for channel, adapter := range adapters {
		copyAdapters[channel] = adapter
	}
	return &Service{keys: keyService, adapters: copyAdapters, recorder: recorder, pool: pool, now: time.Now, modelCache: make(map[string]modelCacheEntry)}
}

func (s *Service) SetChannelState(source ports.ChannelStateSource) {
	s.channelState = source
}

func (s *Service) SetRouteSource(source ports.RouteSource) {
	s.routes = source
}

func (s *Service) SetModelMappingSource(source ports.ModelMappingSource) {
	s.mappings = source
}

// InvalidateModelMappings drops the cached public_id -> upstream_id table so
// admin edits take effect immediately.
func (s *Service) InvalidateModelMappings() {
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	s.mappingCache = nil
	s.mappingRead = time.Time{}
}

const modelMappingTTL = time.Minute

func (s *Service) modelMappingIndex(ctx context.Context) map[string]map[string]string {
	if s.mappings == nil {
		return nil
	}
	s.mappingMu.RLock()
	if s.mappingCache != nil && s.now().Sub(s.mappingRead) < modelMappingTTL {
		cached := s.mappingCache
		s.mappingMu.RUnlock()
		return cached
	}
	s.mappingMu.RUnlock()

	rows, err := s.mappings.ListModelMappings(ctx)
	if err != nil {
		s.mappingMu.RLock()
		stale := s.mappingCache
		s.mappingMu.RUnlock()
		return stale
	}
	index := make(map[string]map[string]string)
	for _, row := range rows {
		channelIndex := index[row.Channel]
		if channelIndex == nil {
			channelIndex = make(map[string]string)
			index[row.Channel] = channelIndex
		}
		channelIndex[row.PublicID] = row.UpstreamID
	}
	s.mappingMu.Lock()
	s.mappingCache = index
	s.mappingRead = s.now()
	s.mappingMu.Unlock()
	return index
}

// resolvePublicModel translates a client-facing "channel/public_id" or bare
// public_id into its channel + upstream id. ok=false means the value is not a
// known public id and the caller should fall back to direct routing.
func (s *Service) resolvePublicModel(ctx context.Context, value string) (channel, upstream, publicID string, ok bool) {
	index := s.modelMappingIndex(ctx)
	if len(index) == 0 {
		return "", "", "", false
	}
	if targetChannel, public, found := strings.Cut(value, "/"); found {
		channelIndex := index[targetChannel]
		if channelIndex == nil {
			return "", "", "", false
		}
		if upstream, hit := channelIndex[public]; hit {
			return targetChannel, upstream, public, true
		}
		return "", "", "", false
	}
	// Bare public id: unambiguous only when a single channel declares it.
	for channel, channelIndex := range index {
		if upstream, hit := channelIndex[value]; hit {
			return channel, upstream, value, true
		}
	}
	return "", "", "", false
}

type modelTarget struct {
	Channel  string
	Upstream string
	Alias    string
	// PublicID is set when the request used a client-facing public model id;
	// scope checks accept either it or the upstream id.
	PublicID string
	Depth    int
}

// targetModelAllowed accepts either the upstream id or the public id the
// client used, so API keys may be scoped with clean public model names.
func targetModelAllowed(keyModels []string, target modelTarget) bool {
	if scope.ModelAllowed(keyModels, target.Channel, target.Upstream) {
		return true
	}
	return target.PublicID != "" && scope.ModelAllowed(keyModels, target.Channel, target.PublicID)
}

func annotateTargetError(err error, target modelTarget) error {	serviceErr, ok := err.(*Error)
	if !ok {
		return err
	}
	serviceErr.Channel = target.Channel
	serviceErr.UpstreamModel = target.Upstream
	serviceErr.RouteAlias = target.Alias
	serviceErr.FallbackDepth = target.Depth
	return serviceErr
}

func (s *Service) resolveTargets(ctx context.Context, value string) ([]modelTarget, error) {
	value = strings.TrimSpace(value)
	if channel, upstream, publicID, ok := s.resolvePublicModel(ctx, value); ok {
		return []modelTarget{{Channel: channel, Upstream: upstream, PublicID: publicID}}, nil
	}
	if strings.Contains(value, "/") {
		channel, upstream, ok := strings.Cut(value, "/")
		if !ok || channel == "" || upstream == "" {
			return nil, &Error{Status: 404, Code: "model_not_found", Message: "model must use channel/model format"}
		}
		return []modelTarget{{Channel: channel, Upstream: upstream}}, nil
	}
	if routeTargets, err := s.resolveRouteTargets(ctx, value); routeTargets != nil || err != nil {
		return routeTargets, err
	}
	return nil, &Error{Status: 404, Code: "model_not_found", Message: "model route was not found"}
}

func (s *Service) resolveRouteTargets(ctx context.Context, value string) ([]modelTarget, error) {
	if s.routes == nil || value == "" {
		return nil, nil
	}
	routes, err := s.routes.ListRoutes(ctx)
	if err != nil {
		return nil, &Error{Status: 503, Code: "route_unavailable", Message: "route configuration is unavailable"}
	}
	for _, route := range routes {
		if route.Alias != value || !route.Enabled || len(route.Targets) == 0 {
			continue
		}
		result := make([]modelTarget, 0, len(route.Targets))
		for index, target := range route.Targets {
			if target.Channel == "" || target.Model == "" {
				continue
			}
			upstream := target.Model
			if channel, suffix, ok := strings.Cut(upstream, "/"); ok {
				if channel != target.Channel || suffix == "" {
					return nil, &Error{Status: 422, Code: "route_invalid", Message: "route target does not match its channel"}
				}
				upstream = suffix
			}
			result = append(result, modelTarget{Channel: target.Channel, Upstream: upstream, Alias: route.Alias, Depth: index})
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	return nil, nil
}

func (s *Service) resolveModel(ctx context.Context, value string) (string, string, string, error) {
	targets, err := s.resolveTargets(ctx, value)
	if err != nil {
		return "", "", "", err
	}
	target := targets[0]
	return target.Channel, target.Upstream, target.Alias, nil
}

func (s *Service) channelEnabled(ctx context.Context, channel string) (bool, error) {
	if s.channelState == nil {
		return true, nil
	}
	return s.channelState.ChannelEnabled(ctx, channel)
}

func channelDisabledError() *Error {
	return &Error{Status: 503, Code: "channel_disabled", Message: "channel is disabled by local management configuration"}
}

func (s *Service) Record(ctx context.Context, record ports.RequestRecord) error {
	if s.recorder == nil {
		return nil
	}
	return s.recorder.RecordRequest(ctx, record)
}

func (s *Service) Authenticate(ctx context.Context, authorization, xAPIKey string) (ports.APIKey, error) {
	return s.authenticate(ctx, authorization, xAPIKey)
}

func (s *Service) Models(ctx context.Context, authorization, xAPIKey string) ([]ports.ModelDescriptor, error) {
	key, err := s.authenticate(ctx, authorization, xAPIKey)
	if err != nil {
		return nil, err
	}
	mappingIndex := s.modelMappingIndex(ctx)
	result := make([]ports.ModelDescriptor, 0)
	var unavailable int
	var firstError error
	for channel, adapter := range s.adapters {
		enabled, stateErr := s.channelEnabled(ctx, channel)
		if stateErr != nil {
			unavailable++
			if firstError == nil {
				firstError = stateErr
			}
			continue
		}
		if !enabled {
			continue
		}
		var models []ports.ModelDescriptor
		var err error
		if s.pool != nil {
			candidates, candidateErr := s.pool.Candidates(ctx, channel, "", true)
			if candidateErr != nil {
				unavailable++
				if firstError == nil {
					firstError = candidateErr
				}
				continue
			}
			if len(candidates) == 0 {
				continue
			}
			if accountAdapter, ok := adapter.(ports.AccountModelAdapter); ok {
				lease, acquireErr := s.pool.Acquire(ctx, channel, "", true)
				if acquireErr != nil {
					continue
				}
				models, err = accountAdapter.ModelsWithAccount(ctx, lease.Candidate.NativeID)
				lease.Release()
			} else {
				models, err = adapter.Models(ctx)
			}
		} else {
			models, err = adapter.Models(ctx)
		}
		if err != nil {
			unavailable++
			if firstError == nil {
				firstError = err
			}
			continue
		}
		s.rememberModels(channel, models)
		channelPublicIDs := mappingIndex[channel]
		for _, model := range models {
			publicID := model.PublicID
			if publicID == "" {
				publicID = channelPublicIDs[model.UpstreamID]
			}
			if !targetModelAllowed(key.Models, modelTarget{Channel: channel, Upstream: model.UpstreamID, PublicID: publicID}) || !scope.ChannelAllowed(key.Channels, channel) {
				continue
			}
			model.Channel = channel
			model.PublicID = publicID
			result = append(result, model)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Channel == result[j].Channel {
			return result[i].UpstreamID < result[j].UpstreamID
		}
		return result[i].Channel < result[j].Channel
	})
	if len(result) == 0 && unavailable > 0 {
		if firstError != nil {
			return nil, classifyAdapterError(firstError)
		}
		return nil, &Error{Status: 503, Code: "adapter_unavailable", Message: "no configured channel has an available model catalogue"}
	}
	return result, nil
}

func (s *Service) Chat(ctx context.Context, authorization, xAPIKey string, input ports.ChatInput) (ports.APIKey, ports.ChatResult, error) {
	key, err := s.authenticate(ctx, authorization, xAPIKey)
	if err != nil {
		return ports.APIKey{}, ports.ChatResult{}, err
	}
	targets, err := s.resolveTargets(ctx, input.Model)
	if err != nil {
		return key, ports.ChatResult{}, err
	}
	var lastErr error
	for _, target := range targets {
		channel, upstreamModel := target.Channel, target.Upstream
		setTargetError := func(err error) { lastErr = annotateTargetError(err, target) }
		if !scope.ChannelAllowed(key.Channels, channel) {
			setTargetError(&Error{Status: 403, Code: "channel_not_allowed", Message: "API key is not authorized for this channel"})
			if len(targets) == 1 {
				return key, ports.ChatResult{}, lastErr
			}
			continue
		}
		if !targetModelAllowed(key.Models, target) {
			setTargetError(&Error{Status: 403, Code: "model_not_allowed", Message: "API key is not authorized for this route target"})
			if len(targets) == 1 {
				return key, ports.ChatResult{}, lastErr
			}
			continue
		}
		enabled, stateErr := s.channelEnabled(ctx, channel)
		if stateErr != nil {
			setTargetError(&Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"})
			continue
		}
		if !enabled {
			setTargetError(channelDisabledError())
			continue
		}
		adapter, ok := s.adapters[channel]
		if !ok {
			setTargetError(&Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"})
			continue
		}
		if s.pool != nil {
			candidates, candidateErr := s.pool.Candidates(ctx, channel, upstreamModel, true)
			if candidateErr != nil || len(candidates) == 0 {
				setTargetError(&Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"})
				continue
			}
		}
		models, modelErr := s.modelsForChannel(ctx, channel, upstreamModel)
		if modelErr != nil {
			setTargetError(&Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"})
			continue
		}
		found := false
		for _, model := range models {
			if model.UpstreamID == upstreamModel && contains(model.Capabilities, "chat") {
				found = true
				break
			}
		}
		if !found {
			setTargetError(&Error{Status: 404, Code: "model_not_found", Message: "model was not found"})
			continue
		}
		adapterInput := input
		adapterInput.Model = upstreamModel
		if accountAdapter, accountAware := adapter.(ports.AccountAwareAdapter); accountAware && s.pool != nil {
			excluded := map[string]struct{}{}
			for {
				lease, acquireErr := s.pool.AcquireExcluding(ctx, channel, upstreamModel, true, excluded)
				if acquireErr != nil {
					if lastErr == nil {
						setTargetError(&Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"})
					}
					break
				}
				result, chatErr := accountAdapter.ChatWithAccount(ctx, adapterInput, lease.Candidate.NativeID)
				accountID := lease.Candidate.ID
				lease.Release()
				if chatErr == nil {
					if result.ID == "" {
						result.ID = "chatcmpl_" + fmt.Sprintf("%d", s.now().UnixNano())
					}
					if result.CreatedAt == 0 {
						result.CreatedAt = s.now().Unix()
					}
					if result.FinishReason == "" {
						result.FinishReason = "stop"
					}
					result.UpstreamModel = upstreamModel
					result.Channel = channel
					result.RouteAlias = target.Alias
					result.FallbackDepth = target.Depth
					return key, result, nil
				}
				setTargetError(classifyAdapterError(chatErr))
				if !retryableRouteError(lastErr) {
					return key, ports.ChatResult{}, lastErr
				}
				excluded[accountID] = struct{}{}
			}
		} else {
			result, chatErr := adapter.Chat(ctx, adapterInput)
			if chatErr != nil {
				setTargetError(classifyAdapterError(chatErr))
				if !retryableRouteError(lastErr) {
					return key, ports.ChatResult{}, lastErr
				}
				continue
			}
			if result.ID == "" {
				result.ID = "chatcmpl_" + fmt.Sprintf("%d", s.now().UnixNano())
			}
			if result.CreatedAt == 0 {
				result.CreatedAt = s.now().Unix()
			}
			if result.FinishReason == "" {
				result.FinishReason = "stop"
			}
			result.UpstreamModel = upstreamModel
			result.Channel = channel
			result.RouteAlias = target.Alias
			result.FallbackDepth = target.Depth
			return key, result, nil
		}
	}
	if lastErr != nil {
		return key, ports.ChatResult{}, lastErr
	}
	return key, ports.ChatResult{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "no route target is available"}
}

func retryableRouteError(err error) bool {
	serviceError, ok := err.(*Error)
	if !ok {
		return false
	}
	switch serviceError.Status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return serviceError.Code == "upstream_unavailable" || serviceError.Code == "upstream_timeout" || serviceError.Code == "account_unavailable" || serviceError.Code == "channel_disabled" || serviceError.Code == "credential_rejected" || serviceError.Code == "token_expired" || serviceError.Code == "refresh_token_invalid"
	}
}

func (s *Service) ChatStream(ctx context.Context, authorization, xAPIKey string, input ports.ChatInput, emit func(ports.StreamChunk) error) (ports.APIKey, error) {
	key, err := s.authenticate(ctx, authorization, xAPIKey)
	if err != nil {
		return ports.APIKey{}, err
	}
	targets, err := s.resolveTargets(ctx, input.Model)
	if err != nil {
		return key, err
	}
	var lastErr error
	for _, target := range targets {
		channel, upstreamModel := target.Channel, target.Upstream
		setTargetError := func(err error) { lastErr = annotateTargetError(err, target) }
		if !scope.ChannelAllowed(key.Channels, channel) {
			setTargetError(&Error{Status: 403, Code: "channel_not_allowed", Message: "API key is not authorized for this channel"})
			if len(targets) == 1 {
				return key, lastErr
			}
			continue
		}
		if !targetModelAllowed(key.Models, target) {
			setTargetError(&Error{Status: 403, Code: "model_not_allowed", Message: "API key is not authorized for this route target"})
			if len(targets) == 1 {
				return key, lastErr
			}
			continue
		}
		enabled, stateErr := s.channelEnabled(ctx, channel)
		if stateErr != nil {
			setTargetError(&Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"})
			continue
		}
		if !enabled {
			setTargetError(channelDisabledError())
			continue
		}
		adapter, ok := s.adapters[channel]
		if !ok {
			setTargetError(&Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"})
			continue
		}
		if s.pool != nil {
			candidates, candidateErr := s.pool.Candidates(ctx, channel, upstreamModel, true)
			if candidateErr != nil || len(candidates) == 0 {
				setTargetError(&Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"})
				continue
			}
		}
		models, modelErr := s.modelsForChannel(ctx, channel, upstreamModel)
		if modelErr != nil {
			setTargetError(&Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"})
			continue
		}
		found := false
		for _, model := range models {
			if model.UpstreamID == upstreamModel && contains(model.Capabilities, "chat") {
				found = true
				break
			}
		}
		if !found {
			setTargetError(&Error{Status: 404, Code: "model_not_found", Message: "model was not found"})
			continue
		}
		adapterInput := input
		adapterInput.Model = upstreamModel
		if streamAdapter, ok := adapter.(ports.AccountStreamAdapter); ok && s.pool != nil {
			excluded := map[string]struct{}{}
			for {
				lease, acquireErr := s.pool.AcquireExcluding(ctx, channel, upstreamModel, true, excluded)
				if acquireErr != nil {
					if lastErr == nil {
						setTargetError(&Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"})
					}
					break
				}
				emitted := false
				emitChunk := func(chunk ports.StreamChunk) error {
					emitted = true
					chunk.Channel = channel
					chunk.UpstreamModel = upstreamModel
					chunk.RouteAlias = target.Alias
					chunk.FallbackDepth = target.Depth
					return emit(chunk)
				}
				streamErr := streamAdapter.ChatStreamWithAccount(ctx, adapterInput, lease.Candidate.NativeID, emitChunk)
				accountID := lease.Candidate.ID
				lease.Release()
				if streamErr == nil {
					return key, nil
				}
				setTargetError(classifyAdapterError(streamErr))
				if emitted || !retryableRouteError(lastErr) {
					return key, lastErr
				}
				excluded[accountID] = struct{}{}
			}
		}
		result, chatErr := adapter.Chat(ctx, adapterInput)
		if chatErr != nil {
			setTargetError(classifyAdapterError(chatErr))
			if !retryableRouteError(lastErr) {
				return key, lastErr
			}
			continue
		}
		chunk := ports.StreamChunk{ID: result.ID, Text: result.Text, Reasoning: result.Reasoning, FinishReason: result.FinishReason, CreatedAt: result.CreatedAt, Usage: result.Usage, Channel: channel, UpstreamModel: upstreamModel, RouteAlias: target.Alias, FallbackDepth: target.Depth}
		if err := emit(chunk); err != nil {
			return key, err
		}
		return key, nil
	}
	if lastErr != nil {
		return key, lastErr
	}
	return key, &Error{Status: 503, Code: "adapter_unavailable", Message: "no route target is available"}
}

func (s *Service) Capability(ctx context.Context, authorization, xAPIKey string, input ports.CapabilityInput) (ports.APIKey, ports.CapabilityResult, error) {
	key, err := s.authenticate(ctx, authorization, xAPIKey)
	if err != nil {
		return ports.APIKey{}, ports.CapabilityResult{}, err
	}
	return s.CapabilityWithKey(ctx, key, input)
}

func (s *Service) CapabilityWithKey(ctx context.Context, key ports.APIKey, input ports.CapabilityInput) (ports.APIKey, ports.CapabilityResult, error) {
	targets, resolveErr := s.resolveTargets(ctx, input.Model)
	if resolveErr != nil {
		return key, ports.CapabilityResult{}, resolveErr
	}
	target := targets[0]
	channel, upstreamModel := target.Channel, target.Upstream
	if !scope.ChannelAllowed(key.Channels, channel) {
		return key, ports.CapabilityResult{}, &Error{Status: 403, Code: "channel_not_allowed", Message: "API key is not authorized for this channel"}
	}
	if !targetModelAllowed(key.Models, target) {
		return key, ports.CapabilityResult{}, &Error{Status: 403, Code: "model_not_allowed", Message: "API key is not authorized for this model"}
	}
	if enabled, stateErr := s.channelEnabled(ctx, channel); stateErr != nil {
		return key, ports.CapabilityResult{}, &Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"}
	} else if !enabled {
		return key, ports.CapabilityResult{}, channelDisabledError()
	}
	adapter, ok := s.adapters[channel]
	if !ok {
		return key, ports.CapabilityResult{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
	}
	capabilityAdapter, ok := adapter.(ports.AccountCapabilityAdapter)
	if !ok || s.pool == nil {
		return key, ports.CapabilityResult{}, &Error{Status: 501, Code: "capability_not_supported", Message: "channel capability is not implemented in the Go runtime"}
	}
	candidates, err := s.pool.Candidates(ctx, channel, upstreamModel, true)
	if err != nil || len(candidates) == 0 {
		return key, ports.CapabilityResult{}, &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
	}
	lease, err := s.pool.Acquire(ctx, channel, upstreamModel, true)
	if err != nil {
		return key, ports.CapabilityResult{}, &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
	}
	defer lease.Release()
	input.Model = upstreamModel
	result, err := capabilityAdapter.CapabilityWithAccount(ctx, input, lease.Candidate.NativeID)
	if err != nil {
		return key, ports.CapabilityResult{}, classifyAdapterError(err)
	}
	if result.StatusCode == 0 {
		result.StatusCode = 200
	}
	return key, result, nil
}

func (s *Service) AdminCapability(ctx context.Context, input ports.CapabilityInput) (ports.CapabilityResult, error) {
	channel, upstreamModel, _, resolveErr := s.resolveModel(ctx, input.Model)
	if resolveErr != nil {
		return ports.CapabilityResult{}, resolveErr
	}
	adapter, ok := s.adapters[channel]
	if !ok {
		return ports.CapabilityResult{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
	}
	if enabled, stateErr := s.channelEnabled(ctx, channel); stateErr != nil {
		return ports.CapabilityResult{}, &Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"}
	} else if !enabled {
		return ports.CapabilityResult{}, channelDisabledError()
	}
	capabilityAdapter, ok := adapter.(ports.AccountCapabilityAdapter)
	if !ok || s.pool == nil {
		return ports.CapabilityResult{}, &Error{Status: 501, Code: "capability_not_supported", Message: "channel capability is not implemented in the Go runtime"}
	}
	candidates, err := s.pool.Candidates(ctx, channel, upstreamModel, true)
	if err != nil || len(candidates) == 0 {
		return ports.CapabilityResult{}, &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
	}
	lease, err := s.pool.Acquire(ctx, channel, upstreamModel, true)
	if err != nil {
		return ports.CapabilityResult{}, &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
	}
	defer lease.Release()
	input.Model = upstreamModel
	result, err := capabilityAdapter.CapabilityWithAccount(ctx, input, lease.Candidate.NativeID)
	if err != nil {
		return ports.CapabilityResult{}, classifyAdapterError(err)
	}
	if result.StatusCode == 0 {
		result.StatusCode = 200
	}
	return result, nil
}

func (s *Service) AdminChat(ctx context.Context, input ports.ChatInput) (ports.ChatResult, error) {
	targets, err := s.resolveTargets(ctx, input.Model)
	if err != nil {
		return ports.ChatResult{}, err
	}
	var lastErr error
	for _, target := range targets {
		channel, upstreamModel := target.Channel, target.Upstream
		adapter, ok := s.adapters[channel]
		if !ok {
			lastErr = &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
			continue
		}
		enabled, stateErr := s.channelEnabled(ctx, channel)
		if stateErr != nil {
			lastErr = &Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"}
			continue
		}
		if !enabled {
			lastErr = channelDisabledError()
			continue
		}
		if s.pool != nil {
			candidates, candidateErr := s.pool.Candidates(ctx, channel, upstreamModel, true)
			if candidateErr != nil || len(candidates) == 0 {
				lastErr = &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
				continue
			}
		}
		models, modelErr := s.modelsForChannel(ctx, channel, upstreamModel)
		if modelErr != nil {
			lastErr = &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
			continue
		}
		found := false
		for _, model := range models {
			if model.UpstreamID == upstreamModel && contains(model.Capabilities, "chat") {
				found = true
				break
			}
		}
		if !found {
			lastErr = &Error{Status: 404, Code: "model_not_found", Message: "model was not found"}
			continue
		}
		input.Model = upstreamModel
		if accountAdapter, accountAware := adapter.(ports.AccountAwareAdapter); accountAware && s.pool != nil {
			excluded := map[string]struct{}{}
			for {
				lease, acquireErr := s.pool.AcquireExcluding(ctx, channel, upstreamModel, true, excluded)
				if acquireErr != nil {
					if lastErr == nil {
						lastErr = &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
					}
					break
				}
				result, chatErr := accountAdapter.ChatWithAccount(ctx, input, lease.Candidate.NativeID)
				accountID := lease.Candidate.ID
				lease.Release()
				if chatErr == nil {
					result = normalizeResult(result, s.now())
					result.UpstreamModel = upstreamModel
					result.Channel = channel
					result.RouteAlias = target.Alias
					result.FallbackDepth = target.Depth
					return result, nil
				}
				lastErr = classifyAdapterError(chatErr)
				if !retryableRouteError(lastErr) {
					return ports.ChatResult{}, lastErr
				}
				excluded[accountID] = struct{}{}
			}
		} else {
			result, chatErr := adapter.Chat(ctx, input)
			if chatErr != nil {
				lastErr = classifyAdapterError(chatErr)
				if !retryableRouteError(lastErr) {
					return ports.ChatResult{}, lastErr
				}
				continue
			}
			result = normalizeResult(result, s.now())
			result.UpstreamModel = upstreamModel
			result.Channel = channel
			result.RouteAlias = target.Alias
			result.FallbackDepth = target.Depth
			return result, nil
		}
	}
	if lastErr != nil {
		return ports.ChatResult{}, lastErr
	}
	return ports.ChatResult{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "no route target is available"}
}

func (s *Service) AdminChatStream(ctx context.Context, input ports.ChatInput, emit func(ports.StreamChunk) error) error {
	targets, err := s.resolveTargets(ctx, input.Model)
	if err != nil {
		return err
	}
	var lastErr error
	for _, target := range targets {
		channel, upstreamModel := target.Channel, target.Upstream
		adapter, ok := s.adapters[channel]
		if !ok {
			lastErr = &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
			continue
		}
		enabled, stateErr := s.channelEnabled(ctx, channel)
		if stateErr != nil {
			lastErr = &Error{Status: 503, Code: "channel_state_unavailable", Message: "channel state is unavailable"}
			continue
		}
		if !enabled {
			lastErr = channelDisabledError()
			continue
		}
		if s.pool == nil {
			lastErr = &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
			continue
		}
		models, modelErr := s.modelsForChannel(ctx, channel, upstreamModel)
		if modelErr != nil {
			lastErr = &Error{Status: 503, Code: "adapter_unavailable", Message: "channel adapter is not available"}
			continue
		}
		found := false
		for _, model := range models {
			if model.UpstreamID == upstreamModel && contains(model.Capabilities, "chat") {
				found = true
				break
			}
		}
		if !found {
			lastErr = &Error{Status: 404, Code: "model_not_found", Message: "model was not found"}
			continue
		}
		adapterInput := input
		adapterInput.Model = upstreamModel
		if streamAdapter, ok := adapter.(ports.AccountStreamAdapter); ok {
			excluded := map[string]struct{}{}
			for {
				lease, acquireErr := s.pool.AcquireExcluding(ctx, channel, upstreamModel, true, excluded)
				if acquireErr != nil {
					if lastErr == nil {
						lastErr = &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
					}
					break
				}
				emitted := false
				emitChunk := func(chunk ports.StreamChunk) error {
					emitted = true
					chunk.Channel = channel
					chunk.UpstreamModel = upstreamModel
					chunk.RouteAlias = target.Alias
					chunk.FallbackDepth = target.Depth
					return emit(chunk)
				}
				streamErr := streamAdapter.ChatStreamWithAccount(ctx, adapterInput, lease.Candidate.NativeID, emitChunk)
				accountID := lease.Candidate.ID
				lease.Release()
				if streamErr == nil {
					return nil
				}
				lastErr = classifyAdapterError(streamErr)
				if emitted || !retryableRouteError(lastErr) {
					return lastErr
				}
				excluded[accountID] = struct{}{}
			}
			continue
		}
		accountAdapter, accountOK := adapter.(ports.AccountAwareAdapter)
		if !accountOK {
			lastErr = &Error{Status: 503, Code: "adapter_unavailable", Message: "channel account adapter is not available"}
			continue
		}
		excluded := map[string]struct{}{}
		for {
			lease, acquireErr := s.pool.AcquireExcluding(ctx, channel, upstreamModel, true, excluded)
			if acquireErr != nil {
				if lastErr == nil {
					lastErr = &Error{Status: 503, Code: "account_unavailable", Message: "no account is available for this channel"}
				}
				break
			}
			result, chatErr := accountAdapter.ChatWithAccount(ctx, adapterInput, lease.Candidate.NativeID)
			accountID := lease.Candidate.ID
			lease.Release()
			if chatErr == nil {
				chunk := ports.StreamChunk{ID: result.ID, Text: result.Text, Reasoning: result.Reasoning, FinishReason: result.FinishReason, CreatedAt: result.CreatedAt, Usage: result.Usage, Channel: channel, UpstreamModel: upstreamModel, RouteAlias: target.Alias, FallbackDepth: target.Depth}
				return emit(chunk)
			}
			lastErr = classifyAdapterError(chatErr)
			if !retryableRouteError(lastErr) {
				return lastErr
			}
			excluded[accountID] = struct{}{}
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return &Error{Status: 503, Code: "adapter_unavailable", Message: "no route target is available"}
}

func normalizeResult(result ports.ChatResult, now time.Time) ports.ChatResult {
	if result.CreatedAt == 0 {
		result.CreatedAt = now.Unix()
	}
	if result.FinishReason == "" {
		result.FinishReason = "stop"
	}
	return result
}

func (s *Service) authenticate(ctx context.Context, authorization, xAPIKey string) (ports.APIKey, error) {
	if s.keys == nil {
		return ports.APIKey{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "gateway authentication is unavailable"}
	}
	key, err := s.keys.Authenticate(ctx, authorization, xAPIKey)
	if err == nil {
		return key, nil
	}
	if errors.Is(err, ports.ErrKeyRateLimited) {
		return ports.APIKey{}, &Error{Status: 429, Code: "rate_limited", Message: "API key rate limit exceeded", Headers: map[string]string{"Retry-After": "60"}}
	}
	if errors.Is(err, ports.ErrInvalidKey) {
		return ports.APIKey{}, &Error{Status: 401, Code: "invalid_api_key", Message: "valid API key required"}
	}
	return ports.APIKey{}, &Error{Status: 503, Code: "adapter_unavailable", Message: "gateway authentication is unavailable"}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (s *Service) modelsForChannel(ctx context.Context, channel, model string) ([]ports.ModelDescriptor, error) {
	if cached, ok := s.cachedModels(channel); ok {
		return cached, nil
	}
	adapter, ok := s.adapters[channel]
	if !ok {
		return nil, fmt.Errorf("channel adapter is unavailable")
	}
	if s.pool != nil {
		candidates, err := s.pool.Candidates(ctx, channel, model, true)
		if err != nil || len(candidates) == 0 {
			return nil, fmt.Errorf("no account is available")
		}
		if accountAdapter, ok := adapter.(ports.AccountModelAdapter); ok {
			lease, err := s.pool.Acquire(ctx, channel, model, true)
			if err != nil {
				return nil, err
			}
			models, err := accountAdapter.ModelsWithAccount(ctx, lease.Candidate.NativeID)
			lease.Release()
			if err == nil {
				s.rememberModels(channel, models)
			}
			return models, err
		}
	}
	models, err := adapter.Models(ctx)
	if err == nil {
		s.rememberModels(channel, models)
	}
	return models, err
}

func (s *Service) rememberModels(channel string, models []ports.ModelDescriptor) {
	if strings.TrimSpace(channel) == "" {
		return
	}
	copyModels := append([]ports.ModelDescriptor(nil), models...)
	s.modelMu.Lock()
	s.modelCache[channel] = modelCacheEntry{models: copyModels, updatedAt: s.now()}
	s.modelMu.Unlock()
}

func (s *Service) cachedModels(channel string) ([]ports.ModelDescriptor, bool) {
	s.modelMu.RLock()
	entry, ok := s.modelCache[channel]
	s.modelMu.RUnlock()
	if !ok || s.now().Sub(entry.updatedAt) > modelCacheTTL {
		return nil, false
	}
	return append([]ports.ModelDescriptor(nil), entry.models...), true
}

type classifiedAdapterError interface {
	StatusCode() int
	Kind() string
}

// publicAdapterError lets an adapter expose an operator-actionable message
// (never containing credentials or upstream bodies), e.g. "sign in again".
type publicAdapterError interface {
	PublicMessage() string
}

func classifyAdapterError(err error) *Error {
	var classified classifiedAdapterError
	if errors.As(err, &classified) {
		status := classified.StatusCode()
		if status < 400 {
			status = 502
		}
		if status >= 500 && status != 504 {
			status = 502
		}
		message := "adapter request failed"
		var public publicAdapterError
		if errors.As(err, &public) {
			if text := public.PublicMessage(); text != "" {
				message = text
			}
		}
		return &Error{Status: status, Code: classified.Kind(), Message: message}
	}
	return &Error{Status: 502, Code: "adapter_error", Message: "adapter request failed"}
}
