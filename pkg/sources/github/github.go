package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/rand"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/go-logr/logr"
	"github.com/gobwas/glob"
	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/trufflesecurity/trufflehog/v3/pkg/cache"
	"github.com/trufflesecurity/trufflehog/v3/pkg/cache/memory"
	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
	"github.com/trufflesecurity/trufflehog/v3/pkg/giturl"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/credentialspb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/source_metadatapb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/sourcespb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sanitizer"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sources"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sources/git"
)

const (
	SourceType = sourcespb.SourceType_SOURCE_TYPE_GITHUB

	unauthGithubOrgRateLimt = 30
	defaultPagination       = 100
	membersAppPagination    = 500
)

type Source struct {
	name string
	// Protects the user and token.
	userMu      sync.Mutex
	githubUser  string
	githubToken string

	sourceID          sources.SourceID
	jobID             sources.JobID
	verify            bool
	repos             []string
	orgsCache         cache.Cache
	filteredRepoCache *filteredRepoCache
	// repos that _probably_ have wikis (see the comment on hasWiki).
	reposWithWikis map[string]struct{}
	memberCache    map[string]struct{}
	repoSizes      repoSize
	totalRepoSize  int // total size of all repos in kb

	useCustomContentWriter bool
	git                    *git.Git

	scanOptMu   sync.Mutex // protects the scanOptions
	scanOptions *git.ScanOptions

	httpClient      *http.Client
	log             logr.Logger
	conn            *sourcespb.GitHub
	jobPool         *errgroup.Group
	resumeInfoMutex sync.Mutex
	resumeInfoSlice []string
	apiClient       *github.Client

	mu        sync.Mutex // protects the visibility maps
	publicMap map[string]source_metadatapb.Visibility

	includePRComments    bool
	includeIssueComments bool
	includeGistComments  bool
	sources.Progress
	sources.CommonSourceUnitUnmarshaller
}

// WithCustomContentWriter sets the useCustomContentWriter flag on the source.
func (s *Source) WithCustomContentWriter() { s.useCustomContentWriter = true }

func (s *Source) WithScanOptions(scanOptions *git.ScanOptions) {
	s.scanOptions = scanOptions
}

func (s *Source) setScanOptions(base, head string) {
	s.scanOptMu.Lock()
	defer s.scanOptMu.Unlock()
	s.scanOptions.BaseHash = base
	s.scanOptions.HeadHash = head
}

// Ensure the Source satisfies the interfaces at compile time
var _ sources.Source = (*Source)(nil)
var _ sources.SourceUnitUnmarshaller = (*Source)(nil)

var endsWithGithub = regexp.MustCompile(`github\.com/?$`)

// Type returns the type of source.
// It is used for matching source types in configuration and job input.
func (s *Source) Type() sourcespb.SourceType {
	return SourceType
}

func (s *Source) SourceID() sources.SourceID {
	return s.sourceID
}

func (s *Source) JobID() sources.JobID {
	return s.jobID
}

type repoSize struct {
	mu        sync.RWMutex
	repoSizes map[string]int // size in kb of each repo
}

func (r *repoSize) addRepo(repo string, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repoSizes[repo] = size
}

func (r *repoSize) getRepo(repo string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.repoSizes[repo]
}

func newRepoSize() repoSize {
	return repoSize{repoSizes: make(map[string]int)}
}

// filteredRepoCache is a wrapper around cache.Cache that filters out repos
// based on include and exclude globs.
type filteredRepoCache struct {
	cache.Cache
	include, exclude []glob.Glob
}

func (s *Source) newFilteredRepoCache(c cache.Cache, include, exclude []string) *filteredRepoCache {
	includeGlobs := make([]glob.Glob, 0, len(include))
	excludeGlobs := make([]glob.Glob, 0, len(exclude))
	for _, ig := range include {
		g, err := glob.Compile(ig)
		if err != nil {
			s.log.V(1).Info("invalid include glob", "include_value", ig, "err", err)
			continue
		}
		includeGlobs = append(includeGlobs, g)
	}
	for _, eg := range exclude {
		g, err := glob.Compile(eg)
		if err != nil {
			s.log.V(1).Info("invalid exclude glob", "exclude_value", eg, "err", err)
			continue
		}
		excludeGlobs = append(excludeGlobs, g)
	}
	return &filteredRepoCache{Cache: c, include: includeGlobs, exclude: excludeGlobs}
}

// Set overrides the cache.Cache Set method to filter out repos based on
// include and exclude globs.
func (c *filteredRepoCache) Set(key, val string) {
	if c.ignoreRepo(key) {
		return
	}
	if !c.includeRepo(key) {
		return
	}
	c.Cache.Set(key, val)
}

func (c *filteredRepoCache) ignoreRepo(s string) bool {
	for _, g := range c.exclude {
		if g.Match(s) {
			return true
		}
	}
	return false
}

func (c *filteredRepoCache) includeRepo(s string) bool {
	if len(c.include) == 0 {
		return true
	}

	for _, g := range c.include {
		if g.Match(s) {
			return true
		}
	}
	return false
}

// Init returns an initialized GitHub source.
func (s *Source) Init(aCtx context.Context, name string, jobID sources.JobID, sourceID sources.SourceID, verify bool, connection *anypb.Any, concurrency int) error {
	s.log = aCtx.Logger()

	s.name = name
	s.sourceID = sourceID
	s.jobID = jobID
	s.verify = verify
	s.jobPool = &errgroup.Group{}
	s.jobPool.SetLimit(concurrency)

	s.httpClient = common.RetryableHTTPClientTimeout(60)
	s.apiClient = github.NewClient(s.httpClient)

	var conn sourcespb.GitHub
	err := anypb.UnmarshalTo(connection, &conn, proto.UnmarshalOptions{})
	if err != nil {
		return fmt.Errorf("error unmarshalling connection: %w", err)
	}
	s.conn = &conn

	s.filteredRepoCache = s.newFilteredRepoCache(memory.New(),
		append(s.conn.GetRepositories(), s.conn.GetIncludeRepos()...),
		s.conn.GetIgnoreRepos(),
	)
	s.reposWithWikis = make(map[string]struct{})
	s.memberCache = make(map[string]struct{})

	s.repoSizes = newRepoSize()
	s.repos = s.conn.Repositories
	for _, repo := range s.repos {
		r, err := s.normalizeRepo(repo)
		if err != nil {
			aCtx.Logger().Error(err, "invalid repository", "repo", repo)
			continue
		}
		s.filteredRepoCache.Set(repo, r)
	}

	s.includeIssueComments = s.conn.IncludeIssueComments
	s.includePRComments = s.conn.IncludePullRequestComments
	s.includeGistComments = s.conn.IncludeGistComments

	s.orgsCache = memory.New()
	for _, org := range s.conn.Organizations {
		s.orgsCache.Set(org, org)
	}

	// Head or base should only be used with incoming webhooks
	if (len(s.conn.Head) > 0 || len(s.conn.Base) > 0) && len(s.repos) != 1 {
		return fmt.Errorf("cannot specify head or base with multiple repositories")
	}

	err = git.CmdCheck()
	if err != nil {
		return err
	}

	s.publicMap = map[string]source_metadatapb.Visibility{}

	cfg := &git.Config{
		SourceName:   s.name,
		JobID:        s.jobID,
		SourceID:     s.sourceID,
		SourceType:   s.Type(),
		Verify:       s.verify,
		SkipBinaries: conn.GetSkipBinaries(),
		SkipArchives: conn.GetSkipArchives(),
		Concurrency:  concurrency,
		SourceMetadataFunc: func(file, email, commit, timestamp, repository string, line int64) *source_metadatapb.MetaData {
			return &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Commit:     sanitizer.UTF8(commit),
						File:       sanitizer.UTF8(file),
						Email:      sanitizer.UTF8(email),
						Repository: sanitizer.UTF8(repository),
						Link:       giturl.GenerateLink(repository, commit, file, line),
						Timestamp:  sanitizer.UTF8(timestamp),
						Line:       line,
						Visibility: s.visibilityOf(aCtx, repository),
					},
				},
			}
		},
		UseCustomContentWriter: s.useCustomContentWriter,
	}
	s.git = git.NewGit(cfg)

	return nil
}

// Validate is used by enterprise CLI to validate the Github config file.
func (s *Source) Validate(ctx context.Context) []error {
	var (
		errs     []error
		ghClient *github.Client
		err      error
	)
	apiEndpoint := s.conn.Endpoint

	switch cred := s.conn.GetCredential().(type) {
	case *sourcespb.GitHub_BasicAuth:
		s.httpClient.Transport = &github.BasicAuthTransport{
			Username: cred.BasicAuth.Username,
			Password: cred.BasicAuth.Password,
		}
		ghClient, err = createGitHubClient(s.httpClient, apiEndpoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("error creating GitHub client: %+v", err))
		}
	case *sourcespb.GitHub_Unauthenticated:
		ghClient, err = createGitHubClient(s.httpClient, apiEndpoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("error creating GitHub client: %+v", err))
		}
	case *sourcespb.GitHub_Token:
		s.githubToken = cred.Token

		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: s.githubToken},
		)
		s.httpClient.Transport = &oauth2.Transport{
			Base:   s.httpClient.Transport,
			Source: oauth2.ReuseTokenSource(nil, ts),
		}

		ghClient, err = createGitHubClient(s.httpClient, apiEndpoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("error creating GitHub client: %+v", err))
		}
	default:
		errs = append(errs, fmt.Errorf("Invalid configuration given for source. Name: %s, Type: %s", s.name, s.Type()))
	}

	// Run a simple query to check if the client is actually valid
	if ghClient != nil {
		err = checkGitHubConnection(ctx, ghClient)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

func checkGitHubConnection(ctx context.Context, client *github.Client) error {
	_, _, err := client.Users.Get(ctx, "")
	return err
}

func (s *Source) visibilityOf(ctx context.Context, repoURL string) (visibility source_metadatapb.Visibility) {
	// It isn't possible to get the visibility of a wiki.
	// We must use the visibility of the corresponding repository.
	if strings.HasSuffix(repoURL, ".wiki.git") {
		repoURL = strings.TrimSuffix(repoURL, ".wiki.git") + ".git"
	}

	s.mu.Lock()
	visibility, ok := s.publicMap[repoURL]
	s.mu.Unlock()
	if ok {
		return visibility
	}

	visibility = source_metadatapb.Visibility_public
	defer func() {
		s.mu.Lock()
		s.publicMap[repoURL] = visibility
		s.mu.Unlock()
	}()
	logger := s.log.WithValues("repo", repoURL)
	if _, unauthenticated := s.conn.GetCredential().(*sourcespb.GitHub_Unauthenticated); unauthenticated {
		logger.V(3).Info("assuming unauthenticated scan has public visibility")
		return source_metadatapb.Visibility_public
	}
	logger.V(2).Info("Checking public status")
	u, err := url.Parse(repoURL)
	if err != nil {
		logger.Error(err, "Could not parse repository URL.")
		return
	}

	urlPathParts := strings.Split(u.Path, "/")
	switch len(urlPathParts) {
	case 2:
		// Check if repoURL is a gist.
		var gist *github.Gist
		repoName := urlPathParts[1]
		repoName = strings.TrimSuffix(repoName, ".git")
		for {
			gist, _, err = s.apiClient.Gists.Get(ctx, repoName)
			if !s.handleRateLimit(err) {
				break
			}
		}
		if err != nil || gist == nil {
			logger.Error(err, "Could not get Github repository")
			return
		}
		if !(*gist.Public) {
			visibility = source_metadatapb.Visibility_private
		}
	case 3:
		var repo *github.Repository
		owner := urlPathParts[1]
		repoName := urlPathParts[2]
		repoName = strings.TrimSuffix(repoName, ".git")
		for {
			repo, _, err = s.apiClient.Repositories.Get(ctx, owner, repoName)
			if !s.handleRateLimit(err) {
				break
			}
		}
		if err != nil || repo == nil {
			logger.Error(err, "Could not get Github repository")
			return
		}
		if *repo.Private {
			visibility = source_metadatapb.Visibility_private
		}
	default:
		logger.Error(fmt.Errorf("unexpected number of parts"), "RepoURL should split into 2 or 3 parts",
			"got", len(urlPathParts),
		)
	}
	return
}

const cloudEndpoint = "https://api.github.com"

// Chunks emits chunks of bytes over a channel.
func (s *Source) Chunks(ctx context.Context, chunksChan chan *sources.Chunk, targets ...sources.ChunkingTarget) error {
	apiEndpoint := s.conn.Endpoint
	if len(apiEndpoint) == 0 || endsWithGithub.MatchString(apiEndpoint) {
		apiEndpoint = cloudEndpoint
	}

	// If targets are provided, we're only scanning the data in those targets.
	// Otherwise, we're scanning all data.
	// This allows us to only scan the commit where a vulnerability was found.
	if len(targets) > 0 {
		return s.scanTargets(ctx, targets, chunksChan)
	}

	// Reset consumption and rate limit metrics on each run.
	githubNumRateLimitEncountered.WithLabelValues(s.name).Set(0)
	githubSecondsSpentRateLimited.WithLabelValues(s.name).Set(0)
	githubReposScanned.WithLabelValues(s.name).Set(0)

	installationClient, err := s.enumerate(ctx, apiEndpoint)
	if err != nil {
		return err
	}

	return s.scan(ctx, installationClient, chunksChan)
}

func (s *Source) enumerate(ctx context.Context, apiEndpoint string) (*github.Client, error) {
	var (
		installationClient *github.Client
		err                error
	)

	switch cred := s.conn.GetCredential().(type) {
	case *sourcespb.GitHub_BasicAuth:
		if err = s.enumerateBasicAuth(ctx, apiEndpoint, cred.BasicAuth); err != nil {
			return nil, err
		}
	case *sourcespb.GitHub_Unauthenticated:
		s.enumerateUnauthenticated(ctx, apiEndpoint)
	case *sourcespb.GitHub_Token:
		if err = s.enumerateWithToken(ctx, apiEndpoint, cred.Token); err != nil {
			return nil, err
		}
	case *sourcespb.GitHub_GithubApp:
		if installationClient, err = s.enumerateWithApp(ctx, apiEndpoint, cred.GithubApp); err != nil {
			return nil, err
		}
	default:
		// TODO: move this error to Init
		return nil, fmt.Errorf("Invalid configuration given for source. Name: %s, Type: %s", s.name, s.Type())
	}

	s.repos = make([]string, 0, s.filteredRepoCache.Count())
	for _, repo := range s.filteredRepoCache.Values() {
		r, ok := repo.(string)
		if !ok {
			ctx.Logger().Error(fmt.Errorf("type assertion failed"), "unexpected value in cache", "repo", repo)
			continue
		}
		s.repos = append(s.repos, r)
	}
	githubReposEnumerated.WithLabelValues(s.name).Set(float64(len(s.repos)))
	s.log.Info("Completed enumeration", "num_repos", len(s.repos), "num_orgs", s.orgsCache.Count(), "num_members", len(s.memberCache))

	// We must sort the repos so we can resume later if necessary.
	sort.Strings(s.repos)
	return installationClient, nil
}

func (s *Source) enumerateBasicAuth(ctx context.Context, apiEndpoint string, basicAuth *credentialspb.BasicAuth) error {
	s.httpClient.Transport = &github.BasicAuthTransport{
		Username: basicAuth.Username,
		Password: basicAuth.Password,
	}
	ghClient, err := createGitHubClient(s.httpClient, apiEndpoint)
	if err != nil {
		s.log.Error(err, "error creating GitHub client")
	}
	s.apiClient = ghClient

	for _, org := range s.orgsCache.Keys() {
		if err := s.getReposByOrg(ctx, org); err != nil {
			s.log.Error(err, "error fetching repos for org or user")
		}
	}

	return nil
}

func (s *Source) enumerateUnauthenticated(ctx context.Context, apiEndpoint string) {
	ghClient, err := createGitHubClient(s.httpClient, apiEndpoint)
	if err != nil {
		s.log.Error(err, "error creating GitHub client")
	}
	s.apiClient = ghClient
	if s.orgsCache.Count() > unauthGithubOrgRateLimt {
		s.log.Info("You may experience rate limiting when using the unauthenticated GitHub api. Consider using an authenticated scan instead.")
	}

	for _, org := range s.orgsCache.Keys() {
		if err := s.getReposByOrg(ctx, org); err != nil {
			s.log.Error(err, "error fetching repos for org")
		}

		// We probably don't need to do this, since getting repos by org makes more sense?
		if err := s.getReposByUser(ctx, org); err != nil {
			s.log.Error(err, "error fetching repos for user")
		}

		if s.conn.ScanUsers {
			s.log.Info("Enumerating unauthenticated does not support scanning organization members")
		}
	}
}

func (s *Source) enumerateWithToken(ctx context.Context, apiEndpoint, token string) error {
	// Needed for clones.
	s.githubToken = token

	// Needed to list repos.
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	s.httpClient.Transport = &oauth2.Transport{
		Base:   s.httpClient.Transport,
		Source: oauth2.ReuseTokenSource(nil, ts),
	}

	// If we're using public GitHub, make a regular client.
	// Otherwise, make an enterprise client.
	ghClient, err := createGitHubClient(s.httpClient, apiEndpoint)
	if err != nil {
		s.log.Error(err, "error creating GitHub client")
	}
	s.apiClient = ghClient

	// TODO: this should support scanning users too

	specificScope := false

	if len(s.repos) > 0 {
		specificScope = true
	}

	var (
		ghUser *github.User
	)

	ctx.Logger().V(1).Info("Enumerating with token", "endpoint", apiEndpoint)
	for {
		ghUser, _, err = s.apiClient.Users.Get(ctx, "")
		if s.handleRateLimit(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("error getting user: %w", err)
		}
		break
	}

	if s.orgsCache.Count() > 0 {
		specificScope = true
		for _, org := range s.orgsCache.Keys() {
			logger := s.log.WithValues("org", org)
			if err := s.getReposByOrg(ctx, org); err != nil {
				logger.Error(err, "error fetching repos for org")
			}

			if s.conn.ScanUsers {
				err := s.addMembersByOrg(ctx, org)
				if err != nil {
					logger.Error(err, "Unable to add members by org")
					continue
				}
			}
		}
	}

	// If no scope was provided, enumerate them.
	if !specificScope {
		if err := s.getReposByUser(ctx, ghUser.GetLogin()); err != nil {
			s.log.Error(err, "error fetching repos by user")
		}

		isGHE := !strings.EqualFold(apiEndpoint, cloudEndpoint)
		if isGHE {
			s.addAllVisibleOrgs(ctx)
		} else {
			// Scan for orgs is default with a token. GitHub App enumerates the repositories
			// that were assigned to it in GitHub App settings.
			s.addOrgsByUser(ctx, ghUser.GetLogin())
		}

		for _, org := range s.orgsCache.Keys() {
			logger := s.log.WithValues("org", org)
			if err := s.getReposByOrg(ctx, org); err != nil {
				logger.Error(err, "error fetching repos by org")
			}

			if err := s.getReposByUser(ctx, ghUser.GetLogin()); err != nil {
				logger.Error(err, "error fetching repos by user")
			}

			if s.conn.ScanUsers {
				err := s.addMembersByOrg(ctx, org)
				if err != nil {
					logger.Error(err, "Unable to add members by org for org")
				}
			}
		}

		// If we enabled ScanUsers above, we've already added the gists for the current user and users from the orgs.
		// So if we don't have ScanUsers enabled, add the user gists as normal.
		if err := s.addUserGistsToCache(ctx, ghUser.GetLogin()); err != nil {
			s.log.Error(err, "error fetching gists", "user", ghUser.GetLogin())
		}

		return nil
	}

	if s.conn.ScanUsers {
		s.log.Info("Adding repos", "members", len(s.memberCache), "orgs", s.orgsCache.Count())
		s.addReposForMembers(ctx)
		return nil
	}

	return nil
}

func (s *Source) enumerateWithApp(ctx context.Context, apiEndpoint string, app *credentialspb.GitHubApp) (installationClient *github.Client, err error) {
	installationID, err := strconv.ParseInt(app.InstallationId, 10, 64)
	if err != nil {
		return nil, err
	}

	appID, err := strconv.ParseInt(app.AppId, 10, 64)
	if err != nil {
		return nil, err
	}

	// This client is required to create installation tokens for cloning.
	// Otherwise, the required JWT is not in the request for the token :/
	// This client uses the source's original HTTP transport, so make sure
	// to build it before modifying that transport (such as is done during
	// the creation of the other API client below).
	appItr, err := ghinstallation.NewAppsTransport(
		s.httpClient.Transport,
		appID,
		[]byte(app.PrivateKey))
	if err != nil {
		return nil, err
	}
	appItr.BaseURL = apiEndpoint

	// Does this need to be separate from |s.httpClient|?
	instHTTPClient := common.RetryableHTTPClientTimeout(60)
	instHTTPClient.Transport = appItr
	installationClient, err = github.NewClient(instHTTPClient).WithEnterpriseURLs(apiEndpoint, apiEndpoint)
	if err != nil {
		return nil, err
	}

	// This client is used for most APIs.
	itr, err := ghinstallation.New(
		s.httpClient.Transport,
		appID,
		installationID,
		[]byte(app.PrivateKey))
	if err != nil {
		return nil, err
	}
	itr.BaseURL = apiEndpoint

	s.httpClient.Transport = itr
	s.apiClient, err = github.NewClient(s.httpClient).WithEnterpriseURLs(apiEndpoint, apiEndpoint)
	if err != nil {
		return nil, err
	}

	// If no repos were provided, enumerate them.
	if len(s.repos) == 0 {
		if err = s.getReposByApp(ctx); err != nil {
			return nil, err
		}

		// Check if we need to find user repos.
		if s.conn.ScanUsers {
			err := s.addMembersByApp(ctx, installationClient)
			if err != nil {
				return nil, err
			}
			s.log.Info("Scanning repos", "org_members", len(s.memberCache))
			for member := range s.memberCache {
				logger := s.log.WithValues("member", member)
				if err := s.getReposByUser(ctx, member); err != nil {
					logger.Error(err, "error fetching gists by user")
				}
				if err := s.getReposByUser(ctx, member); err != nil {
					logger.Error(err, "error fetching repos by user")
				}
			}
		}
	}

	return installationClient, nil
}

func createGitHubClient(httpClient *http.Client, apiEndpoint string) (*github.Client, error) {
	// If we're using public GitHub, make a regular client.
	// Otherwise, make an enterprise client.
	if strings.EqualFold(apiEndpoint, cloudEndpoint) {
		return github.NewClient(httpClient), nil
	}

	return github.NewClient(httpClient).WithEnterpriseURLs(apiEndpoint, apiEndpoint)
}

func (s *Source) scan(ctx context.Context, installationClient *github.Client, chunksChan chan *sources.Chunk) error {
	var scannedCount uint64

	s.log.V(2).Info("Found repos to scan", "count", len(s.repos))

	// If there is resume information available, limit this scan to only the repos that still need scanning.
	reposToScan, progressIndexOffset := sources.FilterReposToResume(s.repos, s.GetProgress().EncodedResumeInfo)
	s.repos = reposToScan

	scanErrs := sources.NewScanErrors()
	// Setup scan options if it wasn't provided.
	if s.scanOptions == nil {
		s.scanOptions = &git.ScanOptions{}
	}

	for i, repoURL := range s.repos {
		i, repoURL := i, repoURL
		s.jobPool.Go(func() error {
			if common.IsDone(ctx) {
				return nil
			}

			// TODO: set progress complete is being called concurrently with i
			s.setProgressCompleteWithRepo(i, progressIndexOffset, repoURL)
			// Ensure the repo is removed from the resume info after being scanned.
			defer func(s *Source, repoURL string) {
				s.resumeInfoMutex.Lock()
				defer s.resumeInfoMutex.Unlock()
				s.resumeInfoSlice = sources.RemoveRepoFromResumeInfo(s.resumeInfoSlice, repoURL)
			}(s, repoURL)

			if !strings.HasSuffix(repoURL, ".git") {
				scanErrs.Add(fmt.Errorf("repo %s does not end in .git", repoURL))
				return nil
			}

			// Scan the repository
			repoCtx := context.WithValues(ctx, "repo", repoURL)
			duration, err := s.cloneAndScanRepo(repoCtx, installationClient, repoURL, chunksChan)
			if err != nil {
				scanErrs.Add(err)
				return nil
			}

			// Scan the wiki, if enabled, and the repo has one.
			if s.conn.IncludeWikis {
				if _, ok := s.reposWithWikis[repoURL]; ok {
					wikiURL := strings.TrimSuffix(repoURL, ".git") + ".wiki.git"
					wikiCtx := context.WithValue(ctx, "repo", wikiURL)

					_, err := s.cloneAndScanRepo(wikiCtx, installationClient, wikiURL, chunksChan)
					if err != nil {
						scanErrs.Add(err)
						// Don't return, it still might be possible to scan comments.
					}
				}
			}

			// Scan comments, if enabled.
			if s.includeGistComments || s.includeIssueComments || s.includePRComments {
				if err = s.scanComments(ctx, repoURL, chunksChan); err != nil {
					scanErrs.Add(fmt.Errorf("error scanning comments in repo %s: %w", repoURL, err))
					return nil
				}
			}

			ctx.Logger().V(2).Info(fmt.Sprintf("scanned %d/%d repos", scannedCount, len(s.repos)), "duration_seconds", duration)
			githubReposScanned.WithLabelValues(s.name).Inc()
			atomic.AddUint64(&scannedCount, 1)
			return nil
		})
	}

	_ = s.jobPool.Wait()
	if scanErrs.Count() > 0 {
		s.log.V(0).Info("failed to scan some repositories", "error_count", scanErrs.Count(), "errors", scanErrs)
	}
	s.SetProgressComplete(len(s.repos), len(s.repos), "Completed Github scan", "")

	return nil
}

func (s *Source) cloneAndScanRepo(ctx context.Context, client *github.Client, repoURL string, chunksChan chan *sources.Chunk) (time.Duration, error) {
	var duration time.Duration

	ctx.Logger().V(2).Info("attempting to clone repo")
	path, repo, err := s.cloneRepo(ctx, repoURL, client)
	if err != nil {
		return duration, fmt.Errorf("error cloning repo %s: %w", repoURL, err)
	}
	defer os.RemoveAll(path)

	// TODO: Can this be set once or does it need to be set on every iteration? Is |s.scanOptions| set every clone?
	s.setScanOptions(s.conn.Base, s.conn.Head)

	// Repo size is not collected for wikis.
	var logger logr.Logger
	if !strings.HasSuffix(repoURL, ".wiki.git") {
		repoSize := s.repoSizes.getRepo(repoURL)
		logger = ctx.Logger().WithValues("repo_size_kb", repoSize)
	} else {
		logger = ctx.Logger()
	}
	logger.V(2).Info("scanning repo")

	start := time.Now()
	if err = s.git.ScanRepo(ctx, repo, path, s.scanOptions, sources.ChanReporter{Ch: chunksChan}); err != nil {
		return duration, fmt.Errorf("error scanning repo %s: %w", repoURL, err)
	}
	duration = time.Since(start)
	return duration, nil
}

var (
	rateLimitMu         sync.RWMutex
	rateLimitResumeTime time.Time
)

// handleRateLimit returns true if a rate limit was handled
//
// Unauthenticated users have a rate limit of 60 requests per hour.
// Authenticated users have a rate limit of 5,000 requests per hour,
// however, certain actions are subject to a stricter "secondary" limit.
// https://docs.github.com/en/rest/overview/rate-limits-for-the-rest-api
func (s *Source) handleRateLimit(errIn error) bool {
	if errIn == nil {
		return false
	}

	rateLimitMu.RLock()
	resumeTime := rateLimitResumeTime
	rateLimitMu.RUnlock()

	var retryAfter time.Duration
	if resumeTime.IsZero() || time.Now().After(resumeTime) {
		rateLimitMu.Lock()

		var (
			now = time.Now()

			// GitHub has both primary (RateLimit) and secondary (AbuseRateLimit) errors.
			limitType  string
			rateLimit  *github.RateLimitError
			abuseLimit *github.AbuseRateLimitError
		)
		if errors.As(errIn, &rateLimit) {
			limitType = "primary"
			rate := rateLimit.Rate
			if rate.Remaining == 0 { // TODO: Will we ever receive a |RateLimitError| when remaining > 0?
				retryAfter = rate.Reset.Sub(now)
			}
		} else if errors.As(errIn, &abuseLimit) {
			limitType = "secondary"
			retryAfter = abuseLimit.GetRetryAfter()
		} else {
			rateLimitMu.Unlock()
			return false
		}

		jitter := time.Duration(rand.Intn(10)+1) * time.Second
		if retryAfter > 0 {
			retryAfter = retryAfter + jitter
			rateLimitResumeTime = now.Add(retryAfter)
			s.log.V(0).Info(fmt.Sprintf("exceeded %s rate limit", limitType), "retry_after", retryAfter.String(), "resume_time", rateLimitResumeTime.Format(time.RFC3339))
		} else {
			retryAfter = (5 * time.Minute) + jitter
			rateLimitResumeTime = now.Add(retryAfter)
			// TODO: Use exponential backoff instead of static retry time.
			s.log.V(0).Error(errIn, "unexpected rate limit error", "retry_after", retryAfter.String(), "resume_time", rateLimitResumeTime.Format(time.RFC3339))
		}

		rateLimitMu.Unlock()
	} else {
		retryAfter = time.Until(resumeTime)
	}

	githubNumRateLimitEncountered.WithLabelValues(s.name).Inc()
	time.Sleep(retryAfter)
	githubSecondsSpentRateLimited.WithLabelValues(s.name).Add(retryAfter.Seconds())
	return true
}

func (s *Source) addReposForMembers(ctx context.Context) {
	s.log.Info("Fetching repos from members", "members", len(s.memberCache))
	for member := range s.memberCache {
		if err := s.addUserGistsToCache(ctx, member); err != nil {
			s.log.Info("Unable to fetch gists by user", "user", member, "error", err)
		}
		if err := s.getReposByUser(ctx, member); err != nil {
			s.log.Info("Unable to fetch repos by user", "user", member, "error", err)
		}
	}
}

// addUserGistsToCache collects all the gist urls for a given user,
// and adds them to the filteredRepoCache.
func (s *Source) addUserGistsToCache(ctx context.Context, user string) error {
	gistOpts := &github.GistListOptions{}
	logger := s.log.WithValues("user", user)
	for {
		gists, res, err := s.apiClient.Gists.List(ctx, user, gistOpts)
		if s.handleRateLimit(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("could not list gists for user %s: %w", user, err)
		}
		for _, gist := range gists {
			s.filteredRepoCache.Set(gist.GetID(), gist.GetGitPullURL())
		}
		if res == nil || res.NextPage == 0 {
			break
		}
		logger.V(2).Info("Listed gists", "page", gistOpts.Page, "last_page", res.LastPage)
		gistOpts.Page = res.NextPage
	}
	return nil
}

func (s *Source) addMembersByApp(ctx context.Context, installationClient *github.Client) error {
	opts := &github.ListOptions{
		PerPage: membersAppPagination,
	}

	// TODO: Check rate limit for this call.
	installs, _, err := installationClient.Apps.ListInstallations(ctx, opts)
	if err != nil {
		return fmt.Errorf("could not enumerate installed orgs: %w", err)
	}

	for _, org := range installs {
		if org.Account.GetType() != "Organization" {
			continue
		}
		if err := s.addMembersByOrg(ctx, *org.Account.Login); err != nil {
			return err
		}
	}

	return nil
}

func (s *Source) addAllVisibleOrgs(ctx context.Context) {
	s.log.V(2).Info("enumerating all visible organizations on GHE")
	// Enumeration on this endpoint does not use pages it uses a since ID.
	// The endpoint will return organizations with an ID greater than the given since ID.
	// Empty org response is our cue to break the enumeration loop.
	orgOpts := &github.OrganizationsListOptions{
		Since: 0,
		ListOptions: github.ListOptions{
			PerPage: defaultPagination,
		},
	}
	for {
		orgs, _, err := s.apiClient.Organizations.ListAll(ctx, orgOpts)
		if s.handleRateLimit(err) {
			continue
		}
		if err != nil {
			s.log.Error(err, "could not list all organizations")
			return
		}
		if len(orgs) == 0 {
			break
		}
		lastOrgID := *orgs[len(orgs)-1].ID
		s.log.V(2).Info(fmt.Sprintf("listed organization IDs %d through %d", orgOpts.Since, lastOrgID))
		orgOpts.Since = lastOrgID

		for _, org := range orgs {
			var name string
			switch {
			case org.Name != nil:
				name = *org.Name
			case org.Login != nil:
				name = *org.Login
			default:
				continue
			}
			s.orgsCache.Set(name, name)
			s.log.V(2).Info("adding organization for repository enumeration", "id", org.ID, "name", name)
		}
	}
}

func (s *Source) addOrgsByUser(ctx context.Context, user string) {
	orgOpts := &github.ListOptions{
		PerPage: defaultPagination,
	}
	logger := s.log.WithValues("user", user)
	for {
		orgs, resp, err := s.apiClient.Organizations.List(ctx, "", orgOpts)
		if handled := s.handleRateLimit(err); handled {
			continue
		}
		if err != nil {
			logger.Error(err, "Could not list organizations")
			return
		}
		if resp == nil {
			break
		}
		logger.V(2).Info("Listed orgs", "page", orgOpts.Page, "last_page", resp.LastPage)
		for _, org := range orgs {
			if org.Login == nil {
				continue
			}
			s.orgsCache.Set(*org.Login, *org.Login)
		}
		if resp.NextPage == 0 {
			break
		}
		orgOpts.Page = resp.NextPage
	}
}

func (s *Source) addMembersByOrg(ctx context.Context, org string) error {
	opts := &github.ListMembersOptions{
		PublicOnly: false,
		ListOptions: github.ListOptions{
			PerPage: membersAppPagination,
		},
	}

	logger := s.log.WithValues("org", org)
	for {
		members, res, err := s.apiClient.Organizations.ListMembers(ctx, org, opts)
		if s.handleRateLimit(err) {
			continue
		}
		if err != nil || len(members) == 0 {
			return fmt.Errorf("could not list organization members: account may not have access to list organization members %w", err)
		}
		if res == nil {
			break
		}
		logger.V(2).Info("Listed members", "page", opts.Page, "last_page", res.LastPage)
		for _, m := range members {
			usr := m.Login
			if usr == nil || *usr == "" {
				continue
			}
			if _, ok := s.memberCache[*usr]; !ok {
				s.memberCache[*usr] = struct{}{}
			}
		}
		if res.NextPage == 0 {
			break
		}
		opts.Page = res.NextPage
	}

	return nil
}

// setProgressCompleteWithRepo calls the s.SetProgressComplete after safely setting up the encoded resume info string.
func (s *Source) setProgressCompleteWithRepo(index int, offset int, repoURL string) {
	s.resumeInfoMutex.Lock()
	defer s.resumeInfoMutex.Unlock()

	// Add the repoURL to the resume info slice.
	s.resumeInfoSlice = append(s.resumeInfoSlice, repoURL)
	sort.Strings(s.resumeInfoSlice)

	// Make the resume info string from the slice.
	encodedResumeInfo := sources.EncodeResumeInfo(s.resumeInfoSlice)

	s.SetProgressComplete(index+offset, len(s.repos)+offset, fmt.Sprintf("Repo: %s", repoURL), encodedResumeInfo)
}

const initialPage = 1 // page to start listing from

func (s *Source) scanComments(ctx context.Context, repoPath string, chunksChan chan *sources.Chunk) error {
	// Support ssh and https URLs
	repoURL, err := git.GitURLParse(repoPath)
	if err != nil {
		return err
	}

	trimmedURL := removeURLAndSplit(repoURL.String())
	if repoURL.Host == "gist.github.com" && s.includeGistComments {
		return s.processGistComments(ctx, repoPath, trimmedURL, repoURL, chunksChan)
	}
	return s.processRepoComments(ctx, repoPath, trimmedURL, repoURL, chunksChan)
}

func (s *Source) processGistComments(ctx context.Context, repoPath string, trimmedURL []string, repoURL *url.URL, chunksChan chan *sources.Chunk) error {
	ctx.Logger().V(2).Info("scanning github gist comments", "repository", repoPath)
	// GitHub Gist URL.
	gistID, err := extractGistID(trimmedURL)
	if err != nil {
		return err
	}

	options := &github.ListOptions{
		PerPage: defaultPagination,
		Page:    initialPage,
	}
	for {
		comments, _, err := s.apiClient.Gists.ListComments(ctx, gistID, options)
		if s.handleRateLimit(err) {
			break
		}
		if err != nil {
			return err
		}

		if err = s.chunkGistComments(ctx, repoURL.String(), comments, chunksChan); err != nil {
			return err
		}

		options.Page++
		if len(comments) < options.PerPage {
			break
		}
	}
	return nil
}

func extractGistID(url []string) (string, error) {
	if len(url) < 2 || len(url) > 3 {
		return "", fmt.Errorf("failed to parse Gist URL: length of trimmedURL should be 2 or 3")
	}
	return url[len(url)-1], nil
}

// Note: these can't be consts because the address is needed when using with the GitHub library.
var (
	// sortType defines the criteria for sorting comments.
	// By default comments are sorted by their creation date.
	sortType = "created"
	// directionType defines the direction of sorting.
	// "desc" means comments will be sorted in descending order, showing the latest comments first.
	directionType = "desc"
	// allComments is a placeholder for specifying the comment ID to start listing from.
	// A value of 0 means that all comments will be listed.
	allComments = 0
	// state of "all" for the ListByRepo captures both open and closed issues.
	state = "all"
)

type repoInfo struct {
	owner      string
	repo       string
	repoPath   string
	visibility source_metadatapb.Visibility
}

func (s *Source) processRepoComments(ctx context.Context, repoPath string, trimmedURL []string, repoURL *url.URL, chunksChan chan *sources.Chunk) error {
	// Normal repository URL (https://github.com/<owner>/<repo>).
	if len(trimmedURL) < 3 {
		return fmt.Errorf("url missing owner and/or repo: '%s'", repoURL.String())
	}
	owner := trimmedURL[1]
	repo := trimmedURL[2]

	if !(s.includeIssueComments || s.includePRComments) {
		return nil
	}

	repoInfo := repoInfo{
		owner:      owner,
		repo:       repo,
		repoPath:   repoPath,
		visibility: s.visibilityOf(ctx, repoPath),
	}

	if s.includeIssueComments {
		ctx.Logger().V(2).Info("scanning github issues", "repository", repoInfo.repoPath)
		if err := s.processIssues(ctx, repoInfo, chunksChan); err != nil {
			return err
		}
		if err := s.processIssueComments(ctx, repoInfo, chunksChan); err != nil {
			return err
		}
	}

	if s.includePRComments {
		ctx.Logger().V(2).Info("scanning github pull requests", "repository", repoInfo.repoPath)
		if err := s.processPRs(ctx, repoInfo, chunksChan); err != nil {
			return err
		}
		if err := s.processPRComments(ctx, repoInfo, chunksChan); err != nil {
			return err
		}
	}

	return nil

}

func (s *Source) processIssues(ctx context.Context, info repoInfo, chunksChan chan *sources.Chunk) error {
	bodyTextsOpts := &github.IssueListByRepoOptions{
		Sort:      sortType,
		Direction: directionType,
		State:     state,
		ListOptions: github.ListOptions{
			PerPage: defaultPagination,
			Page:    initialPage,
		},
	}

	for {
		issues, _, err := s.apiClient.Issues.ListByRepo(ctx, info.owner, info.repo, bodyTextsOpts)
		if s.handleRateLimit(err) {
			break
		}

		if err != nil {
			return err
		}

		if err = s.chunkIssues(ctx, info, issues, chunksChan); err != nil {
			return err
		}

		bodyTextsOpts.ListOptions.Page++

		if len(issues) < defaultPagination {
			break
		}
	}
	return nil
}

func (s *Source) processIssueComments(ctx context.Context, info repoInfo, chunksChan chan *sources.Chunk) error {
	issueOpts := &github.IssueListCommentsOptions{
		Sort:      &sortType,
		Direction: &directionType,
		ListOptions: github.ListOptions{
			PerPage: defaultPagination,
			Page:    initialPage,
		},
	}

	for {
		issueComments, _, err := s.apiClient.Issues.ListComments(ctx, info.owner, info.repo, allComments, issueOpts)
		if s.handleRateLimit(err) {
			break
		}

		if err != nil {
			return err
		}

		if err = s.chunkIssueComments(ctx, info, issueComments, chunksChan); err != nil {
			return err
		}

		issueOpts.ListOptions.Page++

		if len(issueComments) < defaultPagination {
			break
		}
	}
	return nil
}

func (s *Source) processPRs(ctx context.Context, info repoInfo, chunksChan chan *sources.Chunk) error {
	prOpts := &github.PullRequestListOptions{
		Sort:      sortType,
		Direction: directionType,
		State:     state,
		ListOptions: github.ListOptions{
			PerPage: defaultPagination,
			Page:    initialPage,
		},
	}

	for {
		prs, _, err := s.apiClient.PullRequests.List(ctx, info.owner, info.repo, prOpts)
		if s.handleRateLimit(err) {
			break
		}

		if err != nil {
			return err
		}

		if err = s.chunkPullRequests(ctx, info, prs, chunksChan); err != nil {
			return err
		}

		prOpts.ListOptions.Page++

		if len(prs) < defaultPagination {
			break
		}
	}
	return nil
}

func (s *Source) processPRComments(ctx context.Context, info repoInfo, chunksChan chan *sources.Chunk) error {
	prOpts := &github.PullRequestListCommentsOptions{
		Sort:      sortType,
		Direction: directionType,
		ListOptions: github.ListOptions{
			PerPage: defaultPagination,
			Page:    initialPage,
		},
	}

	for {
		prComments, _, err := s.apiClient.PullRequests.ListComments(ctx, info.owner, info.repo, allComments, prOpts)
		if s.handleRateLimit(err) {
			break
		}

		if err != nil {
			return err
		}

		if err = s.chunkPullRequestComments(ctx, info, prComments, chunksChan); err != nil {
			return err
		}

		prOpts.ListOptions.Page++

		if len(prComments) < defaultPagination {
			break
		}
	}
	return nil
}

func (s *Source) chunkIssues(ctx context.Context, repoInfo repoInfo, issues []*github.Issue, chunksChan chan *sources.Chunk) error {
	for _, issue := range issues {

		// Skip pull requests since covered by processPRs.
		if issue.IsPullRequest() {
			continue
		}

		// Create chunk and send it to the channel.
		chunk := &sources.Chunk{
			SourceName: s.name,
			SourceID:   s.SourceID(),
			JobID:      s.JobID(),
			SourceType: s.Type(),
			SourceMetadata: &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Link:       sanitizer.UTF8(issue.GetHTMLURL()),
						Username:   sanitizer.UTF8(issue.GetUser().GetLogin()),
						Email:      sanitizer.UTF8(issue.GetUser().GetEmail()),
						Repository: sanitizer.UTF8(repoInfo.repo),
						Timestamp:  sanitizer.UTF8(issue.GetCreatedAt().String()),
						Visibility: repoInfo.visibility,
					},
				},
			},
			Data:   []byte(sanitizer.UTF8(issue.GetTitle() + "\n" + issue.GetBody())),
			Verify: s.verify,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunksChan <- chunk:
		}
	}
	return nil
}

func (s *Source) chunkIssueComments(ctx context.Context, repoInfo repoInfo, comments []*github.IssueComment, chunksChan chan *sources.Chunk) error {
	for _, comment := range comments {
		// Create chunk and send it to the channel.
		chunk := &sources.Chunk{
			SourceName: s.name,
			SourceID:   s.SourceID(),
			JobID:      s.JobID(),
			SourceType: s.Type(),
			SourceMetadata: &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Link:       sanitizer.UTF8(comment.GetHTMLURL()),
						Username:   sanitizer.UTF8(comment.GetUser().GetLogin()),
						Email:      sanitizer.UTF8(comment.GetUser().GetEmail()),
						Repository: sanitizer.UTF8(repoInfo.repo),
						Timestamp:  sanitizer.UTF8(comment.GetCreatedAt().String()),
						Visibility: repoInfo.visibility,
					},
				},
			},
			Data:   []byte(sanitizer.UTF8(comment.GetBody())),
			Verify: s.verify,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunksChan <- chunk:
		}
	}
	return nil
}

func (s *Source) chunkPullRequestComments(ctx context.Context, repoInfo repoInfo, comments []*github.PullRequestComment, chunksChan chan *sources.Chunk) error {
	for _, comment := range comments {
		// Create chunk and send it to the channel.
		chunk := &sources.Chunk{
			SourceName: s.name,
			SourceID:   s.SourceID(),
			SourceType: s.Type(),
			JobID:      s.JobID(),
			SourceMetadata: &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Link:       sanitizer.UTF8(comment.GetHTMLURL()),
						Username:   sanitizer.UTF8(comment.GetUser().GetLogin()),
						Email:      sanitizer.UTF8(comment.GetUser().GetEmail()),
						Repository: sanitizer.UTF8(repoInfo.repo),
						Timestamp:  sanitizer.UTF8(comment.GetCreatedAt().String()),
						Visibility: repoInfo.visibility,
					},
				},
			},
			Data:   []byte(sanitizer.UTF8(comment.GetBody())),
			Verify: s.verify,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunksChan <- chunk:
		}
	}
	return nil
}

func (s *Source) chunkPullRequests(ctx context.Context, repoInfo repoInfo, prs []*github.PullRequest, chunksChan chan *sources.Chunk) error {
	for _, pr := range prs {
		// Create chunk and send it to the channel.
		chunk := &sources.Chunk{
			SourceName: s.name,
			SourceID:   s.SourceID(),
			SourceType: s.Type(),
			JobID:      s.JobID(),
			SourceMetadata: &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Link:       sanitizer.UTF8(pr.GetHTMLURL()),
						Username:   sanitizer.UTF8(pr.GetUser().GetLogin()),
						Email:      sanitizer.UTF8(pr.GetUser().GetEmail()),
						Repository: sanitizer.UTF8(repoInfo.repo),
						Timestamp:  sanitizer.UTF8(pr.GetCreatedAt().String()),
						Visibility: repoInfo.visibility,
					},
				},
			},
			Data:   []byte(sanitizer.UTF8(pr.GetTitle() + "\n" + pr.GetBody())),
			Verify: s.verify,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunksChan <- chunk:
		}
	}
	return nil
}

func (s *Source) chunkGistComments(ctx context.Context, gistUrl string, comments []*github.GistComment, chunksChan chan *sources.Chunk) error {
	for _, comment := range comments {
		// Create chunk and send it to the channel.
		chunk := &sources.Chunk{
			SourceName: s.name,
			SourceID:   s.SourceID(),
			SourceType: s.Type(),
			JobID:      s.JobID(),
			SourceMetadata: &source_metadatapb.MetaData{
				Data: &source_metadatapb.MetaData_Github{
					Github: &source_metadatapb.Github{
						Link:       sanitizer.UTF8(comment.GetURL()),
						Username:   sanitizer.UTF8(comment.GetUser().GetLogin()),
						Email:      sanitizer.UTF8(comment.GetUser().GetEmail()),
						Repository: sanitizer.UTF8(gistUrl),
						Timestamp:  sanitizer.UTF8(comment.GetCreatedAt().String()),
						// TODO: Fetching this requires making an additional API call. We may want to include this in the future.
						// Visibility: s.visibilityOf(ctx, repoPath),
					},
				},
			},
			Data:   []byte(sanitizer.UTF8(comment.GetBody())),
			Verify: s.verify,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunksChan <- chunk:
		}
	}
	return nil
}

func (s *Source) scanTargets(ctx context.Context, targets []sources.ChunkingTarget, chunksChan chan *sources.Chunk) error {
	for _, tgt := range targets {
		if err := s.scanTarget(ctx, tgt, chunksChan); err != nil {
			ctx.Logger().Error(err, "error scanning target")
		}
	}

	return nil
}

func (s *Source) scanTarget(ctx context.Context, target sources.ChunkingTarget, chunksChan chan *sources.Chunk) error {
	metaType, ok := target.QueryCriteria.GetData().(*source_metadatapb.MetaData_Github)
	if !ok {
		return fmt.Errorf("unable to cast metadata type for targeted scan")
	}
	meta := metaType.Github

	u, err := url.Parse(meta.GetLink())
	if err != nil {
		return fmt.Errorf("unable to parse GitHub URL: %w", err)
	}

	// The owner is the second segment and the repo is the third segment of the path.
	// Ex: https://github.com/owner/repo/.....
	segments := strings.Split(u.Path, "/")
	if len(segments) < 3 {
		return fmt.Errorf("invalid GitHub URL")
	}

	qry := commitQuery{
		repo:     segments[2],
		owner:    segments[1],
		sha:      meta.GetCommit(),
		filename: meta.GetFile(),
	}
	res, err := s.getDiffForFileInCommit(ctx, qry)
	if err != nil {
		return err
	}
	chunk := &sources.Chunk{
		SourceType: s.Type(),
		SourceName: s.name,
		SourceID:   s.SourceID(),
		JobID:      s.JobID(),
		SecretID:   target.SecretID,
		Data:       []byte(res),
		SourceMetadata: &source_metadatapb.MetaData{
			Data: &source_metadatapb.MetaData_Github{Github: meta},
		},
		Verify: s.verify,
	}

	return common.CancellableWrite(ctx, chunksChan, chunk)
}

func removeURLAndSplit(url string) []string {
	trimmedURL := strings.TrimPrefix(url, "https://")
	trimmedURL = strings.TrimSuffix(trimmedURL, ".git")
	splitURL := strings.Split(trimmedURL, "/")

	return splitURL
}
