package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm/streams"
)

// MaxErrorBodySize is the maximum number of bytes read from an upstream error
// response body. Error bodies beyond this size are truncated to prevent OOM
// from pathological upstream responses that echo large request payloads in
// validation error messages, producing response bodies of 1+ GB.
const MaxErrorBodySize = 1 << 20 // 1 MB

// HttpClient implements the HttpClient interface.
type HttpClient struct {
	client            *http.Client
	proxyConfig       *ProxyConfig
	opts              []ClientOption
	publicNetworkOnly bool
	resolver          publicNetworkResolver
}

// ClientOption configures an HttpClient.
type ClientOption func(*clientOptions)

type clientOptions struct {
	insecureSkipVerify bool
	publicNetworkOnly  bool
	resolver           publicNetworkResolver
}

// WithInsecureSkipVerify disables TLS certificate verification.
func WithInsecureSkipVerify(skip bool) ClientOption {
	return func(o *clientOptions) {
		o.insecureSkipVerify = skip
	}
}

// WithPublicNetworkOnly restricts requests to HTTP(S)/WS(S) endpoints that
// resolve exclusively to public IP addresses. DNS is resolved again at dial
// time and the validated IP is dialed directly, preventing DNS-rebinding
// between URL validation and connection establishment. Environment proxies are
// disabled; an explicitly configured URL proxy is retained and subject to the
// same public-address dial guard.
func WithPublicNetworkOnly() ClientOption {
	return func(o *clientOptions) {
		o.publicNetworkOnly = true
	}
}

func withPublicNetworkResolver(resolver publicNetworkResolver) ClientOption {
	return func(o *clientOptions) {
		o.resolver = resolver
	}
}

type publicNetworkResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

var restrictedPublicNetworkPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // current network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking
	netip.MustParsePrefix("2001:10::/28"),    // deprecated ORCHID
	netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // deprecated 6to4
}

func publicNetworkAddressRestricted(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast() ||
		!addr.IsGlobalUnicast() {
		return true
	}

	for _, prefix := range restrictedPublicNetworkPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

func validatePublicNetworkURLWithResolver(
	ctx context.Context,
	rawURL string,
	resolver publicNetworkResolver,
	allowedSchemes map[string]struct{},
	allowUserinfo bool,
) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if parsed.Opaque != "" || parsed.Host == "" {
		return fmt.Errorf("URL must be absolute and include a host")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if _, ok := allowedSchemes[scheme]; !ok {
		return fmt.Errorf("URL scheme %q is not supported", parsed.Scheme)
	}

	if parsed.User != nil && !allowUserinfo {
		return fmt.Errorf("URL userinfo is not allowed")
	}

	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("URL host is required")
	}

	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "metadata.google.internal" || host == "metadata.tencentyun.com" {
		return fmt.Errorf("URL host %q is not publicly routable", host)
	}

	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		if publicNetworkAddressRestricted(literal) {
			return fmt.Errorf("URL host %q resolves to a restricted address", host)
		}

		return nil
	}

	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("failed to resolve URL host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("URL host %q did not resolve to an address", host)
	}

	for _, resolved := range addresses {
		addr, ok := netip.AddrFromSlice(resolved.IP)
		if !ok || publicNetworkAddressRestricted(addr) {
			return fmt.Errorf("URL host %q resolves to a restricted address", host)
		}
	}

	return nil
}

var publicEndpointSchemes = map[string]struct{}{
	"http": {}, "https": {}, "ws": {}, "wss": {},
}

var publicProxySchemes = map[string]struct{}{
	"http": {}, "https": {},
}

func validatePublicURLWithResolver(ctx context.Context, rawURL string, resolver publicNetworkResolver) error {
	return validatePublicNetworkURLWithResolver(ctx, rawURL, resolver, publicEndpointSchemes, false)
}

// ValidatePublicURL verifies that an HTTP(S)/WS(S) URL resolves only to public
// network addresses.
func ValidatePublicURL(ctx context.Context, rawURL string) error {
	return validatePublicURLWithResolver(ctx, rawURL, net.DefaultResolver)
}

// ValidatePublicProxyURL verifies that an HTTP(S) proxy URL resolves only to
// public network addresses. Proxy userinfo is permitted for compatibility with
// standard authenticated proxy URLs.
func ValidatePublicProxyURL(ctx context.Context, rawURL string) error {
	return validatePublicNetworkURLWithResolver(ctx, rawURL, net.DefaultResolver, publicProxySchemes, true)
}

func publicNetworkDialContext(resolver publicNetworkResolver, dialer contextDialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", address, err)
		}

		host = strings.TrimSuffix(strings.ToLower(host), ".")
		var addresses []net.IPAddr
		if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
			addresses = []net.IPAddr{{IP: net.IP(literal.AsSlice())}}
		} else {
			addresses, err = resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve dial host %q: %w", host, err)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("dial host %q did not resolve to an address", host)
		}

		for _, resolved := range addresses {
			addr, ok := netip.AddrFromSlice(resolved.IP)
			if !ok || publicNetworkAddressRestricted(addr) {
				return nil, fmt.Errorf("dial host %q resolves to a restricted address", host)
			}
		}

		var dialErr error
		for _, resolved := range addresses {
			addr, _ := netip.AddrFromSlice(resolved.IP)
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}

		return nil, fmt.Errorf("failed to dial public host %q: %w", host, dialErr)
	}
}

func applyClientOptions(client *http.Client, transport *http.Transport, options clientOptions, proxyConfig *ProxyConfig) {
	if options.resolver == nil {
		options.resolver = net.DefaultResolver
	}

	if options.publicNetworkOnly {
		// Never inherit a server-side/environment proxy. Explicit URL proxies are
		// useful for provider connectivity and are safe here because both the
		// request target and the proxy dial are constrained to public addresses.
		if proxyConfig != nil && proxyConfig.Type == ProxyTypeURL {
			transport.Proxy = getProxyFunc(proxyConfig)
		} else {
			transport.Proxy = nil
		}
		transport.DialContext = publicNetworkDialContext(options.resolver, &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		})
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}

			if err := validatePublicURLWithResolver(req.Context(), req.URL.String(), options.resolver); err != nil {
				return fmt.Errorf("redirect target is not allowed: %w", err)
			}

			return nil
		}
	}

	if options.insecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}

		transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // User-configured option for self-signed certificates
	}
}

func normalizeClientOptions(options *clientOptions) {
	if options.resolver == nil {
		options.resolver = net.DefaultResolver
	}
}

// NewHttpClientWithProxy creates a new HTTP client with proxy configuration.
func NewHttpClientWithProxy(proxyConfig *ProxyConfig, opts ...ClientOption) *HttpClient {
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}
	normalizeClientOptions(&options)

	transport := &http.Transport{
		Proxy: getProxyFunc(proxyConfig),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Transport: transport}
	applyClientOptions(client, transport, options, proxyConfig)

	return &HttpClient{
		client:            client,
		proxyConfig:       proxyConfig,
		opts:              opts,
		publicNetworkOnly: options.publicNetworkOnly,
		resolver:          options.resolver,
	}
}

// WithProxy returns a new HttpClient that uses the given proxy configuration,
// while preserving all other options (e.g., InsecureSkipVerify) from the original client.
func (hc *HttpClient) WithProxy(proxyConfig *ProxyConfig) *HttpClient {
	return NewHttpClientWithProxy(proxyConfig, hc.opts...)
}

// WithPublicNetworkOnly returns a copy of this client restricted to direct,
// public-network destinations while preserving its TLS options.
func (hc *HttpClient) WithPublicNetworkOnly() *HttpClient {
	opts := append([]ClientOption{}, hc.opts...)
	opts = append(opts, WithPublicNetworkOnly())

	return NewHttpClientWithProxy(hc.proxyConfig, opts...)
}

// GetNativeClient returns the underlying *http.Client for advanced use cases.
func (hc *HttpClient) GetNativeClient() *http.Client {
	return hc.client
}

func (hc *HttpClient) ProxyFunc() func(*http.Request) (*url.URL, error) {
	if hc == nil {
		return http.ProxyFromEnvironment
	}

	if hc.publicNetworkOnly {
		if hc.proxyConfig != nil && hc.proxyConfig.Type == ProxyTypeURL {
			return getProxyFunc(hc.proxyConfig)
		}

		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}

	return getProxyFunc(hc.proxyConfig)
}

// getProxyFunc returns a proxy function based on the proxy configuration.
func getProxyFunc(config *ProxyConfig) func(*http.Request) (*url.URL, error) {
	// Handle nil config (backward compatibility) - default to environment
	if config == nil {
		return http.ProxyFromEnvironment
	}

	switch config.Type {
	case ProxyTypeDisabled:
		// No proxy - direct connection
		return func(*http.Request) (*url.URL, error) {
			return nil, nil
		}

	case ProxyTypeEnvironment:
		// Use environment variables (HTTP_PROXY, HTTPS_PROXY, NO_PROXY)
		return http.ProxyFromEnvironment

	case ProxyTypeURL:
		// Use configured URL with optional authentication
		if config.URL == "" {
			return func(*http.Request) (*url.URL, error) {
				return nil, errors.New("proxy URL is required when type is 'url'")
			}
		}

		proxyURL, err := url.Parse(config.URL)
		if err != nil {
			return func(_ *http.Request) (*url.URL, error) {
				return nil, fmt.Errorf("invalid proxy URL: %w", err)
			}
		}

		if config.Username != "" && config.Password != "" {
			proxyURL.User = url.UserPassword(config.Username, config.Password)
		}

		slog.DebugContext(context.Background(), "use custom proxy", slog.Any("proxy_url", proxyURL.Redacted()))

		return http.ProxyURL(proxyURL)

	default:
		// Unknown type - fall back to environment
		return http.ProxyFromEnvironment
	}
}

// NewHttpClient creates a new HTTP client.
func NewHttpClient(opts ...ClientOption) *HttpClient {
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}
	normalizeClientOptions(&options)

	client := &http.Client{}

	if options.insecureSkipVerify || options.publicNetworkOnly {
		var transport *http.Transport
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = defaultTransport.Clone()
		} else {
			// Fall back to a transport close to http.DefaultTransport when it has been replaced.
			transport = (&http.Transport{
				Proxy: getProxyFunc(nil),
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			})
		}

		applyClientOptions(client, transport, options, nil)
		client.Transport = transport
	}

	return &HttpClient{
		client:            client,
		opts:              opts,
		publicNetworkOnly: options.publicNetworkOnly,
		resolver:          options.resolver,
	}
}

// NewHttpClientWithClient creates a new HTTP client with a custom http.Client.
func NewHttpClientWithClient(client *http.Client) *HttpClient {
	return &HttpClient{
		client: client,
	}
}

// Do executes the HTTP request.
func (hc *HttpClient) Do(ctx context.Context, request *Request) (*Response, error) {
	slog.DebugContext(ctx, "execute http request", slog.Any("request", request), slog.Any("proxy", hc.proxyConfig))

	rawReq, err := hc.BuildHttpRequest(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("failed to build HTTP request: %w", err)
	}
	if hc.publicNetworkOnly {
		if err := validatePublicURLWithResolver(ctx, rawReq.URL.String(), hc.resolver); err != nil {
			return nil, fmt.Errorf("request URL is not allowed: %w", err)
		}
	}

	// Only set the default Accept when the transformer did not specify one
	// (e.g. TTS sets Accept: */* to receive binary audio).
	if rawReq.Header.Get("Accept") == "" {
		rawReq.Header.Set("Accept", "application/json")
	}

	rawResp, err := hc.client.Do(rawReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}

	defer func() {
		err := rawResp.Body.Close()
		if err != nil {
			slog.WarnContext(ctx, "failed to close HTTP response body", slog.Any("error", err))
		}
	}()

	var body []byte
	// Cap error response bodies at 1 MB to prevent OOM from pathological
	// upstream error bodies (e.g., vLLM echoing multi-MB input in validation
	// errors). Successful responses are read in full because they are
	// typically small JSON payloads.
	if rawResp.StatusCode >= 400 {
		body, err = io.ReadAll(io.LimitReader(rawResp.Body, MaxErrorBodySize))
		if err != nil {
			return nil, fmt.Errorf("failed to read error response body: %w", err)
		}

		if slog.Default().Enabled(ctx, slog.LevelDebug) {
			slog.DebugContext(ctx, "HTTP request failed",
				slog.String("method", rawReq.Method),
				slog.String("url", rawReq.URL.String()),
				slog.Int("status_code", rawResp.StatusCode),
				slog.String("body", string(body)))
		}

		return nil, &Error{
			Method:     rawReq.Method,
			URL:        rawReq.URL.String(),
			StatusCode: rawResp.StatusCode,
			Status:     rawResp.Status,
			Body:       body,
			Headers:    rawResp.Header,
		}
	}

	body, err = io.ReadAll(rawResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.DebugContext(ctx, "HTTP request success",
			slog.String("method", rawReq.Method),
			slog.String("url", rawReq.URL.String()),
			slog.Int("status_code", rawResp.StatusCode),
			slog.String("body", string(body)))
	}

	// Build generic response
	response := &Response{
		StatusCode:  rawResp.StatusCode,
		Headers:     rawResp.Header,
		Body:        body,
		RawResponse: rawResp,
		Stream:      nil,
		Request:     request,
		RawRequest:  rawReq,
	}

	return response, nil
}

// DoStream executes a streaming HTTP request using Server-Sent Events.
func (hc *HttpClient) DoStream(ctx context.Context, request *Request) (streams.Stream[*StreamEvent], error) {
	slog.DebugContext(ctx, "execute stream request", slog.Any("request", request))

	rawReq, err := hc.BuildHttpRequest(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("failed to build HTTP request: %w", err)
	}
	if hc.publicNetworkOnly {
		if err := validatePublicURLWithResolver(ctx, rawReq.URL.String(), hc.resolver); err != nil {
			return nil, fmt.Errorf("request URL is not allowed: %w", err)
		}
	}

	// Add streaming headers. Force SSE Accept unless the outbound transformer
	// explicitly opted into a non-JSON Accept (e.g. "*/*" for binary TTS chunks),
	// so chat-style outbounds whose default Accept is application/json still
	// negotiate SSE for streaming requests.
	accept := rawReq.Header.Get("Accept")
	if accept == "" || strings.EqualFold(accept, "application/json") {
		rawReq.Header.Set("Accept", "text/event-stream")
	}
	rawReq.Header.Set("Cache-Control", "no-cache")
	rawReq.Header.Set("Connection", "keep-alive")

	// Execute request
	rawResp, err := hc.client.Do(rawReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP stream request failed: %w", err)
	}

	// Check for HTTP errors before creating stream
	if rawResp.StatusCode >= 400 {
		defer func() {
			err := rawResp.Body.Close()
			if err != nil {
				slog.WarnContext(ctx, "failed to close HTTP response body", slog.Any("error", err))
			}
		}()

		// Read error body for streaming requests
		body, err := io.ReadAll(io.LimitReader(rawResp.Body, MaxErrorBodySize))
		if err != nil {
			return nil, err
		}

		if slog.Default().Enabled(ctx, slog.LevelDebug) {
			slog.DebugContext(ctx, "HTTP stream request failed",
				slog.String("method", rawReq.Method),
				slog.String("url", rawReq.URL.String()),
				slog.Int("status_code", rawResp.StatusCode),
				slog.String("body", string(body)))
		}

		return nil, &Error{
			Method:     rawReq.Method,
			URL:        rawReq.URL.String(),
			StatusCode: rawResp.StatusCode,
			Status:     rawResp.Status,
			Body:       body,
			Headers:    rawResp.Header,
		}
	}

	// Determine content type and select appropriate decoder
	contentType := rawResp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/event-stream" // Default to SSE
	}

	// Try to get a registered decoder for the content type
	decoderFactory, exists := GetDecoder(contentType)
	if !exists {
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
			decoderFactory, exists = GetDecoder(mediaType)
		}
	}
	if !exists {
		// Fallback to default SSE decoder
		slog.DebugContext(ctx, "no decoder found for content type, using default SSE", slog.String("content_type", contentType))

		decoderFactory = NewDefaultSSEDecoder
	}

	stream := decoderFactory(ctx, rawResp.Body)

	return stream, nil
}

// BuildHttpRequest builds an HTTP request from Request.
func BuildHttpRequest(
	ctx context.Context,
	request *Request,
) (*http.Request, error) {
	var body io.Reader
	if len(request.Body) > 0 {
		body = bytes.NewReader(request.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, request.Method, request.URL, body)
	if err != nil {
		return nil, err
	}

	httpReq.Header = request.Headers
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}
	// Handle User-Agent header - only set default if not already present
	if httpReq.Header.Get("User-Agent") == "" {
		// No User-Agent set, use default
		httpReq.Header.Set("User-Agent", "axonhub/1.0")
	}

	for k := range libManagedHeaders {
		httpReq.Header.Del(k)
	}

	// Set Content-Type header if specified in request
	if request.ContentType != "" {
		httpReq.Header.Set("Content-Type", request.ContentType)
	}

	if request.Auth != nil {
		err = applyAuth(httpReq.Header, request.Auth)
		if err != nil {
			return nil, fmt.Errorf("failed to apply authentication: %w", err)
		}
	}

	if len(request.Query) > 0 {
		if httpReq.URL.RawQuery != "" {
			httpReq.URL.RawQuery += "&"
		}

		httpReq.URL.RawQuery += request.Query.Encode()
	}

	return httpReq, nil
}

// BuildHttpRequest builds an HTTP request from Request.
func (hc *HttpClient) BuildHttpRequest(
	ctx context.Context,
	request *Request,
) (*http.Request, error) {
	return BuildHttpRequest(ctx, request)
}

// applyAuth applies authentication to the HTTP request.
func applyAuth(headers http.Header, auth *AuthConfig) error {
	switch auth.Type {
	case "bearer":
		if auth.APIKey == "" {
			return fmt.Errorf("bearer token is required")
		}

		headers.Set("Authorization", "Bearer "+auth.APIKey)
	case "api_key":
		if auth.HeaderKey == "" {
			return fmt.Errorf("header key is required")
		}

		headers.Set(auth.HeaderKey, auth.APIKey)
	default:
		return fmt.Errorf("unsupported auth type: %s", auth.Type)
	}

	return nil
}

// extractHeaders extracts headers from HTTP response.
func (hc *HttpClient) extractHeaders(headers http.Header) map[string]string {
	result := make(map[string]string)

	for key, values := range headers {
		if len(values) > 0 {
			result[key] = values[0] // Take the first value
		}
	}

	return result
}
