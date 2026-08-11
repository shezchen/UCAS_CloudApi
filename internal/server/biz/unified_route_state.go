package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

// RouteKey identifies the smallest independently testable upstream route.
// CredentialID is a one-way fingerprint; raw credentials must never enter
// routing state, logs, or API responses.
type RouteKey struct {
	ChannelID    int
	CredentialID string
	ActualModel  string
	APIFormat    string
	// ConfigRevision is an opaque digest of the effective endpoint/proxy
	// configuration. It prevents a verdict produced for an old URL or proxy from
	// being reused after the owner edits the channel, without exposing either.
	ConfigRevision string
}

func (k RouteKey) String() string {
	return fmt.Sprintf("%d:%s:%s:%s:%s", k.ChannelID, k.CredentialID, k.ActualModel, k.APIFormat, k.ConfigRevision)
}

// RouteConfigRevision fingerprints only transport-affecting configuration.
// Proxy credentials and private endpoint URLs are hashed and never retained in
// RouteKey, logs, or API output.
func RouteConfigRevision(channel *Channel, apiFormat string, overrideProxy *httpclient.ProxyConfig) string {
	if channel == nil || channel.Channel == nil {
		return ""
	}
	var endpoint objects.ChannelEndpoint
	for _, candidate := range channel.ResolveEndpoints() {
		if candidate.APIFormat == apiFormat {
			endpoint = candidate
			break
		}
	}
	var channelProxy *httpclient.ProxyConfig
	if channel.Settings != nil {
		channelProxy = channel.Settings.Proxy
	}
	payload := struct {
		ChannelType  string                  `json:"channel_type"`
		BaseURL      string                  `json:"base_url"`
		Endpoint     objects.ChannelEndpoint `json:"endpoint"`
		ChannelProxy *httpclient.ProxyConfig `json:"channel_proxy,omitempty"`
		Override     *httpclient.ProxyConfig `json:"override_proxy,omitempty"`
	}{
		ChannelType:  string(channel.Type),
		BaseURL:      channel.BaseURL,
		Endpoint:     endpoint,
		ChannelProxy: channelProxy,
		Override:     overrideProxy,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

// CredentialFingerprint returns a stable, non-reversible identifier suitable
// for distinguishing credentials without retaining the credential itself.
func CredentialFingerprint(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}

// RouteCredentialFingerprint returns a stable, non-reversible identity for the
// credential material that an exact route actually uses. API-key-only channels
// intentionally retain CredentialFingerprint compatibility. Structured
// credentials are normalized before hashing so short-lived OAuth access-token
// refreshes do not erase a PASS when a stable refresh token identifies the same
// account.
func RouteCredentialFingerprint(channel *Channel, selectedAPIKey string) string {
	if channel == nil || channel.Channel == nil {
		return CredentialFingerprint(selectedAPIKey)
	}

	credentials := &channel.Credentials
	if credentials.IsOAuth() {
		oauthCredentials := credentials.OAuth
		if oauthCredentials == nil {
			var legacy objects.OAuthCredentials
			if err := json.Unmarshal([]byte(strings.TrimSpace(credentials.APIKey)), &legacy); err == nil {
				oauthCredentials = &legacy
			}
		}

		type oauthIdentity struct {
			Kind         string   `json:"kind"`
			ClientID     string   `json:"client_id,omitempty"`
			RefreshToken string   `json:"refresh_token,omitempty"`
			AccessToken  string   `json:"access_token,omitempty"`
			Scopes       []string `json:"scopes,omitempty"`
			LegacyRaw    string   `json:"legacy_raw,omitempty"`
		}
		identity := oauthIdentity{Kind: "oauth"}
		if oauthCredentials != nil {
			identity.ClientID = oauthCredentials.ClientID
			identity.Scopes = append([]string(nil), oauthCredentials.Scopes...)
			sort.Strings(identity.Scopes)
			if oauthCredentials.RefreshToken != "" {
				identity.RefreshToken = oauthCredentials.RefreshToken
			} else {
				identity.AccessToken = oauthCredentials.AccessToken
			}
		} else {
			// IsOAuth also recognizes the legacy JSON form. If malformed legacy
			// data reaches this point, hash it rather than collapsing accounts to
			// the empty fingerprint.
			identity.LegacyRaw = strings.TrimSpace(credentials.APIKey)
		}
		return fingerprintCredentialIdentity(identity)
	}

	// Preserve the existing API-key fingerprint exactly when no additional
	// structured credential participates in authentication.
	if credentials.GCP == nil && credentials.Azure == nil {
		return CredentialFingerprint(selectedAPIKey)
	}

	type structuredIdentity struct {
		Kind   string                   `json:"kind"`
		APIKey string                   `json:"api_key,omitempty"`
		GCP    *objects.GCPCredential   `json:"gcp,omitempty"`
		Azure  *objects.AzureCredential `json:"azure,omitempty"`
	}
	identity := structuredIdentity{
		Kind:   "structured",
		APIKey: selectedAPIKey,
		Azure:  credentials.Azure,
	}
	if credentials.GCP != nil {
		gcp := *credentials.GCP
		gcp.JSONData = canonicalCredentialJSON(gcp.JSONData)
		identity.GCP = &gcp
	}
	return fingerprintCredentialIdentity(identity)
}

// RouteCredentialFingerprints returns the current executable credential slots
// for diagnostics. Raw credentials never leave this package boundary.
func RouteCredentialFingerprints(channel *Channel) []string {
	if channel == nil || channel.Channel == nil {
		return nil
	}

	if channel.Credentials.IsOAuth() {
		return []string{RouteCredentialFingerprint(channel, "")}
	}

	keys := channel.Credentials.GetEnabledAPIKeys(channel.DisabledAPIKeys)
	if len(keys) == 0 {
		if len(channel.Credentials.GetAllAPIKeys()) > 0 {
			return nil
		}
		fingerprint := RouteCredentialFingerprint(channel, "")
		if fingerprint == "" {
			return []string{""}
		}
		return []string{fingerprint}
	}

	result := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		fingerprint := RouteCredentialFingerprint(channel, key)
		if _, duplicate := seen[fingerprint]; duplicate {
			continue
		}
		seen[fingerprint] = struct{}{}
		result = append(result, fingerprint)
	}
	sort.Strings(result)
	return result
}

func fingerprintCredentialIdentity(identity any) string {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func canonicalCredentialJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return trimmed
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(encoded)
}

// RouteAvailability is deliberately boolean. Known distinguishes an untested
// route from an authoritative TestChannel FAIL; neither state removes a route
// from the fair primary ring.
type RouteAvailability struct {
	Known         bool
	Available     bool
	LastTestAt    time.Time
	LastTestError string
}

type RouteTestSnapshot struct {
	Availability RouteAvailability
	InFlight     bool
}

// RouteTestVerdict is the only input permitted to mutate RouteAvailability.
// Completed must be false for tester cancellation, panic, infrastructure
// failure, or any run that did not reach TestChannel's normal verdict path.
type RouteTestVerdict struct {
	Completed bool
	Pass      bool
	Error     string
}

type routeRing struct {
	next           uint64
	lastChannelID  int
	hasLastChannel bool
}

const (
	// routeStateSweepInterval bounds how often the stale-entry sweep runs.
	routeStateSweepInterval = time.Hour
	// routeStateStaleAfter is how long a verdict may go unrefreshed before its
	// route entry is dropped. RouteKey embeds CredentialID and ConfigRevision,
	// so channel edits, credential rotations, and channel deletions abandon old
	// keys forever; without a sweep availability/generations grow monotonically.
	routeStateStaleAfter = 24 * time.Hour
)

// UnifiedRouteState owns the two independent routing facts:
//   - a stable per-model fair cursor used only for primary selection;
//   - TestChannel-authored boolean availability used only for rescue.
//
// It intentionally contains no score, penalty, cooldown, failure counter, or
// time-based production eligibility rule.
type UnifiedRouteState struct {
	mu           sync.RWMutex
	rings        map[string]*routeRing
	rescueRings  map[string]*routeRing
	availability map[RouteKey]RouteAvailability
	inflight     map[RouteKey]int
	generations  map[RouteKey]uint64
	nextTestGen  uint64
	lastSweep    time.Time
	tests        singleflight.Group
}

func NewUnifiedRouteState() *UnifiedRouteState {
	return &UnifiedRouteState{
		rings:        make(map[string]*routeRing),
		rescueRings:  make(map[string]*routeRing),
		availability: make(map[RouteKey]RouteAvailability),
		inflight:     make(map[RouteKey]int),
		generations:  make(map[RouteKey]uint64),
	}
}

// NextRescue rotates only within the supplied TestChannel-PASS channels. Its
// cursor is independent from primary fairness, so rescue traffic can never
// consume or rearrange a future primary slot.
func (s *UnifiedRouteState) NextRescue(model string, channelIDs []int) (int, bool) {
	ids := uniqueSortedPositiveInts(channelIDs)
	if len(ids) == 0 {
		return 0, false
	}

	key := strings.TrimSpace(model)
	s.mu.Lock()
	ring := s.rescueRings[key]
	if ring == nil {
		ring = &routeRing{}
		s.rescueRings[key] = ring
	}
	selected := ids[0]
	if ring.hasLastChannel {
		for _, channelID := range ids {
			if channelID > ring.lastChannelID {
				selected = channelID
				break
			}
		}
	}
	ring.lastChannelID = selected
	ring.hasLastChannel = true
	s.mu.Unlock()
	return selected, true
}

// NextPrimary returns the next channel in stable channel-ID order. Runtime
// availability is intentionally not an argument. Duplicate channel IDs occupy
// one outer-ring slot, so multiple credentials cannot buy extra fair share.
func (s *UnifiedRouteState) NextPrimary(model string, channelIDs []int) (int, bool) {
	ids := uniqueSortedPositiveInts(channelIDs)
	if len(ids) == 0 {
		return 0, false
	}

	key := strings.TrimSpace(model)
	s.mu.Lock()
	ring := s.rings[key]
	if ring == nil {
		ring = &routeRing{}
		s.rings[key] = ring
	}
	selected := ids[0]
	if ring.hasLastChannel {
		for _, channelID := range ids {
			if channelID > ring.lastChannelID {
				selected = channelID
				break
			}
		}
	}
	ring.lastChannelID = selected
	ring.hasLastChannel = true
	s.mu.Unlock()

	return selected, true
}

func uniqueSortedPositiveInts(values []int) []int {
	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func (s *UnifiedRouteState) Availability(key RouteKey) RouteAvailability {
	s.mu.RLock()
	availability := s.availability[key]
	s.mu.RUnlock()
	return availability
}

// ChannelAvailabilities returns a defensive snapshot for diagnostics/UI. It
// never exposes raw credentials because RouteKey contains fingerprints only.
func (s *UnifiedRouteState) ChannelAvailabilities(channelID int) map[RouteKey]RouteAvailability {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[RouteKey]RouteAvailability)
	for key, availability := range s.availability {
		if key.ChannelID == channelID {
			result[key] = availability
		}
	}
	return result
}

// ChannelTestSnapshots returns exact TestChannel state for UI aggregation.
// CredentialID and ConfigRevision are one-way digests; no secret or endpoint
// value is exposed.
func (s *UnifiedRouteState) ChannelTestSnapshots(channelID int) map[RouteKey]RouteTestSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[RouteKey]RouteTestSnapshot)
	for key, availability := range s.availability {
		if key.ChannelID == channelID {
			result[key] = RouteTestSnapshot{Availability: availability, InFlight: s.inflight[key] > 0}
		}
	}
	for key, count := range s.inflight {
		if key.ChannelID != channelID || count <= 0 {
			continue
		}
		snapshot := result[key]
		snapshot.InFlight = true
		result[key] = snapshot
	}
	return result
}

func (s *UnifiedRouteState) AvailableRoutes(channelID int, actualModel, apiFormat string) []RouteKey {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]RouteKey, 0)
	for key, availability := range s.availability {
		if key.ChannelID == channelID && key.ActualModel == actualModel && key.APIFormat == apiFormat &&
			availability.Known && availability.Available {
			result = append(result, key)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CredentialID < result[j].CredentialID })
	return result
}

// AnyAvailable reports whether TestChannel has passed any credential route for
// the given channel/model/protocol. It is used only after the fair primary has
// failed; unknown and failed routes remain eligible for future primary slots.
func (s *UnifiedRouteState) AnyAvailable(channelID int, actualModel, apiFormat string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for key, availability := range s.availability {
		if key.ChannelID == channelID &&
			key.ActualModel == actualModel &&
			key.APIFormat == apiFormat &&
			availability.Known && availability.Available {
			return true
		}
	}
	return false
}

// NextRescueCredential performs the inner credential rotation only after an
// outer rescue channel was selected. It cannot alter channel fair share.
func (s *UnifiedRouteState) NextRescueCredential(model string, channelID int, credentialIDs []string) (string, bool) {
	ids := append([]string(nil), credentialIDs...)
	sort.Strings(ids)
	ids = compactNonEmptyStrings(ids)
	if len(ids) == 0 {
		return "", false
	}

	key := fmt.Sprintf("credential:%s:%d", strings.TrimSpace(model), channelID)
	s.mu.Lock()
	ring := s.rescueRings[key]
	if ring == nil {
		ring = &routeRing{}
		s.rescueRings[key] = ring
	}
	selected := ids[ring.next%uint64(len(ids))]
	ring.next++
	s.mu.Unlock()
	return selected, true
}

// NextRescueRoute rotates exact PASS routes only after the outer channel slot
// has been chosen. Therefore a channel with many credentials or model aliases
// still receives one outer rescue share, while distinct routes within it are
// exhausted deterministically.
func (s *UnifiedRouteState) NextRescueRoute(model string, channelID int, routes []RouteKey) (RouteKey, bool) {
	unique := make(map[RouteKey]struct{}, len(routes))
	ordered := make([]RouteKey, 0, len(routes))
	for _, route := range routes {
		if route.ChannelID != channelID {
			continue
		}
		if _, ok := unique[route]; ok {
			continue
		}
		unique[route] = struct{}{}
		ordered = append(ordered, route)
	}
	if len(ordered) == 0 {
		return RouteKey{}, false
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ActualModel != ordered[j].ActualModel {
			return ordered[i].ActualModel < ordered[j].ActualModel
		}
		if ordered[i].APIFormat != ordered[j].APIFormat {
			return ordered[i].APIFormat < ordered[j].APIFormat
		}
		return ordered[i].CredentialID < ordered[j].CredentialID
	})

	key := fmt.Sprintf("route:%s:%d", strings.TrimSpace(model), channelID)
	s.mu.Lock()
	ring := s.rescueRings[key]
	if ring == nil {
		ring = &routeRing{}
		s.rescueRings[key] = ring
	}
	selected := ordered[ring.next%uint64(len(ordered))]
	ring.next++
	s.mu.Unlock()
	return selected, true
}

func compactNonEmptyStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value == "" || (len(result) > 0 && result[len(result)-1] == value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

// RecordTestVerdict is the single write gate for boolean route availability.
// Returning false means the tester did not produce a complete verdict and no
// previous value was changed.
func (s *UnifiedRouteState) RecordTestVerdict(key RouteKey, verdict RouteTestVerdict) bool {
	return s.RecordTestVerdictGeneration(key, s.NextTestGeneration(), verdict)
}

// NextTestGeneration returns a monotonic test-start generation. Test callers
// must capture it before doing I/O, not when the result arrives.
func (s *UnifiedRouteState) NextTestGeneration() uint64 {
	s.mu.Lock()
	s.nextTestGen++
	generation := s.nextTestGen
	s.mu.Unlock()
	return generation
}

// RecordTestVerdictGeneration prevents a slow, older TestChannel run from
// overwriting a newer completed verdict for the same exact route.
func (s *UnifiedRouteState) RecordTestVerdictGeneration(key RouteKey, generation uint64, verdict RouteTestVerdict) bool {
	if !verdict.Completed {
		return false
	}

	s.mu.Lock()
	if generation < s.generations[key] {
		s.mu.Unlock()
		return false
	}
	s.generations[key] = generation
	now := time.Now()
	s.availability[key] = RouteAvailability{
		Known:         true,
		Available:     verdict.Pass,
		LastTestAt:    now,
		LastTestError: verdict.Error,
	}
	s.sweepStaleLocked(now)
	s.mu.Unlock()
	return true
}

// sweepStaleLocked drops availability/generation entries whose verdict has not
// been refreshed within routeStateStaleAfter. Hooking the sweep into the only
// write path that grows those maps keeps them bounded without a background
// goroutine: no new verdicts means no growth to clean up. Entries with an
// in-flight test are kept because their verdict is about to be refreshed.
// Callers must hold s.mu for writing.
func (s *UnifiedRouteState) sweepStaleLocked(now time.Time) {
	if now.Sub(s.lastSweep) < routeStateSweepInterval {
		return
	}
	s.lastSweep = now

	for key, availability := range s.availability {
		if now.Sub(availability.LastTestAt) < routeStateStaleAfter {
			continue
		}
		if s.inflight[key] > 0 {
			continue
		}
		delete(s.availability, key)
		delete(s.generations, key)
	}
}

// TriggerTest starts (or joins) one asynchronous TestChannel run per exact
// route. The production request never waits for it before failing over.
func (s *UnifiedRouteState) TriggerTest(
	ctx context.Context,
	key RouteKey,
	run func(context.Context) (RouteTestVerdict, error),
) {
	if run == nil || key.ChannelID <= 0 || key.ActualModel == "" {
		return
	}

	testCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	result := s.tests.DoChan(key.String(), func() (value any, err error) {
		defer cancel()
		generation := s.NextTestGeneration()
		s.mu.Lock()
		s.inflight[key]++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inflight[key]--
			if s.inflight[key] <= 0 {
				delete(s.inflight, key)
			}
			s.mu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(testCtx, "programmatic TestChannel panicked",
					log.Int("channel_id", key.ChannelID),
					log.String("actual_model", key.ActualModel),
					log.String("api_format", key.APIFormat),
					log.Any("panic", recovered),
				)
				err = fmt.Errorf("programmatic TestChannel panicked: %v", recovered)
			}
		}()

		verdict, runErr := run(testCtx)
		if runErr != nil || testCtx.Err() != nil || !verdict.Completed {
			return nil, runErr
		}
		s.RecordTestVerdictGeneration(key, generation, verdict)
		return verdict, nil
	})

	// DoChan is buffered and starts the singleflight worker itself. Drain its
	// result so failed tests remain observable without coupling them to the user
	// request. This goroutine has the required panic guard.
	go func() {
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(context.Background(), "programmatic TestChannel result handler panicked", log.Any("panic", recovered))
			}
		}()
		outcome := <-result
		if outcome.Err != nil {
			log.Warn(testCtx, "programmatic TestChannel did not produce a verdict",
				log.Int("channel_id", key.ChannelID),
				log.String("actual_model", key.ActualModel),
				log.String("api_format", key.APIFormat),
				log.Cause(outcome.Err),
			)
		}
	}()
}
