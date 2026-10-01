package compiler

import (
	"context"

	"github.com/d7z-team/mini-go/compiler/cache"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type sessionCache struct {
	shared cache.Cache
	local  *cache.TransientCache
}

func (c *sessionCache) Close() {
	if c == nil || c.local == nil {
		return
	}
	c.local.Close()
	c.local = nil
	c.shared = nil
}

func newSessionCache(shared cache.Cache, config cache.TransientConfig) *sessionCache {
	return &sessionCache{shared: shared, local: cache.NewTransient(config)}
}

func (c *sessionCache) LookupPrepare(action cache.PrepareAction) (cache.PrepareLookup, error) {
	if value, err := c.local.LookupPrepare(action); err != nil || value.Hit {
		return value, err
	}
	if c.shared == nil {
		return cache.PrepareLookup{Reason: "session action missing"}, nil
	}
	value, err := c.shared.LookupPrepare(action)
	if err == nil && value.Hit {
		err = c.local.StorePrepare(action, cache.PreparedOutput{Image: value.Image, TestManifest: value.TestManifest})
	}
	return value, err
}

func (c *sessionCache) StorePrepare(action cache.PrepareAction, output cache.PreparedOutput) error {
	if err := c.local.StorePrepare(action, output); err != nil {
		return err
	}
	if c.shared != nil {
		return c.shared.StorePrepare(action, output)
	}
	return nil
}

func (c *sessionCache) LookupCompile(action cache.Action) (cache.Lookup, error) {
	if value, err := c.local.LookupCompile(action); err != nil || value.Hit {
		return value, err
	}
	if c.shared == nil {
		return cache.Lookup{Reason: "session action missing"}, nil
	}
	value, err := c.shared.LookupCompile(action)
	if err == nil && value.Hit {
		var sealed cache.CompiledArtifact
		sealed, err = cache.SealArtifact(value.Artifact)
		if err == nil {
			_, err = c.local.StoreCompile(action, sealed, value.Symbols, value.ExportData)
		}
	}
	return value, err
}

func (c *sessionCache) LookupCompileManifest(action cache.Action) (cache.ManifestLookup, error) {
	if value, err := c.local.LookupCompileManifest(action); err != nil || value.Hit {
		return value, err
	}
	if c.shared == nil {
		return cache.ManifestLookup{Reason: "session action missing"}, nil
	}
	return c.shared.LookupCompileManifest(action)
}

func (c *sessionCache) StoreCompile(action cache.Action, artifact cache.CompiledArtifact, symbols ir.PackageSymbols, data cache.PackageData) (cache.Manifest, error) {
	manifest, err := c.local.StoreCompile(action, artifact, symbols, data)
	if err != nil {
		return cache.Manifest{}, err
	}
	if c.shared != nil {
		return c.shared.StoreCompile(action, artifact, symbols, data)
	}
	return manifest, nil
}

func (c *sessionCache) LookupSymbols(action cache.SymbolAction) (cache.SymbolLookup, error) {
	if value, err := c.local.LookupSymbols(action); err != nil || value.Hit {
		return value, err
	}
	if c.shared == nil {
		return cache.SymbolLookup{Reason: "session symbols missing"}, nil
	}
	value, err := c.shared.LookupSymbols(action)
	if err == nil && value.Hit {
		err = c.local.StoreSymbols(action, value.Symbols)
	}
	return value, err
}

func (c *sessionCache) StoreSymbols(action cache.SymbolAction, symbols ir.ProgramSymbols) error {
	if err := c.local.StoreSymbols(action, symbols); err != nil {
		return err
	}
	if c.shared != nil {
		return c.shared.StoreSymbols(action, symbols)
	}
	return nil
}

func (c *sessionCache) LockCompile(ctx context.Context, action cache.Action) (func(), error) {
	localRelease, err := c.local.LockCompile(ctx, action)
	if err != nil {
		return nil, err
	}
	sharedRelease := func() {}
	if c.shared != nil {
		sharedRelease, err = c.shared.LockCompile(ctx, action)
		if err != nil {
			localRelease()
			return nil, err
		}
	}
	return func() {
		sharedRelease()
		localRelease()
	}, nil
}

func (c *sessionCache) LockPrepare(ctx context.Context, action cache.PrepareAction) (func(), error) {
	localRelease, err := c.local.LockPrepare(ctx, action)
	if err != nil {
		return nil, err
	}
	sharedRelease := func() {}
	if c.shared != nil {
		sharedRelease, err = c.shared.LockPrepare(ctx, action)
		if err != nil {
			localRelease()
			return nil, err
		}
	}
	return func() {
		sharedRelease()
		localRelease()
	}, nil
}

func (c *sessionCache) LockSymbols(ctx context.Context, action cache.SymbolAction) (func(), error) {
	localRelease, err := c.local.LockSymbols(ctx, action)
	if err != nil {
		return nil, err
	}
	sharedRelease := func() {}
	if c.shared != nil {
		sharedRelease, err = c.shared.LockSymbols(ctx, action)
		if err != nil {
			localRelease()
			return nil, err
		}
	}
	return func() {
		sharedRelease()
		localRelease()
	}, nil
}
