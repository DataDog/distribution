package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/cache/memory"
	"github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/distribution/reference"
)

type fillTestEnv struct {
	upstream *fakeUpstream
	store    *proxyBlobStore
	spoolDir string
}

func newFillTestEnv(t *testing.T, size int, spoolMaxSize int64) *fillTestEnv {
	t.Helper()
	upstream := newFakeUpstream(t, size)
	upstream.gate = make(chan struct{})

	driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := storage.NewRegistry(t.Context(), driver,
		storage.BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)))
	if err != nil {
		t.Fatal(err)
	}
	name, err := reference.WithName("foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := registry.Repository(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}

	spoolDir := t.TempDir()
	fills, err := newFillRegistry(configuration.SharedFetch{Enabled: true, SpoolDirectory: spoolDir, SpoolMaxSize: spoolMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	return &fillTestEnv{
		upstream: upstream,
		spoolDir: spoolDir,
		store: &proxyBlobStore{
			localStore:        repo.Blobs(t.Context()),
			remoteStore:       upstream.remoteStore(t.Context()),
			cacheWriteTimeout: time.Minute,
			repositoryName:    name,
			authChallenger:    &mockChallenger{},
			fills:             fills,
		},
	}
}

func (env *fillTestEnv) serve(ctx context.Context, rangeValue string) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if rangeValue != "" {
		r.Header.Set("Range", rangeValue)
	}
	err := env.store.ServeBlob(ctx, w, r, env.upstream.desc.Digest)
	return w, err
}

func (env *fillTestEnv) refs() int {
	key := fillKey{repository: env.store.repositoryName.Name(), dgst: env.upstream.desc.Digest}
	fr := env.store.fills
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if f := fr.fills[key]; f != nil {
		return f.refs
	}
	return 0
}

func (env *fillTestEnv) assertCached(t *testing.T) {
	t.Helper()
	rc, err := env.store.localStore.Open(t.Context(), env.upstream.desc.Digest)
	if err != nil {
		t.Fatalf("blob not cached: %v", err)
	}
	defer rc.Close()
	cached, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, env.upstream.blob) {
		t.Fatal("cached blob does not match the upstream blob")
	}
}

func (env *fillTestEnv) assertSpoolReleased(t *testing.T) {
	t.Helper()
	eventually(t, func() bool { return env.store.fills.spool.used.Load() == 0 })
	entries, err := os.ReadDir(env.spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("spool directory has %d entries, want 0", len(entries))
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewFillRegistry(t *testing.T) {
	fills, err := newFillRegistry(configuration.SharedFetch{SpoolDirectory: "/nonexistent/unused"})
	if err != nil || fills != nil {
		t.Fatalf("disabled: got (%v, %v), want (nil, nil)", fills, err)
	}

	dir := filepath.Join(t.TempDir(), "spool")
	fills, err = newFillRegistry(configuration.SharedFetch{Enabled: true, SpoolDirectory: dir})
	if err != nil {
		t.Fatal(err)
	}
	if fills == nil {
		t.Fatal("enabled: got nil registry")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("spool directory not created: %v", err)
	}
}

func TestFillConcurrentRequestsShareOneFetch(t *testing.T) {
	env := newFillTestEnv(t, 1<<20, 0)
	ranges := []string{"", "", "", "bytes=0-1023", "bytes=4096-8191", "bytes=-1024"}

	var wg sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, len(ranges))
	for i, rangeValue := range ranges {
		if i == 1 {
			eventually(t, func() bool { return env.refs() == 2 })
		}
		wg.Go(func() {
			w, err := env.serve(t.Context(), rangeValue)
			if err != nil {
				t.Error(err)
			}
			responses[i] = w
		})
	}
	// The leader and the fetch goroutine hold two references, each other request holds one.
	eventually(t, func() bool { return env.refs() == len(ranges)+1 })
	close(env.upstream.gate)
	wg.Wait()

	if got := env.upstream.gets(); !slices.Equal(got, []string{""}) {
		t.Errorf("upstream GET ranges: got %q, want one unranged GET", got)
	}
	for i, rangeValue := range ranges {
		want := env.upstream.blob
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Range", rangeValue)
		if start, end, ok := singleRange(r, env.upstream.desc.Size); ok {
			want = want[start:end]
		}
		if !bytes.Equal(responses[i].Body.Bytes(), want) {
			t.Errorf("request %d (%q): got %d bytes, want %d", i, rangeValue, responses[i].Body.Len(), len(want))
		}
	}
	env.assertCached(t)
	env.assertSpoolReleased(t)
}

func TestFillFarRangeReadsRemote(t *testing.T) {
	env := newFillTestEnv(t, 3*spoolLookahead, 0)

	leaderDone := make(chan error, 1)
	go func() {
		_, err := env.serve(t.Context(), "")
		leaderDone <- err
	}()
	eventually(t, func() bool { return env.refs() == 2 })

	start := 2 * spoolLookahead
	w, err := env.serve(t.Context(), "bytes=16777216-16778239")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), env.upstream.blob[start:start+1024]) {
		t.Errorf("far range body: got %d bytes, want 1024", w.Body.Len())
	}

	close(env.upstream.gate)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
	got := env.upstream.gets()
	slices.Sort(got)
	if want := []string{"", "bytes=16777216-16778239"}; !slices.Equal(got, want) {
		t.Errorf("upstream GET ranges: got %q, want %q", got, want)
	}
	env.assertCached(t)
}

func TestFillSurvivesLeaderDisconnect(t *testing.T) {
	env := newFillTestEnv(t, 1<<20, 0)

	ctx, cancel := context.WithCancel(t.Context())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := env.serve(ctx, "")
		leaderDone <- err
	}()
	eventually(t, func() bool { return env.refs() == 2 })
	cancel()
	if err := <-leaderDone; err != nil {
		t.Fatalf("canceled leader: got %v, want nil", err)
	}

	close(env.upstream.gate)
	eventually(t, func() bool {
		_, err := env.store.localStore.Stat(t.Context(), env.upstream.desc.Digest)
		return err == nil
	})
	env.assertCached(t)
	env.assertSpoolReleased(t)
}

func TestFillWithoutSpoolBudget(t *testing.T) {
	env := newFillTestEnv(t, 1<<20, 1)
	close(env.upstream.gate)

	w, err := env.serve(t.Context(), "bytes=0-1023")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), env.upstream.blob[:1024]) {
		t.Errorf("range body: got %d bytes, want 1024", w.Body.Len())
	}
	env.assertCached(t)
	got := env.upstream.gets()
	slices.Sort(got)
	if want := []string{"", "bytes=0-1023"}; !slices.Equal(got, want) {
		t.Errorf("upstream GET ranges: got %q, want one fill and one ranged GET", got)
	}
	env.assertSpoolReleased(t)
}

func TestFillFailure(t *testing.T) {
	env := newFillTestEnv(t, 1<<20, 0)
	env.upstream.truncate = true

	followerDone := make(chan *httptest.ResponseRecorder, 1)
	leaderDone := make(chan error, 1)
	go func() {
		_, err := env.serve(t.Context(), "")
		leaderDone <- err
	}()
	eventually(t, func() bool { return env.refs() == 2 })
	go func() {
		w, _ := env.serve(t.Context(), "")
		followerDone <- w
	}()
	eventually(t, func() bool { return env.refs() == 3 })
	close(env.upstream.gate)

	if err := <-leaderDone; err == nil {
		t.Fatal("expected the leader to return the fill error")
	}
	if w := <-followerDone; w.Body.Len() >= int(env.upstream.desc.Size) {
		t.Errorf("follower got %d bytes from a failed fill", w.Body.Len())
	}
	if _, err := env.store.localStore.Stat(t.Context(), env.upstream.desc.Digest); err == nil {
		t.Fatal("failed fill was cached")
	}
	env.assertSpoolReleased(t)

	env.upstream.mu.Lock()
	env.upstream.truncate = false
	env.upstream.mu.Unlock()
	if _, err := env.serve(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	env.assertCached(t)
}
