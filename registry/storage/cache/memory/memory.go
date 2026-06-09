package memory

import (
	"context"
	"math"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	prometheus "github.com/distribution/distribution/v3/metrics"
	"github.com/distribution/distribution/v3/registry/storage/cache"
	"github.com/distribution/reference"
	"github.com/hashicorp/golang-lru/arc/v2"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// DefaultSize is the default cache size to use if no size is explicitly
	// configured.
	DefaultSize = 10000

	// UnlimitedSize indicates the cache size should not be limited.
	UnlimitedSize = math.MaxInt

	// DefaultTTL is used when no TTL is explicitly configured. It is set to a
	// very large value to preserve the original behaviour of no expiry.
	DefaultTTL = 365 * 24 * time.Hour
)

var (
	expiredCacheCount = prometheus.StorageNamespace.NewCounter("expired_cache_count", "The number of cache entries that have expired")
)

type descriptorCacheKey struct {
	digest digest.Digest
	repo   string
}

type descriptionCacheValue struct {
	descriptor v1.Descriptor
	addedAt    time.Time
}

type inMemoryBlobDescriptorCacheProvider struct {
	lru *arc.ARCCache[descriptorCacheKey, descriptionCacheValue]
	ttl *time.Duration
}

// NewInMemoryBlobDescriptorCacheProvider returns a new mapped-based cache for
// storing blob descriptor data.
func NewInMemoryBlobDescriptorCacheProvider(size int, opts ...Option) cache.BlobDescriptorCacheProvider {
	if size <= 0 {
		size = math.MaxInt
	}
	lruCache, err := arc.NewARC[descriptorCacheKey, descriptionCacheValue](size)
	if err != nil {
		// NewARC can only fail if size is <= 0, so this unreachable
		panic(err)
	}
	c := &inMemoryBlobDescriptorCacheProvider{
		lru: lruCache,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type Option func(*inMemoryBlobDescriptorCacheProvider)

func WithTTL(ttl time.Duration) Option {
	// A TTL of 0 disables the cache (all entries expire immediately).
	return func(imbdcp *inMemoryBlobDescriptorCacheProvider) {
		imbdcp.ttl = &ttl
	}
}

func (imbdcp *inMemoryBlobDescriptorCacheProvider) RepositoryScoped(repo string) (distribution.BlobDescriptorService, error) {
	if _, err := reference.ParseNormalizedNamed(repo); err != nil {
		if err == reference.ErrNameTooLong {
			return nil, distribution.ErrRepositoryNameInvalid{
				Name:   repo,
				Reason: reference.ErrNameTooLong,
			}
		}
		return nil, err
	}

	return &repositoryScopedInMemoryBlobDescriptorCache{
		repo:   repo,
		parent: imbdcp,
	}, nil
}

func (imbdcp *inMemoryBlobDescriptorCacheProvider) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	if err := dgst.Validate(); err != nil {
		return v1.Descriptor{}, err
	}

	key := descriptorCacheKey{
		digest: dgst,
	}
	descriptor, ok := imbdcp.lru.Get(key)
	if ok {
		if imbdcp.ttl != nil && time.Now().After(descriptor.addedAt.Add(*imbdcp.ttl)) {
			dcontext.GetLogger(ctx).Debugf("cache entry for %s has expired", dgst)
			imbdcp.lru.Remove(key)
			expiredCacheCount.Inc(1)
			return v1.Descriptor{}, distribution.ErrBlobUnknown
		}
		return descriptor.descriptor, nil
	}
	return v1.Descriptor{}, distribution.ErrBlobUnknown
}

func (imbdcp *inMemoryBlobDescriptorCacheProvider) Clear(ctx context.Context, dgst digest.Digest) error {
	key := descriptorCacheKey{
		digest: dgst,
	}
	imbdcp.lru.Remove(key)
	return nil
}

func (imbdcp *inMemoryBlobDescriptorCacheProvider) SetDescriptor(ctx context.Context, dgst digest.Digest, desc v1.Descriptor) error {
	_, err := imbdcp.Stat(ctx, dgst)
	if err == distribution.ErrBlobUnknown {
		if dgst.Algorithm() != desc.Digest.Algorithm() && dgst != desc.Digest {
			// if the digests differ, set the other canonical mapping
			if err := imbdcp.SetDescriptor(ctx, desc.Digest, desc); err != nil {
				return err
			}
		}

		if err := dgst.Validate(); err != nil {
			return err
		}

		if err := cache.ValidateDescriptor(desc); err != nil {
			return err
		}
		cacheValue := descriptionCacheValue{
			descriptor: desc,
			addedAt:    time.Now(),
		}
		key := descriptorCacheKey{
			digest: dgst,
		}
		imbdcp.lru.Add(key, cacheValue)
		return nil
	}
	// we already know it, do nothing
	return err
}

// repositoryScopedInMemoryBlobDescriptorCache provides the request scoped
// repository cache. Instances are not thread-safe but the delegated
// operations are.
type repositoryScopedInMemoryBlobDescriptorCache struct {
	repo   string
	parent *inMemoryBlobDescriptorCacheProvider // allows lazy allocation of repo's map
}

func (rsimbdcp *repositoryScopedInMemoryBlobDescriptorCache) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	if err := dgst.Validate(); err != nil {
		return v1.Descriptor{}, err
	}

	key := descriptorCacheKey{
		digest: dgst,
		repo:   rsimbdcp.repo,
	}
	descriptor, ok := rsimbdcp.parent.lru.Get(key)
	if ok {
		if rsimbdcp.parent.ttl != nil && time.Now().After(descriptor.addedAt.Add(*rsimbdcp.parent.ttl)) {
			rsimbdcp.parent.lru.Remove(key)
			expiredCacheCount.Inc(1)
			return v1.Descriptor{}, distribution.ErrBlobUnknown
		}
		return descriptor.descriptor, nil
	}
	return v1.Descriptor{}, distribution.ErrBlobUnknown
}

func (rsimbdcp *repositoryScopedInMemoryBlobDescriptorCache) Clear(ctx context.Context, dgst digest.Digest) error {
	key := descriptorCacheKey{
		digest: dgst,
		repo:   rsimbdcp.repo,
	}
	rsimbdcp.parent.lru.Remove(key)
	return nil
}

func (rsimbdcp *repositoryScopedInMemoryBlobDescriptorCache) SetDescriptor(ctx context.Context, dgst digest.Digest, desc v1.Descriptor) error {
	if err := dgst.Validate(); err != nil {
		return err
	}

	if err := cache.ValidateDescriptor(desc); err != nil {
		return err
	}

	key := descriptorCacheKey{
		digest: dgst,
		repo:   rsimbdcp.repo,
	}
	cacheValue := descriptionCacheValue{
		descriptor: desc,
		addedAt:    time.Now(),
	}
	rsimbdcp.parent.lru.Add(key, cacheValue)
	return rsimbdcp.parent.SetDescriptor(ctx, dgst, desc)
}
