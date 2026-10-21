package integrations

import (
	"regexp"
	"strings"

	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/providers/generic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib/passthroughspec"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// genericPassthroughPrefix is the route every generic custom provider is reached under:
// /passthrough/{provider}/{path:*}. The provider segment is resolved per request because custom
// providers are added and removed at runtime while routes are registered once.
const genericPassthroughPrefix = "/passthrough"

// genericProviderNameRe bounds the provider segment before any lookup happens.
var genericProviderNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// GenericPassthroughRouter relays requests to custom providers whose base type is generic. Only
// such providers are served: a standard provider, a custom provider of another base type, or an
// unknown name gets a 404, never a forwarded request.
type GenericPassthroughRouter struct {
	*GenericRouter
	specs *passthroughspec.Manager // compiled OpenAPI documents, one per provider that has one
}

// NewGenericPassthroughRouter creates the /passthrough/{provider} router.
func NewGenericPassthroughRouter(client *bifrost.Bifrost, handlerStore lib.HandlerStore, accessResolver AccessResolver, logger schemas.Logger) *GenericPassthroughRouter {
	var configured func(schemas.ModelProvider) bool
	if handlerStore != nil {
		configured = handlerStore.IsProviderConfigured
	}
	return &GenericPassthroughRouter{
		GenericRouter: NewGenericRouter(client, handlerStore, accessResolver, nil, nil, logger),
		specs:         passthroughspec.NewManager(configured),
	}
}

// checkSpec applies the provider's OpenAPI document to a request: an operation the document does
// not list is refused when enforce_operations is on, and a JSON body that fails the operation's
// schema is refused when validate_request_body is on. A document that does not compile is a 500,
// never a silent pass. Providers without a document pay one nil check.
func (g *GenericPassthroughRouter) checkSpec(provider schemas.ModelProvider, cfg *schemas.GenericProviderConfig, method, path, contentType string, body []byte) *schemas.BifrostError {
	if cfg == nil || cfg.OpenAPI == nil {
		return nil
	}
	spec, err := g.specs.Lookup(provider, cfg.OpenAPI)
	if err != nil {
		return newBifrostErrorWithCode(err, "openapi spec for provider "+string(provider)+" failed to compile: "+err.Error(), fasthttp.StatusInternalServerError)
	}
	if spec == nil {
		return nil
	}
	op, ok := spec.Match(method, path)
	if !ok {
		if cfg.OpenAPI.EnforceOperations {
			return newBifrostErrorWithCode(nil, "operation "+method+" "+path+" is not in the provider's OpenAPI spec", fasthttp.StatusNotFound)
		}
		return nil
	}
	if cfg.OpenAPI.ValidateRequestBody {
		if err := spec.ValidateBody(op, contentType, body); err != nil {
			return newBifrostErrorWithCode(err, err.Error(), fasthttp.StatusBadRequest)
		}
	}
	return nil
}

// RegisterRoutes registers the catch-all for every method a relay may carry.
func (g *GenericPassthroughRouter) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	handler := lib.ChainMiddlewares(g.handleGenericPassthrough, middlewares...)
	for _, method := range []string{fasthttp.MethodGet, fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodDelete, fasthttp.MethodPatch, fasthttp.MethodHead} {
		r.Handle(method, genericPassthroughPrefix+"/{provider}/{path:*}", handler)
	}
}

// resolveGenericProvider maps a provider segment to a running generic provider. The known-provider
// map read and the provider slice scan are the only per-request work; nothing is allocated.
func resolveGenericProvider(client *bifrost.Bifrost, name string) (schemas.ModelProvider, generic.PassthroughProvider, bool) {
	if !genericProviderNameRe.MatchString(name) || !schemas.IsKnownProvider(name) {
		return "", nil, false
	}
	provider := schemas.ModelProvider(name)
	gp, ok := client.GetProviderByKey(provider).(generic.PassthroughProvider)
	if !ok || gp == nil {
		return "", nil, false
	}
	return provider, gp, true
}

// extractGenericModel reads the model a JSON body names at modelPath (default "model"). It is
// lenient on purpose: a non-LLM API has no model, and a body that is not JSON names none.
func extractGenericModel(body []byte, contentType, modelPath string) string {
	if len(body) == 0 || !looksLikeJSON(contentType, body) {
		return ""
	}
	if modelPath == "" {
		modelPath = "model"
	}
	if v := gjson.GetBytes(body, modelPath); v.Type == gjson.String {
		return strings.TrimSpace(v.Str)
	}
	return ""
}

// genericBodyStreams reports a JSON body asking for a stream with "stream": true.
func genericBodyStreams(body []byte, contentType string) bool {
	return len(body) > 0 && looksLikeJSON(contentType, body) && gjson.GetBytes(body, "stream").Type == gjson.True
}

func looksLikeJSON(contentType string, body []byte) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "" {
		return isJSONMediaType(mediaType)
	}
	return gjson.ValidBytes(body)
}

func (g *GenericPassthroughRouter) handleGenericPassthrough(ctx *fasthttp.RequestCtx) {
	passthroughErr := func(_ *schemas.BifrostContext, err *schemas.BifrostError) interface{} {
		return err
	}

	name, _ := ctx.UserValue("provider").(string)
	provider, gp, ok := resolveGenericProvider(g.client, name)
	if !ok {
		g.sendError(ctx, nil, passthroughErr, newBifrostErrorWithCode(nil, "no generic passthrough provider named "+name, fasthttp.StatusNotFound))
		return
	}

	// The remainder is concatenated onto the provider's base URL, so it is checked for shape before
	// anything else happens; a rejected request never reaches key selection or the upstream.
	rest, _ := ctx.UserValue("path").(string)
	path := "/" + rest
	if err := validatePassthroughPath(path); err != nil {
		g.sendError(ctx, nil, passthroughErr, newBifrostErrorWithCode(err, err.Error(), fasthttp.StatusBadRequest))
		return
	}

	// x-model-provider, when sent, must name the provider the route resolved to.
	if _, err := getPassthroughProvider(ctx, provider); err != nil {
		g.sendError(ctx, nil, passthroughErr, newBifrostErrorWithCode(err, err.Error(), fasthttp.StatusBadRequest))
		return
	}

	safeHeaders, _ := collectPassthroughHeaders(ctx)
	body := ctx.Request.Body()
	contentType := string(ctx.Request.Header.ContentType())
	cfg := gp.GenericConfig()
	modelPath := ""
	if cfg != nil && cfg.Usage != nil {
		modelPath = cfg.Usage.ModelPath
	}
	model := extractGenericModel(body, contentType, modelPath)
	method := string(ctx.Method())
	if bifrostErr := g.checkSpec(provider, cfg, method, path, contentType, body); bifrostErr != nil {
		g.sendError(ctx, nil, passthroughErr, bifrostErr)
		return
	}

	bifrostCtx, cancel := lib.ConvertToBifrostContext(ctx, g.handlerStore)
	passthroughReq := &schemas.BifrostPassthroughRequest{
		Method:      method,
		Path:        path,
		RawQuery:    string(ctx.URI().QueryString()),
		Body:        body,
		SafeHeaders: safeHeaders,
		Provider:    provider,
		Model:       model,
	}

	accept := string(ctx.Request.Header.Peek("Accept"))
	if gp.Streams(method, path, accept, genericBodyStreams(body, contentType) || passthroughQueryStreams(method, passthroughReq.RawQuery)) {
		g.handlePassthroughStream(ctx, bifrostCtx, cancel, provider, passthroughReq)
	} else {
		g.handlePassthroughNonStream(ctx, bifrostCtx, cancel, provider, passthroughReq)
	}
}
