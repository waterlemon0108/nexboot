package local

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

type releaseRacePool struct {
	*fakePool
	listed chan struct{}
	once   sync.Once
}

func (z *releaseRacePool) ListOrigins(ctx context.Context) (map[string]string, error) {
	origins, err := z.fakePool.ListOrigins(ctx)
	z.once.Do(func() { close(z.listed) })
	return origins, err
}

func TestReleaseIdentityLayersRechecksOriginAfterTakingTheMACLock(t *testing.T) {
	const mac = "001122334455"
	const layer = "tank/run/IDENT-001122334455"
	z := &releaseRacePool{fakePool: newFakePool(), listed: make(chan struct{})}
	z.datasets = []string{layer}
	z.origin[layer] = "tank/nd-rebuilding-1/cfg@0"
	a := New("node-a", z, &fakeExporter{})
	unlock := a.lockClient(mac)
	done := make(chan error, 1)
	go func() {
		_, err := a.ReleaseIdentityLayers(context.Background(), []string{"tank/nd-rebuilding-1"})
		done <- err
	}()
	select {
	case <-z.listed:
	case <-time.After(time.Second):
		unlock()
		t.Fatal("release did not scan origins")
	}
	// Another lifecycle operation replaced the layer before release acquired this MAC's lock.
	z.origin[layer] = "tank/nd/current@1"
	a.provisionedAt.Store(mac, time.Now())
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !z.present(layer) {
		t.Fatalf("new identity was deleted using an old origin: %v", z.destroyed)
	}
}

func TestReleaseIdentityLayersNeverTearsDownAnotherLifecycle(t *testing.T) {
	for _, kind := range []string{"recent", "session", "CLIENT-", "SCLIENT-", "ADAPT-", "INSTALL-", "dependent"} {
		t.Run(kind, func(t *testing.T) {
			const mac = "001122334455"
			layer := "tank/run/IDENT-" + mac
			z := newFakePool()
			z.datasets = []string{layer}
			z.origin[layer] = "tank/nd-rebuilding-1/cfg@0"
			exporter := &fakeExporter{}
			a := New("node-a", z, exporter)
			if kind == "recent" {
				a.provisionedAt.Store(mac, time.Now())
			} else if kind == "session" {
				exporter.sessions = []string{mac}
			} else {
				other := "tank/run/" + kind + mac
				if kind == "INSTALL-" {
					other += "-install-0123456789abcdef"
				}
				if kind == "dependent" {
					other = "tank/nd/saved-image"
					z.origin[other] = layer + "@ready"
				}
				z.datasets = append(z.datasets, other)
			}
			before := slices.Clone(z.datasets)
			_, err := a.ReleaseIdentityLayers(context.Background(), []string{"tank/nd-rebuilding-1"})
			if err == nil || !strings.Contains(err.Error(), mac) || !strings.Contains(err.Error(), "cfg@0") {
				t.Fatalf("matching busy layer must report a named refusal, got %v", err)
			}
			if !slices.Equal(z.datasets, before) || len(z.destroyAttempts) > 0 || exporter.tornTarget != "" {
				t.Fatalf("automatic release changed protected lifecycle: datasets=%v destroyed=%v target=%s", z.datasets, z.destroyAttempts, exporter.tornTarget)
			}
		})
	}
}

func TestReleaseIdentityLayersRequiresSessionVisibilityAndOwnRunDataset(t *testing.T) {
	for _, which := range []string{"missing-session-reader", "foreign-pool", "nested-spoof"} {
		t.Run(which, func(t *testing.T) {
			z := newFakePool()
			layer := "tank/run/IDENT-001122334455"
			var exporter []Exporter
			if which != "missing-session-reader" {
				exporter = []Exporter{&fakeExporter{}}
			}
			if which == "foreign-pool" {
				layer = "other/run/IDENT-001122334455"
			}
			if which == "nested-spoof" {
				layer = "tank/nd/IDENT-001122334455"
			}
			z.datasets = []string{layer}
			z.origin[layer] = "tank/nd-rebuilding-1/cfg@0"
			a := New("node-a", z, exporter...)
			_, err := a.ReleaseIdentityLayers(context.Background(), []string{"tank/nd-rebuilding-1"})
			if which == "missing-session-reader" && err == nil {
				t.Fatal("missing session reader must fail closed")
			}
			if len(z.destroyAttempts) > 0 {
				t.Fatalf("must not destroy a foreign/unverifiable layer: %v", z.destroyAttempts)
			}
		})
	}
}

var _ storage.IdentityHostAgent = (*Agent)(nil)

func TestReleaseIdentityLayersValidatesAndResolvesExpectedSources(t *testing.T) {
	for _, source := range []string{"cfg@old", "tank/nd/cfg@old", "tank/nd-rebuilding-123/cfg@old", "tank/nd-rebuilding-123", "tank/nd-diverged-123/cfg@old", "tank/nd-diverged-123", "", "tank", "tank/nd", "tank/nd@rep-9", "other/nd/cfg@old", "tank/run", "tank/nd/../run", "tank/nd/cfg@", "tank/nd-rebuilding-nope"} {
		t.Run(source, func(t *testing.T) {
			z := newFakePool()
			layer := "tank/run/IDENT-001122334455"
			z.datasets = []string{layer}
			origin := "tank/nd/cfg@old"
			valid := source == "cfg@old" || source == "tank/nd/cfg@old" || source == "tank/nd-rebuilding-123/cfg@old" || source == "tank/nd-rebuilding-123" || source == "tank/nd-diverged-123/cfg@old"
			if strings.Contains(source, "rebuilding-123") {
				origin = "tank/nd-rebuilding-123/cfg@old"
			}
			if source == "tank/nd-diverged-123/cfg@old" {
				origin = source
			}
			z.origin[layer] = origin
			a := New("node-a", z, &fakeExporter{})
			released, err := a.ReleaseIdentityLayers(context.Background(), []string{source})
			if valid {
				if err != nil || len(released) != 1 || z.present(layer) {
					t.Fatalf("expected source did not release cache: %v %v", released, err)
				}
			} else {
				if err == nil || len(z.destroyAttempts) > 0 {
					t.Fatalf("invalid release scope accepted: %q %v %v", source, err, z.destroyAttempts)
				}
			}
		})
	}
}

func TestReleaseIdentityLayersProtectsTheGapBetweenIdentityPrepareAndClientProvisioning(t *testing.T) {
	z := seedSuperPool()
	a := New("node", z, &fakeExporter{})
	a.identityWriteFn = func(context.Context, string, storage.IdentityLayerReq) error { return nil }
	req := storage.IdentityLayerReq{MAC: "001122334455", ConfigID: "cfg", SnapshotName: "0", InstanceID: "test"}
	if err := a.CreateIdentityLayer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	layer := z.Dataset(storage.IdentityCloneName(req.MAC))
	if _, err := a.ReleaseIdentityLayers(context.Background(), []string{"cfg@0"}); err == nil {
		t.Fatal("foreground preparation must report busy, not successful cleanup")
	}
	if !z.present(layer) {
		t.Fatal("foreground identity prepare was deleted before CreateClientLUN could use it")
	}
}

// 暂存的数据被空闲身份层克隆着时要能释放，否则这份数据永远回收不掉。
func TestReleaseIdentityLayersAcceptsAHeldCopyAsSource(t *testing.T) {
	const layer = "tank/run/IDENT-001122334455"
	z := newFakePool()
	z.datasets = []string{"tank/nd-held-1", layer}
	z.origin[layer] = "tank/nd-held-1@0"
	a := New("node-a", z, &fakeExporter{})
	released, err := a.ReleaseIdentityLayers(context.Background(), []string{"tank/nd-held-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(released, []string{"001122334455"}) || z.present(layer) {
		t.Fatalf("身份层没有释放：released=%v datasets=%v", released, z.datasets)
	}
}
