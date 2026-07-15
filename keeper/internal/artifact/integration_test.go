//go:build integration

package artifact

import (
	"context"
	"sync"
	"testing"
)

// TestIntegration_ConcurrentLoadsSameSnapshot runs several parallel Load calls
// for the same service/ref through local-fs git: a per-service Mutex must
// serialize the git operations, and all calls must return the same snapshot
// with no races and no partial directories.
func TestIntegration_ConcurrentLoadsSameSnapshot(t *testing.T) {
	tr := newTestRepo(t)
	tr.writeFile("scenario/deploy/main.yml", "on: keeper\n")
	tr.commit("scenario")

	loader := newLoader(t)
	ref := ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"}

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			art, err := loader.Load(context.Background(), ref)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = art.LocalDir
		}(i)
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("goroutine %d вернула другой снапшот: %s != %s", i, results[i], results[0])
		}
	}
}

// TestIntegration_MultiServiceParallel checks that different services load
// independently (separate cache directories, separate per-service locks).
func TestIntegration_MultiServiceParallel(t *testing.T) {
	loader := newLoader(t)

	repos := map[string]*testRepo{
		"web-app": newTestRepo(t),
		"db-core": newTestRepo(t),
	}

	var wg sync.WaitGroup
	for name, tr := range repos {
		wg.Add(1)
		go func(name string, tr *testRepo) {
			defer wg.Done()
			art, err := loader.Load(context.Background(), ServiceRef{Name: name, Git: tr.fileURL()})
			if err != nil {
				t.Errorf("Load %s: %v", name, err)
				return
			}
			if art.SHA1 != tr.headSHA() {
				t.Errorf("%s: SHA1 %s != HEAD %s", name, art.SHA1, tr.headSHA())
			}
		}(name, tr)
	}
	wg.Wait()
}
