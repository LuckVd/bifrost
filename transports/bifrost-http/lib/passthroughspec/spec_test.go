package passthroughspec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "framework", "openapimcp", "testdata", name))
	require.NoError(t, err)
	return string(data)
}

func compileFixture(t *testing.T, name string, validateBody bool) *Spec {
	t.Helper()
	spec, err := Compile(&schemas.GenericOpenAPIConfig{Spec: fixture(t, name), ValidateRequestBody: validateBody})
	require.NoError(t, err)
	return spec
}

func TestCompileAcrossVersions(t *testing.T) {
	for _, name := range []string{"petstore-3.0.yaml", "petstore-3.1.json", "petstore-3.2.yaml", "petstore-swagger2.json"} {
		t.Run(name, func(t *testing.T) {
			spec := compileFixture(t, name, true)
			assert.Contains(t, spec.Title, "Petstore")
			assert.NotEmpty(t, spec.OpenAPIVersion)
			assert.NotEmpty(t, spec.Operations())
			_, ok := spec.Match("GET", "/pets")
			assert.True(t, ok, "GET /pets is listed in every fixture")
		})
	}

	_, err := Compile(&schemas.GenericOpenAPIConfig{Spec: "not: a spec"})
	require.Error(t, err)
	_, err = Compile(&schemas.GenericOpenAPIConfig{Spec: ""})
	require.Error(t, err)
	_, err = Compile(nil)
	require.Error(t, err)
}

func TestMatch(t *testing.T) {
	spec := compileFixture(t, "petstore-3.0.yaml", false)

	op, ok := spec.Match("get", "/pets/42")
	require.True(t, ok)
	assert.Equal(t, "getPetById", op.ID)

	op, ok = spec.Match("POST", "/pets/42/photo/")
	require.True(t, ok, "a trailing slash is ignored")
	assert.Equal(t, "uploadPetPhoto", op.ID)

	_, ok = spec.Match("GET", "/pets/")
	require.True(t, ok, "a trailing slash is ignored")

	_, ok = spec.Match("POST", "/pets//photo")
	require.False(t, ok, "an empty parameter segment matches nothing")

	_, ok = spec.Match("PATCH", "/pets/42")
	require.False(t, ok, "method must match")

	_, ok = spec.Match("GET", "/pets/42/photo/extra")
	require.False(t, ok, "segment count must match")

	_, ok = spec.Match("GET", "/nope")
	require.False(t, ok)

	var nilSpec *Spec
	_, ok = nilSpec.Match("GET", "/pets")
	assert.False(t, ok)
}

// A literal path wins over a templated one with the same shape.
func TestMatchLiteralBeforeParameter(t *testing.T) {
	doc := `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{
		"/users/{id}":{"get":{"operationId":"getUser","responses":{"200":{"description":"ok"}}}},
		"/users/me":{"get":{"operationId":"getMe","responses":{"200":{"description":"ok"}}}}}}`
	spec, err := Compile(&schemas.GenericOpenAPIConfig{Spec: doc})
	require.NoError(t, err)
	op, ok := spec.Match("GET", "/users/me")
	require.True(t, ok)
	assert.Equal(t, "getMe", op.ID)
	op, ok = spec.Match("GET", "/users/7")
	require.True(t, ok)
	assert.Equal(t, "getUser", op.ID)
}

func TestValidateBody(t *testing.T) {
	spec := compileFixture(t, "petstore-3.0.yaml", true)
	create, ok := spec.Match("POST", "/pets")
	require.True(t, ok)

	assert.NoError(t, spec.ValidateBody(create, "application/json", []byte(`{"name":"rex"}`)))
	assert.NoError(t, spec.ValidateBody(create, "application/json; charset=utf-8", []byte(`{"name":"rex","tag":null}`)))

	err := spec.ValidateBody(create, "application/json", []byte(`{"name":5}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the operation schema")

	err = spec.ValidateBody(create, "application/json", []byte(`{}`))
	require.Error(t, err, "name is required")

	err = spec.ValidateBody(create, "application/json", []byte(`{"name":`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")

	err = spec.ValidateBody(create, "application/json", nil)
	require.Error(t, err, "the body is required")

	assert.NoError(t, spec.ValidateBody(create, "text/plain", []byte(`not json`)), "other media types are not checked")

	list, ok := spec.Match("GET", "/pets")
	require.True(t, ok)
	assert.NoError(t, spec.ValidateBody(list, "application/json", []byte(`{"anything":true}`)), "an operation without a body schema checks nothing")

	noBodies := compileFixture(t, "petstore-3.0.yaml", false)
	create, _ = noBodies.Match("POST", "/pets")
	assert.NoError(t, noBodies.ValidateBody(create, "application/json", []byte(`{"name":5}`)), "schemas are not compiled when validation is off")
	assert.Error(t, noBodies.ValidateBody(create, "application/json", nil), "a required body is still required")
}

func TestOperationsAndMetadata(t *testing.T) {
	cfg := &schemas.GenericOpenAPIConfig{Spec: fixture(t, "petstore-3.0.yaml")}
	spec, err := Compile(cfg)
	require.NoError(t, err)
	ops := spec.Operations()
	require.NotEmpty(t, ops)
	var sawCreate bool
	for _, op := range ops {
		if op.OperationID == "createPet" {
			sawCreate = true
			assert.Equal(t, "POST", op.Method)
			assert.Equal(t, "/pets", op.Path)
			assert.True(t, op.HasBody)
		}
	}
	assert.True(t, sawCreate)

	Metadata(cfg, spec)
	assert.Equal(t, len(cfg.Spec), cfg.SpecSize)
	assert.Len(t, cfg.SpecHash, 64)
	assert.Equal(t, "Petstore", cfg.SpecTitle)
	assert.Equal(t, "3.0.3", cfg.OpenAPIVersion)
	assert.Equal(t, len(ops), cfg.OperationCount)
}
