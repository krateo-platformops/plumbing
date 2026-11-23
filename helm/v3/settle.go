package helm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// SettleInterruptedInstall: see helm.Client.
func (c *client) SettleInterruptedInstall(ctx context.Context, releaseName string, revision int) error {
	store, err := c.conditionalReleases()
	if err != nil {
		return fmt.Errorf("settling an interrupted install: %w", err)
	}
	history, err := store.History(releaseName)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return fmt.Errorf("settling an interrupted install: reading the release history: %w", err)
	}
	if len(history) == 0 {
		return fmt.Errorf("%w: %s has no revisions", helmconfig.ErrNotInterruptedInstall, releaseName)
	}
	releaseutil.SortByRevision(history)
	for _, rel := range history {
		if rel.Info.Status == release.StatusDeployed {
			return fmt.Errorf("%w: %s revision %d is deployed", helmconfig.ErrNotInterruptedInstall, releaseName, rel.Version)
		}
	}
	if last := history[len(history)-1]; last.Version != revision {
		return fmt.Errorf("%w: the latest revision of %s is %d, not %d", helmconfig.ErrNotInterruptedInstall, releaseName, last.Version, revision)
	}
	// Read again by itself: this read's resourceVersion is the one the write is conditional on, so the
	// status decided on is the status replaced.
	rel, err := store.Get(releaseName, revision)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return fmt.Errorf("%w: %s revision %d is gone", helmconfig.ErrNotInterruptedInstall, releaseName, revision)
	}
	if err != nil {
		return fmt.Errorf("settling an interrupted install: %w", err)
	}
	if rel.Info.Status != release.StatusPendingInstall {
		return fmt.Errorf("%w: %s revision %d is %s", helmconfig.ErrNotInterruptedInstall, releaseName, revision, rel.Info.Status)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rel.SetStatus(release.StatusFailed, "Install interrupted: the process running it ended before it finished")
	if err := store.Update(rel); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %s revision %d was written meanwhile: %w", helmconfig.ErrNotInterruptedInstall, releaseName, revision, err)
		}
		return fmt.Errorf("settling an interrupted install: %w", err)
	}
	return nil
}

// conditionalReleases is helm's release storage whose Update replaces only the record it last read.
// helm's own drivers send no resourceVersion, so their update is last-write-wins.
func (c *client) conditionalReleases() (*storage.Storage, error) {
	kube, err := kubernetes.NewForConfig(c.restConfig)
	if err != nil {
		return nil, err
	}
	switch name := os.Getenv("HELM_DRIVER"); name {
	case "secret", "secrets", "":
		return storage.Init(driver.NewSecrets(&conditionalSecrets{SecretInterface: kube.CoreV1().Secrets(c.namespace)})), nil
	case "configmap", "configmaps":
		return storage.Init(driver.NewConfigMaps(&conditionalConfigMaps{ConfigMapInterface: kube.CoreV1().ConfigMaps(c.namespace)})), nil
	default:
		return nil, fmt.Errorf("HELM_DRIVER %q cannot be written conditionally", name)
	}
}

// readVersions remembers each object's resourceVersion as last read by name.
type readVersions struct {
	mu sync.Mutex
	rv map[string]string
}

func (v *readVersions) set(name, rv string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.rv == nil {
		v.rv = map[string]string{}
	}
	v.rv[name] = rv
}

// get refuses an object that was never read: an empty resourceVersion would make the update
// unconditional.
func (v *readVersions) get(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.rv[name] == "" {
		return "", fmt.Errorf("%s was not read before its update", name)
	}
	return v.rv[name], nil
}

type conditionalSecrets struct {
	typedcorev1.SecretInterface
	read readVersions
}

func (s *conditionalSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error) {
	obj, err := s.SecretInterface.Get(ctx, name, opts)
	if err == nil {
		s.read.set(name, obj.ResourceVersion)
	}
	return obj, err
}

func (s *conditionalSecrets) Update(ctx context.Context, obj *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error) {
	rv, err := s.read.get(obj.Name)
	if err != nil {
		return nil, err
	}
	obj.ResourceVersion = rv
	return s.SecretInterface.Update(ctx, obj, opts)
}

type conditionalConfigMaps struct {
	typedcorev1.ConfigMapInterface
	read readVersions
}

func (s *conditionalConfigMaps) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	obj, err := s.ConfigMapInterface.Get(ctx, name, opts)
	if err == nil {
		s.read.set(name, obj.ResourceVersion)
	}
	return obj, err
}

func (s *conditionalConfigMaps) Update(ctx context.Context, obj *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	rv, err := s.read.get(obj.Name)
	if err != nil {
		return nil, err
	}
	obj.ResourceVersion = rv
	return s.ConfigMapInterface.Update(ctx, obj, opts)
}
