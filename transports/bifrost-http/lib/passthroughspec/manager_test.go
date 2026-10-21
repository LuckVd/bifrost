package passthroughspec

import (
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tinySpec = `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/ping":{"get":{"responses":{"200":{"description":"ok"}}}}}}`

func TestManagerLookup(t *testing.T) {
	live := map[schemas.ModelProvider]bool{"a": true, "b": true}
	m := NewManager(func(p schemas.ModelProvider) bool { return live[p] })

	spec, err := m.Lookup("a", nil)
	require.NoError(t, err)
	assert.Nil(t, spec, "no document means nothing to enforce")
	assert.Equal(t, 0, m.Len())

	cfgA := &schemas.GenericOpenAPIConfig{Spec: tinySpec}
	first, err := m.Lookup("a", cfgA)
	require.NoError(t, err)
	require.NotNil(t, first)
	again, err := m.Lookup("a", cfgA)
	require.NoError(t, err)
	assert.Same(t, first, again, "the same config pointer reuses the compiled spec")

	rebuilt := &schemas.GenericOpenAPIConfig{Spec: tinySpec}
	third, err := m.Lookup("a", rebuilt)
	require.NoError(t, err)
	assert.NotSame(t, first, third, "a new config pointer (provider rebuild) recompiles")

	_, err = m.Lookup("b", &schemas.GenericOpenAPIConfig{Spec: "nope"})
	require.Error(t, err, "a broken document reports its error on every lookup")
	assert.Equal(t, 2, m.Len())

	delete(live, "b")
	_, _ = m.Lookup("a", &schemas.GenericOpenAPIConfig{Spec: tinySpec})
	assert.Equal(t, 1, m.Len(), "a provider that no longer exists is swept on the next compile")
}

func TestManagerConcurrentLookups(t *testing.T) {
	m := NewManager(nil)
	cfg := &schemas.GenericOpenAPIConfig{Spec: tinySpec}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec, err := m.Lookup("p", cfg)
			assert.NoError(t, err)
			assert.NotNil(t, spec)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, m.Len())
}

func BenchmarkManagerLookupHit(b *testing.B) {
	m := NewManager(nil)
	cfg := &schemas.GenericOpenAPIConfig{Spec: tinySpec}
	if _, err := m.Lookup("p", cfg); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Lookup("p", cfg); err != nil {
			b.Fatal(err)
		}
	}
}
