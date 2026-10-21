package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/openapimcp"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib/passthroughspec"
	"github.com/valyala/fasthttp"
)

// GenericOpenAPIPreviewRequest is the body of POST /api/providers/openapi/preview.
type GenericOpenAPIPreviewRequest struct {
	Spec    string `json:"spec,omitempty"`     // Inline document (JSON or YAML)
	SpecURL string `json:"spec_url,omitempty"` // Fetched when spec is empty
}

// GenericOpenAPIPreviewResponse summarizes a parsed document for the provider form. Spec carries
// the fetched document when the request named a spec_url, so the client can store it inline.
type GenericOpenAPIPreviewResponse struct {
	Title          string                             `json:"title"`
	Version        string                             `json:"version"`
	OpenAPIVersion string                             `json:"openapi_version"`
	OperationCount int                                `json:"operation_count"`
	SpecSize       int                                `json:"spec_size"`
	SpecHash       string                             `json:"spec_hash"`
	Operations     []passthroughspec.OperationSummary `json:"operations"`
	Spec           string                             `json:"spec,omitempty"`
	SpecURL        string                             `json:"spec_url,omitempty"`
}

// previewGenericOpenAPISpec handles POST /api/providers/openapi/preview: parse only, create nothing.
func (h *ProviderHandler) previewGenericOpenAPISpec(ctx *fasthttp.RequestCtx) {
	var req GenericOpenAPIPreviewRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid request payload")
		return
	}
	cfg := &schemas.GenericOpenAPIConfig{Spec: req.Spec}
	if u := strings.TrimSpace(req.SpecURL); u != "" {
		cfg.SpecURL = &u
	}
	if status, msg := prepareGenericOpenAPIConfig(ctx, cfg); status != 0 {
		SendError(ctx, status, msg)
		return
	}
	spec, err := passthroughspec.Compile(cfg)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid spec: %v", err))
		return
	}
	resp := GenericOpenAPIPreviewResponse{
		Title: spec.Title, Version: spec.Version, OpenAPIVersion: cfg.OpenAPIVersion,
		OperationCount: cfg.OperationCount, SpecSize: cfg.SpecSize, SpecHash: cfg.SpecHash,
		Operations: spec.Operations(),
	}
	if req.SpecURL != "" {
		resp.Spec = cfg.Spec
		if cfg.SpecURL != nil {
			resp.SpecURL = *cfg.SpecURL
		}
	}
	SendJSON(ctx, resp)
}

// getGenericOpenAPISpec handles GET /api/providers/{provider}/openapi-spec: the stored document,
// which the list/get endpoints leave out.
func (h *ProviderHandler) getGenericOpenAPISpec(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}
	config, err := h.inMemoryStore.GetProviderConfigRaw(provider)
	if err != nil {
		if errors.Is(err, lib.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
		return
	}
	cpc := config.CustomProviderConfig
	if cpc == nil || cpc.GenericConfig == nil || cpc.GenericConfig.OpenAPI == nil || cpc.GenericConfig.OpenAPI.Spec == "" {
		SendError(ctx, fasthttp.StatusNotFound, "Provider has no OpenAPI document")
		return
	}
	oc := cpc.GenericConfig.OpenAPI
	SendJSON(ctx, map[string]any{
		"spec":            oc.Spec,
		"spec_url":        oc.SpecURL,
		"spec_size":       oc.SpecSize,
		"spec_hash":       oc.SpecHash,
		"spec_title":      oc.SpecTitle,
		"openapi_version": oc.OpenAPIVersion,
		"operation_count": oc.OperationCount,
	})
}

// prepareGenericOpenAPIConfig validates an openapi block arriving over the API: a spec_file is
// refused (config.json only), a spec_url document is fetched under the same public-target rule
// the MCP OpenAPI clients use and stored inline, the document is compiled so a broken one fails
// here rather than at request time, and the server-computed metadata is filled. status is 0 on
// success. It writes into cfg, so callers pass a block they own.
func prepareGenericOpenAPIConfig(ctx *fasthttp.RequestCtx, cfg *schemas.GenericOpenAPIConfig) (int, string) {
	if cfg == nil {
		return 0, ""
	}
	if cfg.SpecFile != nil && strings.TrimSpace(*cfg.SpecFile) != "" {
		return fasthttp.StatusBadRequest, "generic_config.openapi.spec_file is only supported in config.json; send the document as spec or spec_url"
	}
	specURL := ""
	if cfg.SpecURL != nil {
		specURL = strings.TrimSpace(*cfg.SpecURL)
	}
	if strings.TrimSpace(cfg.Spec) != "" {
		if _, err := openapimcp.LoadBytes([]byte(cfg.Spec)); err != nil {
			return openAPISpecErrorStatus(err), fmt.Sprintf("Invalid generic_config.openapi.spec: %v", err)
		}
	} else if specURL != "" {
		data, finalURL, status, err := fetchOpenAPISpecForCaller(ctx, specURL)
		if err != nil {
			return status, err.Error()
		}
		cfg.Spec = string(data)
		cfg.SpecURL = &finalURL
	} else {
		return fasthttp.StatusBadRequest, "generic_config.openapi requires spec (inline document) or spec_url"
	}
	spec, err := passthroughspec.Compile(cfg)
	if err != nil {
		return openAPISpecErrorStatus(err), fmt.Sprintf("Invalid generic_config.openapi.spec: %v", err)
	}
	passthroughspec.Metadata(cfg, spec)
	return 0, ""
}

// mergeGenericOpenAPIUpdate keeps the stored document when an update echoes the provider back
// without it: the list/get endpoints blank the spec, so a UI save would otherwise drop it. A new
// spec, or a changed spec_url, replaces the stored one.
func mergeGenericOpenAPIUpdate(incoming, stored *schemas.CustomProviderConfig) {
	if incoming == nil || incoming.GenericConfig == nil || incoming.GenericConfig.OpenAPI == nil {
		return
	}
	in := incoming.GenericConfig.OpenAPI
	if strings.TrimSpace(in.Spec) != "" || stored == nil || stored.GenericConfig == nil || stored.GenericConfig.OpenAPI == nil || stored.GenericConfig.OpenAPI.Spec == "" {
		return
	}
	old := stored.GenericConfig.OpenAPI
	sameURL := (in.SpecURL == nil && old.SpecURL == nil) || (in.SpecURL != nil && old.SpecURL != nil && strings.TrimSpace(*in.SpecURL) == strings.TrimSpace(*old.SpecURL))
	if !sameURL {
		return
	}
	in.Spec = old.Spec
	in.SpecFile = old.SpecFile
	in.SpecSize, in.SpecHash, in.SpecTitle, in.OpenAPIVersion, in.OperationCount = old.SpecSize, old.SpecHash, old.SpecTitle, old.OpenAPIVersion, old.OperationCount
}

// genericOpenAPIBlock returns the openapi block of a generic custom provider config, or nil.
func genericOpenAPIBlock(cpc *schemas.CustomProviderConfig) *schemas.GenericOpenAPIConfig {
	if cpc == nil || cpc.BaseProviderType != schemas.Generic || cpc.GenericConfig == nil {
		return nil
	}
	return cpc.GenericConfig.OpenAPI
}
