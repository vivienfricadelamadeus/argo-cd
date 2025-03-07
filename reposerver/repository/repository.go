package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/TomOnTime/utfutil"
	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	textutils "github.com/argoproj/gitops-engine/pkg/utils/text"
	"github.com/argoproj/pkg/sync"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/ghodss/yaml"
	"github.com/google/go-jsonnet"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v2/reposerver/cache"
	reposervercache "github.com/argoproj/argo-cd/v2/reposerver/cache"
	"github.com/argoproj/argo-cd/v2/reposerver/metrics"
	"github.com/argoproj/argo-cd/v2/util/app/discovery"
	argopath "github.com/argoproj/argo-cd/v2/util/app/path"
	executil "github.com/argoproj/argo-cd/v2/util/exec"
	"github.com/argoproj/argo-cd/v2/util/git"
	"github.com/argoproj/argo-cd/v2/util/glob"
	"github.com/argoproj/argo-cd/v2/util/gpg"
	"github.com/argoproj/argo-cd/v2/util/helm"
	"github.com/argoproj/argo-cd/v2/util/io"
	"github.com/argoproj/argo-cd/v2/util/ksonnet"
	argokube "github.com/argoproj/argo-cd/v2/util/kube"
	"github.com/argoproj/argo-cd/v2/util/kustomize"
	"github.com/argoproj/argo-cd/v2/util/security"
	"github.com/argoproj/argo-cd/v2/util/text"
)

const (
	cachedManifestGenerationPrefix = "Manifest generation error (cached)"
	helmDepUpMarkerFile            = ".argocd-helm-dep-up"
	allowConcurrencyFile           = ".argocd-allow-concurrency"
	repoSourceFile                 = ".argocd-source.yaml"
	appSourceFile                  = ".argocd-source-%s.yaml"
	ociPrefix                      = "oci://"
)

// Service implements ManifestService interface
type Service struct {
	repoLock                  *repositoryLock
	cache                     *reposervercache.Cache
	parallelismLimitSemaphore *semaphore.Weighted
	metricsServer             *metrics.MetricsServer
	newGitClient              func(rawRepoURL string, creds git.Creds, insecure bool, enableLfs bool, proxy string, opts ...git.ClientOpts) (git.Client, error)
	newHelmClient             func(repoURL string, creds helm.Creds, enableOci bool, proxy string, opts ...helm.ClientOpts) helm.Client
	initConstants             RepoServerInitConstants
	// now is usually just time.Now, but may be replaced by unit tests for testing purposes
	now func() time.Time
}

type RepoServerInitConstants struct {
	ParallelismLimit                             int64
	PauseGenerationAfterFailedGenerationAttempts int
	PauseGenerationOnFailureForMinutes           int
	PauseGenerationOnFailureForRequests          int
}

// NewService returns a new instance of the Manifest service
func NewService(metricsServer *metrics.MetricsServer, cache *reposervercache.Cache, initConstants RepoServerInitConstants) *Service {
	var parallelismLimitSemaphore *semaphore.Weighted
	if initConstants.ParallelismLimit > 0 {
		parallelismLimitSemaphore = semaphore.NewWeighted(initConstants.ParallelismLimit)
	}
	repoLock := NewRepositoryLock()
	return &Service{
		parallelismLimitSemaphore: parallelismLimitSemaphore,
		repoLock:                  repoLock,
		cache:                     cache,
		metricsServer:             metricsServer,
		newGitClient:              git.NewClient,
		newHelmClient: func(repoURL string, creds helm.Creds, enableOci bool, proxy string, opts ...helm.ClientOpts) helm.Client {
			return helm.NewClientWithLock(repoURL, creds, sync.NewKeyLock(), enableOci, proxy, opts...)
		},
		initConstants: initConstants,
		now:           time.Now,
	}
}

// List a subset of the refs (currently, branches and tags) of a git repo
func (s *Service) ListRefs(ctx context.Context, q *apiclient.ListRefsRequest) (*apiclient.Refs, error) {
	gitClient, err := s.newClient(q.Repo)
	if err != nil {
		return nil, err
	}

	s.metricsServer.IncPendingRepoRequest(q.Repo.Repo)
	defer s.metricsServer.DecPendingRepoRequest(q.Repo.Repo)

	refs, err := gitClient.LsRefs()
	if err != nil {
		return nil, err
	}

	res := apiclient.Refs{
		Branches: refs.Branches,
		Tags:     refs.Tags,
	}

	return &res, nil
}

// ListApps lists the contents of a GitHub repo
func (s *Service) ListApps(ctx context.Context, q *apiclient.ListAppsRequest) (*apiclient.AppList, error) {
	gitClient, commitSHA, err := s.newClientResolveRevision(q.Repo, q.Revision)
	if err != nil {
		return nil, err
	}
	if apps, err := s.cache.ListApps(q.Repo.Repo, commitSHA); err == nil {
		log.Infof("cache hit: %s/%s", q.Repo.Repo, q.Revision)
		return &apiclient.AppList{Apps: apps}, nil
	}

	s.metricsServer.IncPendingRepoRequest(q.Repo.Repo)
	defer s.metricsServer.DecPendingRepoRequest(q.Repo.Repo)

	closer, err := s.repoLock.Lock(gitClient.Root(), commitSHA, true, func() error {
		return checkoutRevision(gitClient, commitSHA)
	})

	if err != nil {
		return nil, err
	}

	defer io.Close(closer)
	apps, err := discovery.Discover(gitClient.Root())
	if err != nil {
		return nil, err
	}
	err = s.cache.SetApps(q.Repo.Repo, commitSHA, apps)
	if err != nil {
		log.Warnf("cache set error %s/%s: %v", q.Repo.Repo, commitSHA, err)
	}
	res := apiclient.AppList{Apps: apps}
	return &res, nil
}

type operationSettings struct {
	sem             *semaphore.Weighted
	noCache         bool
	noRevisionCache bool
	allowConcurrent bool
}

// operationContext contains request values which are generated by runRepoOperation (on demand) by a call to the
// provided operationContextSrc function.
type operationContext struct {

	// application path or helm chart path
	appPath string

	// output of 'git verify-(tag/commit)', if signature verification is enabled (otherwise "")
	verificationResult string
}

// The 'operation' function parameter of 'runRepoOperation' may call this function to retrieve
// the appPath or GPG verificationResult.
// Failure to generate either of these values will return an error which may be cached by
// the calling function (for example, 'runManifestGen')
type operationContextSrc = func() (*operationContext, error)

// runRepoOperation downloads either git folder or helm chart and executes specified operation
// - Returns a value from the cache if present (by calling getCached(...)); if no value is present, the
// provide operation(...) is called. The specific return type of this function is determined by the
// calling function, via the provided  getCached(...) and operation(...) function.
func (s *Service) runRepoOperation(
	ctx context.Context,
	revision string,
	repo *v1alpha1.Repository,
	source *v1alpha1.ApplicationSource,
	verifyCommit bool,
	cacheFn func(cacheKey string, firstInvocation bool) (bool, error),
	operation func(repoRoot, commitSHA, cacheKey string, ctxSrc operationContextSrc) error,
	settings operationSettings) error {

	var gitClient git.Client
	var helmClient helm.Client
	var err error
	revision = textutils.FirstNonEmpty(revision, source.TargetRevision)
	if source.IsHelm() {
		helmClient, revision, err = s.newHelmClientResolveRevision(repo, revision, source.Chart, settings.noCache || settings.noRevisionCache)
		if err != nil {
			return err
		}
	} else {
		gitClient, revision, err = s.newClientResolveRevision(repo, revision, git.WithCache(s.cache, !settings.noRevisionCache && !settings.noCache))
		if err != nil {
			return err
		}
	}

	if !settings.noCache {
		if ok, err := cacheFn(revision, true); ok {
			return err
		}
	}

	s.metricsServer.IncPendingRepoRequest(repo.Repo)
	defer s.metricsServer.DecPendingRepoRequest(repo.Repo)

	if settings.sem != nil {
		err = settings.sem.Acquire(ctx, 1)
		if err != nil {
			return err
		}
		defer settings.sem.Release(1)
	}

	if source.IsHelm() {
		if settings.noCache {
			err = helmClient.CleanChartCache(source.Chart, revision)
			if err != nil {
				return err
			}
		}
		chartPath, closer, err := helmClient.ExtractChart(source.Chart, revision)
		if err != nil {
			return err
		}
		defer io.Close(closer)
		return operation(chartPath, revision, revision, func() (*operationContext, error) {
			return &operationContext{chartPath, ""}, nil
		})
	} else {
		closer, err := s.repoLock.Lock(gitClient.Root(), revision, settings.allowConcurrent, func() error {
			return checkoutRevision(gitClient, revision)
		})

		if err != nil {
			return err
		}

		defer io.Close(closer)

		commitSHA, err := gitClient.CommitSHA()
		if err != nil {
			return err
		}

		// double-check locking
		if !settings.noCache {
			if ok, err := cacheFn(revision, false); ok {
				return err
			}
		}
		// Here commitSHA refers to the SHA of the actual commit, whereas revision refers to the branch/tag name etc
		// We use the commitSHA to generate manifests and store them in cache, and revision to retrieve them from cache
		return operation(gitClient.Root(), commitSHA, revision, func() (*operationContext, error) {
			var signature string
			if verifyCommit {
				signature, err = gitClient.VerifyCommitSignature(revision)
				if err != nil {
					return nil, err
				}
			}
			appPath, err := argopath.Path(gitClient.Root(), source.Path)
			if err != nil {
				return nil, err
			}
			return &operationContext{appPath, signature}, nil
		})
	}
}

func (s *Service) GenerateManifest(ctx context.Context, q *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	var res *apiclient.ManifestResponse
	var err error

	cacheFn := func(cacheKey string, firstInvocation bool) (bool, error) {
		ok, resp, err := s.getManifestCacheEntry(cacheKey, q, firstInvocation)
		res = resp
		return ok, err
	}

	operation := func(repoRoot, commitSHA, cacheKey string, ctxSrc operationContextSrc) error {
		res, err = s.runManifestGen(repoRoot, commitSHA, cacheKey, ctxSrc, q)
		return err
	}

	settings := operationSettings{sem: s.parallelismLimitSemaphore, noCache: q.NoCache, noRevisionCache: q.NoRevisionCache, allowConcurrent: q.ApplicationSource.AllowsConcurrentProcessing()}

	err = s.runRepoOperation(ctx, q.Revision, q.Repo, q.ApplicationSource, q.VerifySignature, cacheFn, operation, settings)

	return res, err
}

// runManifestGen will be called by runRepoOperation if:
// - the cache does not contain a value for this key
// - or, the cache does contain a value for this key, but it is an expired manifest generation entry
// - or, NoCache is true
// Returns a ManifestResponse, or an error, but not both
func (s *Service) runManifestGen(repoRoot, commitSHA, cacheKey string, ctxSrc operationContextSrc, q *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	var manifestGenResult *apiclient.ManifestResponse
	ctx, err := ctxSrc()
	if err == nil {
		manifestGenResult, err = GenerateManifests(ctx.appPath, repoRoot, commitSHA, q, false)
	}
	if err != nil {

		// If manifest generation error caching is enabled
		if s.initConstants.PauseGenerationAfterFailedGenerationAttempts > 0 {

			// Retrieve a new copy (if available) of the cached response: this ensures we are updating the latest copy of the cache,
			// rather than a copy of the cache that occurred before (a potentially lengthy) manifest generation.
			innerRes := &cache.CachedManifestResponse{}
			cacheErr := s.cache.GetManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName, innerRes)
			if cacheErr != nil && cacheErr != reposervercache.ErrCacheMiss {
				log.Warnf("manifest cache set error %s: %v", q.ApplicationSource.String(), cacheErr)
				return nil, cacheErr
			}

			// If this is the first error we have seen, store the time (we only use the first failure, as this
			// value is used for PauseGenerationOnFailureForMinutes)
			if innerRes.FirstFailureTimestamp == 0 {
				innerRes.FirstFailureTimestamp = s.now().Unix()
			}

			// Update the cache to include failure information
			innerRes.NumberOfConsecutiveFailures++
			innerRes.MostRecentError = err.Error()
			cacheErr = s.cache.SetManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName, innerRes)
			if cacheErr != nil {
				log.Warnf("manifest cache set error %s: %v", q.ApplicationSource.String(), cacheErr)
				return nil, cacheErr
			}

		}
		return nil, err
	}
	// Otherwise, no error occurred, so ensure the manifest generation error data in the cache entry is reset before we cache the value
	manifestGenCacheEntry := cache.CachedManifestResponse{
		ManifestResponse:                manifestGenResult,
		NumberOfCachedResponsesReturned: 0,
		NumberOfConsecutiveFailures:     0,
		FirstFailureTimestamp:           0,
		MostRecentError:                 "",
	}
	manifestGenResult.Revision = commitSHA
	manifestGenResult.VerifyResult = ctx.verificationResult
	err = s.cache.SetManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName, &manifestGenCacheEntry)
	if err != nil {
		log.Warnf("manifest cache set error %s/%s: %v", q.ApplicationSource.String(), cacheKey, err)
	}
	return manifestGenCacheEntry.ManifestResponse, nil
}

// getManifestCacheEntry returns false if the 'generate manifests' operation should be run by runRepoOperation, e.g.:
// - If the cache result is empty for the requested key
// - If the cache is not empty, but the cached value is a manifest generation error AND we have not yet met the failure threshold (e.g. res.NumberOfConsecutiveFailures > 0 && res.NumberOfConsecutiveFailures <  s.initConstants.PauseGenerationAfterFailedGenerationAttempts)
// - If the cache is not empty, but the cache value is an error AND that generation error has expired
// and returns true otherwise.
// If true is returned, either the second or third parameter (but not both) will contain a value from the cache (a ManifestResponse, or error, respectively)
func (s *Service) getManifestCacheEntry(cacheKey string, q *apiclient.ManifestRequest, firstInvocation bool) (bool, *apiclient.ManifestResponse, error) {
	res := cache.CachedManifestResponse{}
	err := s.cache.GetManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName, &res)
	if err == nil {

		// The cache contains an existing value

		// If caching of manifest generation errors is enabled, and res is a cached manifest generation error...
		if s.initConstants.PauseGenerationAfterFailedGenerationAttempts > 0 && res.FirstFailureTimestamp > 0 {

			// If we are already in the 'manifest generation caching' state, due to too many consecutive failures...
			if res.NumberOfConsecutiveFailures >= s.initConstants.PauseGenerationAfterFailedGenerationAttempts {

				// Check if enough time has passed to try generation again (e.g. to exit the 'manifest generation caching' state)
				if s.initConstants.PauseGenerationOnFailureForMinutes > 0 {

					elapsedTimeInMinutes := int((s.now().Unix() - res.FirstFailureTimestamp) / 60)

					// After X minutes, reset the cache and retry the operation (e.g. perhaps the error is ephemeral and has passed)
					if elapsedTimeInMinutes >= s.initConstants.PauseGenerationOnFailureForMinutes {
						// We can now try again, so reset the cache state and run the operation below
						err = s.cache.DeleteManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName)
						if err != nil {
							log.Warnf("manifest cache set error %s/%s: %v", q.ApplicationSource.String(), cacheKey, err)
						}
						log.Infof("manifest error cache hit and reset: %s/%s", q.ApplicationSource.String(), cacheKey)
						return false, nil, nil
					}
				}

				// Check if enough cached responses have been returned to try generation again (e.g. to exit the 'manifest generation caching' state)
				if s.initConstants.PauseGenerationOnFailureForRequests > 0 && res.NumberOfCachedResponsesReturned > 0 {

					if res.NumberOfCachedResponsesReturned >= s.initConstants.PauseGenerationOnFailureForRequests {
						// We can now try again, so reset the error cache state and run the operation below
						err = s.cache.DeleteManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName)
						if err != nil {
							log.Warnf("manifest cache set error %s/%s: %v", q.ApplicationSource.String(), cacheKey, err)
						}
						log.Infof("manifest error cache hit and reset: %s/%s", q.ApplicationSource.String(), cacheKey)
						return false, nil, nil
					}
				}

				// Otherwise, manifest generation is still paused
				log.Infof("manifest error cache hit: %s/%s", q.ApplicationSource.String(), cacheKey)

				cachedErrorResponse := fmt.Errorf(cachedManifestGenerationPrefix+": %s", res.MostRecentError)

				if firstInvocation {
					// Increment the number of returned cached responses and push that new value to the cache
					// (if we have not already done so previously in this function)
					res.NumberOfCachedResponsesReturned++
					err = s.cache.SetManifests(cacheKey, q.ApplicationSource, q, q.Namespace, q.AppLabelKey, q.AppName, &res)
					if err != nil {
						log.Warnf("manifest cache set error %s/%s: %v", q.ApplicationSource.String(), cacheKey, err)
					}
				}

				return true, nil, cachedErrorResponse

			}

			// Otherwise we are not yet in the manifest generation error state, and not enough consecutive errors have
			// yet occurred to put us in that state.
			log.Infof("manifest error cache miss: %s/%s", q.ApplicationSource.String(), cacheKey)
			return false, res.ManifestResponse, nil
		}

		log.Infof("manifest cache hit: %s/%s", q.ApplicationSource.String(), cacheKey)
		return true, res.ManifestResponse, nil
	}

	if err != reposervercache.ErrCacheMiss {
		log.Warnf("manifest cache error %s: %v", q.ApplicationSource.String(), err)
	} else {
		log.Infof("manifest cache miss: %s/%s", q.ApplicationSource.String(), cacheKey)
	}

	return false, nil, nil
}

func getHelmRepos(repositories []*v1alpha1.Repository) []helm.HelmRepository {
	repos := make([]helm.HelmRepository, 0)
	for _, repo := range repositories {
		repos = append(repos, helm.HelmRepository{Name: repo.Name, Repo: repo.Repo, Creds: repo.GetHelmCreds(), EnableOci: repo.EnableOCI})
	}
	return repos
}

type dependencies struct {
	Dependencies []repositories `yaml:"dependencies"`
}

type repositories struct {
	Repository string `yaml:"repository"`
}

func getHelmDependencyRepos(appPath string) ([]*v1alpha1.Repository, error) {
	repos := make([]*v1alpha1.Repository, 0)
	f, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", appPath, "Chart.yaml"))
	if err != nil {
		return nil, err
	}

	d := &dependencies{}
	if err = yaml.Unmarshal(f, d); err != nil {
		return nil, err
	}

	for _, r := range d.Dependencies {
		if u, err := url.Parse(r.Repository); err == nil && (u.Scheme == "https" || u.Scheme == "oci") {
			repo := &v1alpha1.Repository{
				Repo: r.Repository,
				Name: r.Repository,
			}
			repos = append(repos, repo)
		}
	}

	return repos, nil
}

func repoExists(repo string, repos []*v1alpha1.Repository) bool {
	for _, r := range repos {
		if strings.TrimPrefix(repo, ociPrefix) == strings.TrimPrefix(r.Repo, ociPrefix) {
			return true
		}
	}
	return false
}

func isConcurrencyAllowed(appPath string) bool {
	if _, err := os.Stat(path.Join(appPath, allowConcurrencyFile)); err == nil {
		return true
	}
	return false
}

var manifestGenerateLock = sync.NewKeyLock()

// runHelmBuild executes `helm dependency build` in a given path and ensures that it is executed only once
// if multiple threads are trying to run it.
// Multiple goroutines might process same helm app in one repo concurrently when repo server process multiple
// manifest generation requests of the same commit.
func runHelmBuild(appPath string, h helm.Helm) error {
	manifestGenerateLock.Lock(appPath)
	defer manifestGenerateLock.Unlock(appPath)

	// the `helm dependency build` is potentially time consuming 1~2 seconds
	// marker file is used to check if command already run to avoid running it again unnecessary
	// file is removed when repository re-initialized (e.g. when another commit is processed)
	markerFile := path.Join(appPath, helmDepUpMarkerFile)
	_, err := os.Stat(markerFile)
	if err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	err = h.DependencyBuild()
	if err != nil {
		return err
	}
	return ioutil.WriteFile(markerFile, []byte("marker"), 0644)
}

func helmTemplate(appPath string, repoRoot string, env *v1alpha1.Env, q *apiclient.ManifestRequest, isLocal bool) ([]*unstructured.Unstructured, error) {
	concurrencyAllowed := isConcurrencyAllowed(appPath)
	if !concurrencyAllowed {
		manifestGenerateLock.Lock(appPath)
		defer manifestGenerateLock.Unlock(appPath)
	}

	templateOpts := &helm.TemplateOpts{
		Name:        q.AppName,
		Namespace:   q.Namespace,
		KubeVersion: text.SemVer(q.KubeVersion),
		APIVersions: q.ApiVersions,
		Set:         map[string]string{},
		SetString:   map[string]string{},
		SetFile:     map[string]string{},
	}

	appHelm := q.ApplicationSource.Helm
	var version string
	if appHelm != nil {
		if appHelm.Version != "" {
			version = appHelm.Version
		}
		if appHelm.ReleaseName != "" {
			templateOpts.Name = appHelm.ReleaseName
		}

		for _, val := range appHelm.ValueFiles {
			// If val is not a URL, run it against the directory enforcer. If it is a URL, use it without checking
			if _, err := url.ParseRequestURI(val); err != nil {

				// Ensure that the repo root provided is absolute
				absRepoPath, err := filepath.Abs(repoRoot)
				if err != nil {
					return nil, err
				}

				// If the path to the file is relative, join it with the current working directory (appPath)
				path := val
				if !filepath.IsAbs(path) {
					absWorkDir, err := filepath.Abs(appPath)
					if err != nil {
						return nil, err
					}
					path = filepath.Join(absWorkDir, path)
				}

				_, err = security.EnforceToCurrentRoot(absRepoPath, path)
				if err != nil {
					return nil, err
				}
			}
			templateOpts.Values = append(templateOpts.Values, val)
		}

		if appHelm.Values != "" {
			file, err := ioutil.TempFile("", "values-*.yaml")
			if err != nil {
				return nil, err
			}
			p := file.Name()
			defer func() { _ = os.RemoveAll(p) }()
			err = ioutil.WriteFile(p, []byte(appHelm.Values), 0644)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			templateOpts.Values = append(templateOpts.Values, p)
		}

		for _, p := range appHelm.Parameters {
			if p.ForceString {
				templateOpts.SetString[p.Name] = p.Value
			} else {
				templateOpts.Set[p.Name] = p.Value
			}
		}
		for _, p := range appHelm.FileParameters {
			templateOpts.SetFile[p.Name] = p.Path
		}
	}
	if templateOpts.Name == "" {
		templateOpts.Name = q.AppName
	}
	for i, j := range templateOpts.Set {
		templateOpts.Set[i] = env.Envsubst(j)
	}
	for i, j := range templateOpts.SetString {
		templateOpts.SetString[i] = env.Envsubst(j)
	}
	for i, j := range templateOpts.SetFile {
		templateOpts.SetFile[i] = env.Envsubst(j)
	}

	repos, err := getHelmDependencyRepos(appPath)
	if err != nil {
		return nil, err
	}

	for _, r := range repos {
		if !repoExists(r.Repo, q.Repos) {
			repositoryCredential := getRepoCredential(q.HelmRepoCreds, r.Repo)
			if repositoryCredential != nil {
				r.EnableOCI = repositoryCredential.EnableOCI
				r.Password = repositoryCredential.Password
				r.Username = repositoryCredential.Username
				r.SSHPrivateKey = repositoryCredential.SSHPrivateKey
				r.TLSClientCertData = repositoryCredential.TLSClientCertData
				r.TLSClientCertKey = repositoryCredential.TLSClientCertKey
			}
			q.Repos = append(q.Repos, r)
		}
	}

	var proxy string
	if q.Repo != nil {
		proxy = q.Repo.Proxy
	}

	h, err := helm.NewHelmApp(appPath, getHelmRepos(q.Repos), isLocal, version, proxy)
	if err != nil {
		return nil, err
	}

	defer h.Dispose()
	err = h.Init()
	if err != nil {
		return nil, err
	}

	out, err := h.Template(templateOpts)
	if err != nil {
		if !helm.IsMissingDependencyErr(err) {
			return nil, err
		}

		if concurrencyAllowed {
			err = runHelmBuild(appPath, h)
		} else {
			err = h.DependencyBuild()
		}

		if err != nil {
			return nil, err
		}

		out, err = h.Template(templateOpts)
		if err != nil {
			return nil, err
		}
	}
	return kube.SplitYAML([]byte(out))
}

func getRepoCredential(repoCredentials []*v1alpha1.RepoCreds, repoURL string) *v1alpha1.RepoCreds {
	for _, cred := range repoCredentials {
		url := strings.TrimPrefix(repoURL, ociPrefix)
		if strings.HasPrefix(url, cred.URL) {
			return cred
		}
	}
	return nil
}

// GenerateManifests generates manifests from a path
func GenerateManifests(appPath, repoRoot, revision string, q *apiclient.ManifestRequest, isLocal bool) (*apiclient.ManifestResponse, error) {
	var targetObjs []*unstructured.Unstructured
	var dest *v1alpha1.ApplicationDestination

	appSourceType, err := GetAppSourceType(q.ApplicationSource, appPath, q.AppName)
	if err != nil {
		return nil, err
	}
	repoURL := ""
	if q.Repo != nil {
		repoURL = q.Repo.Repo
	}
	env := newEnv(q, revision)

	switch appSourceType {
	case v1alpha1.ApplicationSourceTypeKsonnet:
		targetObjs, dest, err = ksShow(q.AppLabelKey, appPath, q.ApplicationSource.Ksonnet)
	case v1alpha1.ApplicationSourceTypeHelm:
		targetObjs, err = helmTemplate(appPath, repoRoot, env, q, isLocal)
	case v1alpha1.ApplicationSourceTypeKustomize:
		kustomizeBinary := ""
		if q.KustomizeOptions != nil {
			kustomizeBinary = q.KustomizeOptions.BinaryPath
		}
		k := kustomize.NewKustomizeApp(appPath, q.Repo.GetGitCreds(), repoURL, kustomizeBinary)
		targetObjs, _, err = k.Build(q.ApplicationSource.Kustomize, q.KustomizeOptions)
	case v1alpha1.ApplicationSourceTypePlugin:
		targetObjs, err = runConfigManagementPlugin(appPath, env, q, q.Repo.GetGitCreds())
	case v1alpha1.ApplicationSourceTypeDirectory:
		var directory *v1alpha1.ApplicationSourceDirectory
		if directory = q.ApplicationSource.Directory; directory == nil {
			directory = &v1alpha1.ApplicationSourceDirectory{}
		}
		targetObjs, err = findManifests(appPath, repoRoot, env, *directory)
	}
	if err != nil {
		return nil, err
	}

	manifests := make([]string, 0)
	for _, obj := range targetObjs {
		var targets []*unstructured.Unstructured
		if obj.IsList() {
			err = obj.EachListItem(func(object runtime.Object) error {
				unstructuredObj, ok := object.(*unstructured.Unstructured)
				if ok {
					targets = append(targets, unstructuredObj)
					return nil
				}
				return fmt.Errorf("resource list item has unexpected type")
			})
			if err != nil {
				return nil, err
			}
		} else if isNullList(obj) {
			// noop
		} else {
			targets = []*unstructured.Unstructured{obj}
		}

		for _, target := range targets {
			if q.AppLabelKey != "" && q.AppName != "" && !kube.IsCRD(target) {
				err = argokube.SetAppInstanceLabel(target, q.AppLabelKey, q.AppName)
				if err != nil {
					return nil, err
				}
			}
			manifestStr, err := json.Marshal(target.Object)
			if err != nil {
				return nil, err
			}
			manifests = append(manifests, string(manifestStr))
		}
	}

	res := apiclient.ManifestResponse{
		Manifests:  manifests,
		SourceType: string(appSourceType),
	}
	if dest != nil {
		res.Namespace = dest.Namespace
		res.Server = dest.Server
	}
	return &res, nil
}

func newEnv(q *apiclient.ManifestRequest, revision string) *v1alpha1.Env {
	return &v1alpha1.Env{
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAME", Value: q.AppName},
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAMESPACE", Value: q.Namespace},
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION", Value: revision},
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_REPO_URL", Value: q.Repo.Repo},
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_PATH", Value: q.ApplicationSource.Path},
		&v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_TARGET_REVISION", Value: q.ApplicationSource.TargetRevision},
	}
}

// mergeSourceParameters merges parameter overrides from one or more files in
// the Git repo into the given ApplicationSource objects.
//
// If .argocd-source.yaml exists at application's path in repository, it will
// be read and merged. If appName is not the empty string, and a file named
// .argocd-source-<appName>.yaml exists, it will also be read and merged.
func mergeSourceParameters(source *v1alpha1.ApplicationSource, path, appName string) error {
	repoFilePath := filepath.Join(path, repoSourceFile)
	overrides := []string{repoFilePath}
	if appName != "" {
		overrides = append(overrides, filepath.Join(path, fmt.Sprintf(appSourceFile, appName)))
	}

	var merged v1alpha1.ApplicationSource = *source.DeepCopy()

	for _, filename := range overrides {
		info, err := os.Stat(filename)
		if os.IsNotExist(err) {
			continue
		} else if info != nil && info.IsDir() {
			continue
		} else if err != nil {
			// filename should be part of error message here
			return err
		}

		data, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("%s: %v", filename, err)
		}
		patch, err := ioutil.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("%s: %v", filename, err)
		}
		patch, err = yaml.YAMLToJSON(patch)
		if err != nil {
			return fmt.Errorf("%s: %v", filename, err)
		}
		data, err = jsonpatch.MergePatch(data, patch)
		if err != nil {
			return fmt.Errorf("%s: %v", filename, err)
		}
		err = json.Unmarshal(data, &merged)
		if err != nil {
			return fmt.Errorf("%s: %v", filename, err)
		}
	}

	// make sure only config management tools related properties are used and ignore everything else
	merged.Chart = source.Chart
	merged.Path = source.Path
	merged.RepoURL = source.RepoURL
	merged.TargetRevision = source.TargetRevision

	*source = merged
	return nil
}

// GetAppSourceType returns explicit application source type or examines a directory and determines its application source type
func GetAppSourceType(source *v1alpha1.ApplicationSource, path, appName string) (v1alpha1.ApplicationSourceType, error) {
	err := mergeSourceParameters(source, path, appName)
	if err != nil {
		return "", fmt.Errorf("error while parsing source parameters: %v", err)
	}

	appSourceType, err := source.ExplicitType()
	if err != nil {
		return "", err
	}
	if appSourceType != nil {
		return *appSourceType, nil
	}
	appType, err := discovery.AppType(path)
	if err != nil {
		return "", err
	}
	return v1alpha1.ApplicationSourceType(appType), nil
}

// isNullList checks if the object is a "List" type where items is null instead of an empty list.
// Handles a corner case where obj.IsList() returns false when a manifest is like:
// ---
// apiVersion: v1
// items: null
// kind: ConfigMapList
func isNullList(obj *unstructured.Unstructured) bool {
	if _, ok := obj.Object["spec"]; ok {
		return false
	}
	if _, ok := obj.Object["status"]; ok {
		return false
	}
	field, ok := obj.Object["items"]
	if !ok {
		return false
	}
	return field == nil
}

// ksShow runs `ks show` in an app directory after setting any component parameter overrides
func ksShow(appLabelKey, appPath string, ksonnetOpts *v1alpha1.ApplicationSourceKsonnet) ([]*unstructured.Unstructured, *v1alpha1.ApplicationDestination, error) {
	ksApp, err := ksonnet.NewKsonnetApp(appPath)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "unable to load application from %s: %v", appPath, err)
	}
	if ksonnetOpts == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "Ksonnet environment not set")
	}
	for _, override := range ksonnetOpts.Parameters {
		err = ksApp.SetComponentParams(ksonnetOpts.Environment, override.Component, override.Name, override.Value)
		if err != nil {
			return nil, nil, err
		}
	}
	dest, err := ksApp.Destination(ksonnetOpts.Environment)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	targetObjs, err := ksApp.Show(ksonnetOpts.Environment)
	if err == nil && appLabelKey == common.LabelKeyLegacyApplicationName {
		// Address https://github.com/ksonnet/ksonnet/issues/707
		for _, d := range targetObjs {
			kube.UnsetLabel(d, "ksonnet.io/component")
		}
	}
	if err != nil {
		return nil, nil, err
	}
	return targetObjs, dest, nil
}

var manifestFile = regexp.MustCompile(`^.*\.(yaml|yml|json|jsonnet)$`)

// findManifests looks at all yaml files in a directory and unmarshals them into a list of unstructured objects
func findManifests(appPath string, repoRoot string, env *v1alpha1.Env, directory v1alpha1.ApplicationSourceDirectory) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	err := filepath.Walk(appPath, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if f.IsDir() {
			if path != appPath && !directory.Recurse {
				return filepath.SkipDir
			} else {
				return nil
			}
		}

		if !manifestFile.MatchString(f.Name()) {
			return nil
		}

		relPath, err := filepath.Rel(appPath, path)
		if err != nil {
			return err
		}
		if directory.Exclude != "" && glob.Match(directory.Exclude, relPath) {
			return nil
		}

		if directory.Include != "" && !glob.Match(directory.Include, relPath) {
			return nil
		}

		if strings.HasSuffix(f.Name(), ".jsonnet") {
			vm, err := makeJsonnetVm(appPath, repoRoot, directory.Jsonnet, env)
			if err != nil {
				return err
			}
			jsonStr, err := vm.EvaluateFile(path)
			if err != nil {
				return status.Errorf(codes.FailedPrecondition, "Failed to evaluate jsonnet %q: %v", f.Name(), err)
			}

			// attempt to unmarshal either array or single object
			var jsonObjs []*unstructured.Unstructured
			err = json.Unmarshal([]byte(jsonStr), &jsonObjs)
			if err == nil {
				objs = append(objs, jsonObjs...)
			} else {
				var jsonObj unstructured.Unstructured
				err = json.Unmarshal([]byte(jsonStr), &jsonObj)
				if err != nil {
					return status.Errorf(codes.FailedPrecondition, "Failed to unmarshal generated json %q: %v", f.Name(), err)
				}
				objs = append(objs, &jsonObj)
			}
		} else {
			out, err := utfutil.ReadFile(path, utfutil.UTF8)
			if err != nil {
				return err
			}
			if strings.HasSuffix(f.Name(), ".json") {
				var obj unstructured.Unstructured
				err = json.Unmarshal(out, &obj)
				if err != nil {
					return status.Errorf(codes.FailedPrecondition, "Failed to unmarshal %q: %v", f.Name(), err)
				}
				objs = append(objs, &obj)
			} else {
				yamlObjs, err := kube.SplitYAML(out)
				if err != nil {
					if len(yamlObjs) > 0 {
						// If we get here, we had a multiple objects in a single YAML file which had some
						// valid k8s objects, but errors parsing others (within the same file). It's very
						// likely the user messed up a portion of the YAML, so report on that.
						return status.Errorf(codes.FailedPrecondition, "Failed to unmarshal %q: %v", f.Name(), err)
					}
					// Otherwise, let's see if it looks like a resource, if yes, we return error
					if bytes.Contains(out, []byte("apiVersion:")) &&
						bytes.Contains(out, []byte("kind:")) &&
						bytes.Contains(out, []byte("metadata:")) {
						return status.Errorf(codes.FailedPrecondition, "Failed to unmarshal %q: %v", f.Name(), err)
					}
					// Otherwise, it might be a unrelated YAML file which we will ignore
					return nil
				}
				objs = append(objs, yamlObjs...)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objs, nil
}

func makeJsonnetVm(appPath string, repoRoot string, sourceJsonnet v1alpha1.ApplicationSourceJsonnet, env *v1alpha1.Env) (*jsonnet.VM, error) {

	vm := jsonnet.MakeVM()
	for i, j := range sourceJsonnet.TLAs {
		sourceJsonnet.TLAs[i].Value = env.Envsubst(j.Value)
	}
	for i, j := range sourceJsonnet.ExtVars {
		sourceJsonnet.ExtVars[i].Value = env.Envsubst(j.Value)
	}
	for _, arg := range sourceJsonnet.TLAs {
		if arg.Code {
			vm.TLACode(arg.Name, arg.Value)
		} else {
			vm.TLAVar(arg.Name, arg.Value)
		}
	}
	for _, extVar := range sourceJsonnet.ExtVars {
		if extVar.Code {
			vm.ExtCode(extVar.Name, extVar.Value)
		} else {
			vm.ExtVar(extVar.Name, extVar.Value)
		}
	}

	// Jsonnet Imports relative to the repository path
	jpaths := []string{appPath}
	for _, p := range sourceJsonnet.Libs {
		jpath := path.Join(repoRoot, p)
		if !strings.HasPrefix(jpath, repoRoot) {
			return nil, status.Errorf(codes.FailedPrecondition, "%s: referenced library points outside the repository", p)
		}
		jpaths = append(jpaths, jpath)
	}

	vm.Importer(&jsonnet.FileImporter{
		JPaths: jpaths,
	})

	return vm, nil
}

func runCommand(command v1alpha1.Command, path string, env []string) (string, error) {
	if len(command.Command) == 0 {
		return "", fmt.Errorf("Command is empty")
	}
	cmd := exec.Command(command.Command[0], append(command.Command[1:], command.Args...)...)
	cmd.Env = env
	cmd.Dir = path
	return executil.Run(cmd)
}

func findPlugin(plugins []*v1alpha1.ConfigManagementPlugin, name string) *v1alpha1.ConfigManagementPlugin {
	for _, plugin := range plugins {
		if plugin.Name == name {
			return plugin
		}
	}
	return nil
}

func runConfigManagementPlugin(appPath string, envVars *v1alpha1.Env, q *apiclient.ManifestRequest, creds git.Creds) ([]*unstructured.Unstructured, error) {
	concurrencyAllowed := isConcurrencyAllowed(appPath)
	if !concurrencyAllowed {
		manifestGenerateLock.Lock(appPath)
		defer manifestGenerateLock.Unlock(appPath)
	}

	plugin := findPlugin(q.Plugins, q.ApplicationSource.Plugin.Name)
	if plugin == nil {
		return nil, fmt.Errorf("config management plugin with name '%s' is not supported", q.ApplicationSource.Plugin.Name)
	}
	env := append(os.Environ(), envVars.Environ()...)
	if creds != nil {
		closer, environ, err := creds.Environ()
		if err != nil {
			return nil, err
		}
		defer func() { _ = closer.Close() }()
		env = append(env, environ...)
	}
	env = append(env, "KUBE_VERSION="+q.KubeVersion)
	env = append(env, "KUBE_API_VERSIONS="+strings.Join(q.ApiVersions, ","))

	parsedEnv := make(v1alpha1.Env, len(env))
	for i, v := range env {
		parsedVar, err := v1alpha1.NewEnvEntry(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse env vars")
		}
		parsedEnv[i] = parsedVar
	}

	pluginEnv := q.ApplicationSource.Plugin.Env
	for i, j := range pluginEnv {
		pluginEnv[i].Value = parsedEnv.Envsubst(j.Value)
	}
	env = append(env, pluginEnv.Environ()...)

	if plugin.Init != nil {
		_, err := runCommand(*plugin.Init, appPath, env)
		if err != nil {
			return nil, err
		}
	}
	out, err := runCommand(plugin.Generate, appPath, env)
	if err != nil {
		return nil, err
	}
	return kube.SplitYAML([]byte(out))
}

func (s *Service) GetAppDetails(ctx context.Context, q *apiclient.RepoServerAppDetailsQuery) (*apiclient.RepoAppDetailsResponse, error) {
	res := &apiclient.RepoAppDetailsResponse{}

	cacheFn := s.createGetAppDetailsCacheHandler(res, q)
	operation := func(repoRoot, commitSHA, revision string, ctxSrc operationContextSrc) error {
		ctx, err := ctxSrc()
		if err != nil {
			return err
		}

		appSourceType, err := GetAppSourceType(q.Source, ctx.appPath, q.AppName)
		if err != nil {
			return err
		}

		res.Type = string(appSourceType)

		switch appSourceType {
		case v1alpha1.ApplicationSourceTypeKsonnet:
			if err := populateKsonnetAppDetails(res, ctx.appPath, q); err != nil {
				return err
			}
		case v1alpha1.ApplicationSourceTypeHelm:
			if err := populateHelmAppDetails(res, ctx.appPath, q); err != nil {
				return err
			}
		case v1alpha1.ApplicationSourceTypeKustomize:
			if err := populateKustomizeAppDetails(res, q, ctx.appPath); err != nil {
				return err
			}
		}
		_ = s.cache.SetAppDetails(revision, q.Source, res)
		return nil
	}

	settings := operationSettings{allowConcurrent: q.Source.AllowsConcurrentProcessing(), noCache: q.NoCache, noRevisionCache: q.NoCache}
	err := s.runRepoOperation(ctx, q.Source.TargetRevision, q.Repo, q.Source, false, cacheFn, operation, settings)

	return res, err
}

func (s *Service) createGetAppDetailsCacheHandler(res *apiclient.RepoAppDetailsResponse, q *apiclient.RepoServerAppDetailsQuery) func(revision string, _ bool) (bool, error) {
	return func(revision string, _ bool) (bool, error) {
		err := s.cache.GetAppDetails(revision, q.Source, res)
		if err == nil {
			log.Infof("app details cache hit: %s/%s", revision, q.Source.Path)
			return true, nil
		}

		if err != reposervercache.ErrCacheMiss {
			log.Warnf("app details cache error %s: %v", revision, q.Source)
		} else {
			log.Infof("app details cache miss: %s/%s", revision, q.Source)
		}
		return false, nil
	}
}

func populateKsonnetAppDetails(res *apiclient.RepoAppDetailsResponse, appPath string, q *apiclient.RepoServerAppDetailsQuery) error {
	var ksonnetAppSpec apiclient.KsonnetAppSpec
	data, err := ioutil.ReadFile(filepath.Join(appPath, "app.yaml"))
	if err != nil {
		return err
	}
	err = yaml.Unmarshal(data, &ksonnetAppSpec)
	if err != nil {
		return err
	}
	ksApp, err := ksonnet.NewKsonnetApp(appPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "unable to load application from %s: %v", appPath, err)
	}
	env := ""
	if q.Source.Ksonnet != nil {
		env = q.Source.Ksonnet.Environment
	}
	params, err := ksApp.ListParams(env)
	if err != nil {
		return err
	}
	ksonnetAppSpec.Parameters = params
	res.Ksonnet = &ksonnetAppSpec
	return nil
}

func populateHelmAppDetails(res *apiclient.RepoAppDetailsResponse, appPath string, q *apiclient.RepoServerAppDetailsQuery) error {
	var selectedValueFiles []string

	if q.Source.Helm != nil {
		selectedValueFiles = q.Source.Helm.ValueFiles
	}

	availableValueFiles, err := findHelmValueFilesInPath(appPath)
	if err != nil {
		return err
	}

	res.Helm = &apiclient.HelmAppSpec{ValueFiles: availableValueFiles}
	var version string
	if q.Source.Helm != nil {
		if q.Source.Helm.Version != "" {
			version = q.Source.Helm.Version
		}
	}
	h, err := helm.NewHelmApp(appPath, getHelmRepos(q.Repos), false, version, q.Repo.Proxy)
	if err != nil {
		return err
	}
	defer h.Dispose()
	err = h.Init()
	if err != nil {
		return err
	}

	if err := loadFileIntoIfExists(filepath.Join(appPath, "values.yaml"), &res.Helm.Values); err != nil {
		return err
	}
	params, err := h.GetParameters(selectedValueFiles)
	if err != nil {
		return err
	}
	for k, v := range params {
		res.Helm.Parameters = append(res.Helm.Parameters, &v1alpha1.HelmParameter{
			Name:  k,
			Value: v,
		})
	}
	for _, v := range fileParameters(q) {
		res.Helm.FileParameters = append(res.Helm.FileParameters, &v1alpha1.HelmFileParameter{
			Name: v.Name,
			Path: v.Path, //filepath.Join(appPath, v.Path),
		})
	}
	return nil
}

func loadFileIntoIfExists(path string, destination *string) error {
	info, err := os.Stat(path)

	if err == nil && !info.IsDir() {
		if bytes, err := ioutil.ReadFile(path); err != nil {
			*destination = string(bytes)
		} else {
			return err
		}
	}

	return nil
}

func findHelmValueFilesInPath(path string) ([]string, error) {
	var result []string

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return result, err
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		filename := f.Name()
		fileNameExt := strings.ToLower(filepath.Ext(filename))
		if strings.Contains(filename, "values") && (fileNameExt == ".yaml" || fileNameExt == ".yml") {
			result = append(result, filename)
		}
	}

	return result, nil
}

func populateKustomizeAppDetails(res *apiclient.RepoAppDetailsResponse, q *apiclient.RepoServerAppDetailsQuery, appPath string) error {
	res.Kustomize = &apiclient.KustomizeAppSpec{}
	kustomizeBinary := ""
	if q.KustomizeOptions != nil {
		kustomizeBinary = q.KustomizeOptions.BinaryPath
	}
	k := kustomize.NewKustomizeApp(appPath, q.Repo.GetGitCreds(), q.Repo.Repo, kustomizeBinary)
	_, images, err := k.Build(q.Source.Kustomize, q.KustomizeOptions)
	if err != nil {
		return err
	}
	res.Kustomize.Images = images
	return nil
}

func (s *Service) GetRevisionMetadata(ctx context.Context, q *apiclient.RepoServerRevisionMetadataRequest) (*v1alpha1.RevisionMetadata, error) {
	if !(git.IsCommitSHA(q.Revision) || git.IsTruncatedCommitSHA(q.Revision)) {
		return nil, fmt.Errorf("revision %s must be resolved", q.Revision)
	}
	metadata, err := s.cache.GetRevisionMetadata(q.Repo.Repo, q.Revision)
	if err == nil {
		// The logic here is that if a signature check on metadata is requested,
		// but there is none in the cache, we handle as if we have a cache miss
		// and re-generate the meta data. Otherwise, if there is signature info
		// in the metadata, but none was requested, we remove it from the data
		// that we return.
		if q.CheckSignature && metadata.SignatureInfo == "" {
			log.Infof("revision metadata cache hit, but need to regenerate due to missing signature info: %s/%s", q.Repo.Repo, q.Revision)
		} else {
			log.Infof("revision metadata cache hit: %s/%s", q.Repo.Repo, q.Revision)
			if !q.CheckSignature {
				metadata.SignatureInfo = ""
			}
			return metadata, nil
		}
	} else {
		if err != reposervercache.ErrCacheMiss {
			log.Warnf("revision metadata cache error %s/%s: %v", q.Repo.Repo, q.Revision, err)
		} else {
			log.Infof("revision metadata cache miss: %s/%s", q.Repo.Repo, q.Revision)
		}
	}

	gitClient, _, err := s.newClientResolveRevision(q.Repo, q.Revision)
	if err != nil {
		return nil, err
	}

	s.metricsServer.IncPendingRepoRequest(q.Repo.Repo)
	defer s.metricsServer.DecPendingRepoRequest(q.Repo.Repo)

	closer, err := s.repoLock.Lock(gitClient.Root(), q.Revision, true, func() error {
		return checkoutRevision(gitClient, q.Revision)
	})

	if err != nil {
		return nil, err
	}

	defer io.Close(closer)

	m, err := gitClient.RevisionMetadata(q.Revision)
	if err != nil {
		return nil, err
	}

	// Run gpg verify-commit on the revision
	signatureInfo := ""
	if gpg.IsGPGEnabled() && q.CheckSignature {
		cs, err := gitClient.VerifyCommitSignature(q.Revision)
		if err != nil {
			log.Errorf("error verifying signature of commit '%s' in repo '%s': %v", q.Revision, q.Repo.Repo, err)
			return nil, err
		}

		if cs != "" {
			vr := gpg.ParseGitCommitVerification(cs)
			if vr.Result == gpg.VerifyResultUnknown {
				signatureInfo = fmt.Sprintf("UNKNOWN signature: %s", vr.Message)
			} else {
				signatureInfo = fmt.Sprintf("%s signature from %s key %s", vr.Result, vr.Cipher, gpg.KeyID(vr.KeyID))
			}
		} else {
			signatureInfo = "Revision is not signed."
		}
	}

	metadata = &v1alpha1.RevisionMetadata{Author: m.Author, Date: metav1.Time{Time: m.Date}, Tags: m.Tags, Message: m.Message, SignatureInfo: signatureInfo}
	_ = s.cache.SetRevisionMetadata(q.Repo.Repo, q.Revision, metadata)
	return metadata, nil
}

func fileParameters(q *apiclient.RepoServerAppDetailsQuery) []v1alpha1.HelmFileParameter {
	if q.Source.Helm == nil {
		return nil
	}
	return q.Source.Helm.FileParameters
}

func (s *Service) newClient(repo *v1alpha1.Repository, opts ...git.ClientOpts) (git.Client, error) {
	opts = append(opts, git.WithEventHandlers(metrics.NewGitClientEventHandlers(s.metricsServer)))
	return s.newGitClient(repo.Repo, repo.GetGitCreds(), repo.IsInsecure(), repo.EnableLFS, repo.Proxy, opts...)
}

// newClientResolveRevision is a helper to perform the common task of instantiating a git client
// and resolving a revision to a commit SHA
func (s *Service) newClientResolveRevision(repo *v1alpha1.Repository, revision string, opts ...git.ClientOpts) (git.Client, string, error) {
	gitClient, err := s.newClient(repo, opts...)
	if err != nil {
		return nil, "", err
	}
	commitSHA, err := gitClient.LsRemote(revision)
	if err != nil {
		return nil, "", err
	}
	return gitClient, commitSHA, nil
}

func (s *Service) newHelmClientResolveRevision(repo *v1alpha1.Repository, revision string, chart string, noRevisionCache bool) (helm.Client, string, error) {
	enableOCI := repo.EnableOCI || helm.IsHelmOciRepo(repo.Repo)
	helmClient := s.newHelmClient(repo.Repo, repo.GetHelmCreds(), enableOCI, repo.Proxy, helm.WithIndexCache(s.cache))
	// OCI helm registers don't support semver ranges. Assuming that given revision is exact version
	if helm.IsVersion(revision) || enableOCI {
		return helmClient, revision, nil
	}
	constraints, err := semver.NewConstraint(revision)
	if err != nil {
		return nil, "", fmt.Errorf("invalid revision '%s': %v", revision, err)
	}
	index, err := helmClient.GetIndex(noRevisionCache)
	if err != nil {
		return nil, "", err
	}
	entries, err := index.GetEntries(chart)
	if err != nil {
		return nil, "", err
	}
	version, err := entries.MaxVersion(constraints)
	if err != nil {
		return nil, "", err
	}
	return helmClient, version.String(), nil
}

// checkoutRevision is a convenience function to initialize a repo, fetch, and checkout a revision
// Returns the 40 character commit SHA after the checkout has been performed
// nolint:unparam
func checkoutRevision(gitClient git.Client, revision string) error {
	err := gitClient.Init()
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to initialize git repo: %v", err)
	}

	// Some git providers don't support fetching commit sha
	if revision != "" && !git.IsCommitSHA(revision) && !git.IsTruncatedCommitSHA(revision) {
		err = gitClient.Fetch(revision)
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to fetch %s: %v", revision, err)
		}
		err = gitClient.Checkout("FETCH_HEAD")
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to checkout FETCH_HEAD: %v", err)
		}
	} else {
		err = gitClient.Fetch("")
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to fetch %s: %v", revision, err)
		}
		err = gitClient.Checkout(revision)
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to checkout %s: %v", revision, err)
		}
	}

	return err
}

func (s *Service) GetHelmCharts(ctx context.Context, q *apiclient.HelmChartsRequest) (*apiclient.HelmChartsResponse, error) {
	index, err := s.newHelmClient(q.Repo.Repo, q.Repo.GetHelmCreds(), q.Repo.EnableOCI, q.Repo.Proxy).GetIndex(true)
	if err != nil {
		return nil, err
	}
	res := apiclient.HelmChartsResponse{}
	for chartName, entries := range index.Entries {
		chart := apiclient.HelmChart{
			Name: chartName,
		}
		for _, entry := range entries {
			chart.Versions = append(chart.Versions, entry.Version)
		}
		res.Items = append(res.Items, &chart)
	}
	return &res, nil
}

func (s *Service) TestRepository(ctx context.Context, q *apiclient.TestRepositoryRequest) (*apiclient.TestRepositoryResponse, error) {
	repo := q.Repo
	checks := map[string]func() error{
		"git": func() error {
			return git.TestRepo(repo.Repo, repo.GetGitCreds(), repo.IsInsecure(), repo.IsLFSEnabled(), repo.Proxy)
		},
		"helm": func() error {
			if repo.EnableOCI {
				if !helm.IsHelmOciRepo(repo.Repo) {
					return errors.New("OCI Helm repository URL should include hostname and port only")
				}
				_, err := helm.NewClient(repo.Repo, repo.GetHelmCreds(), repo.EnableOCI, repo.Proxy).TestHelmOCI()
				return err
			} else {
				_, err := helm.NewClient(repo.Repo, repo.GetHelmCreds(), repo.EnableOCI, repo.Proxy).GetIndex(false)
				return err
			}
		},
	}
	if check, ok := checks[repo.Type]; ok {
		return &apiclient.TestRepositoryResponse{VerifiedRepository: false}, check()
	}
	var err error
	for _, check := range checks {
		err = check()
		if err == nil {
			return &apiclient.TestRepositoryResponse{VerifiedRepository: true}, nil
		}
	}
	return &apiclient.TestRepositoryResponse{VerifiedRepository: false}, err
}
