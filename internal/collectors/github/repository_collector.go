package github

import (
	"context"
	"fmt"
	"github.com/Legit-Labs/legitify/internal/collectors"
	"github.com/Legit-Labs/legitify/internal/common/types"
	"github.com/Legit-Labs/legitify/internal/context_utils"
	"github.com/Legit-Labs/legitify/internal/scorecard"
	"log"
	"net/http"

	"github.com/Legit-Labs/legitify/internal/common/group_waiter"
	"github.com/Legit-Labs/legitify/internal/common/permissions"

	ghclient "github.com/Legit-Labs/legitify/internal/clients/github"
	"github.com/Legit-Labs/legitify/internal/clients/github/pagination"
	ghcollected "github.com/Legit-Labs/legitify/internal/collected/github"
	"github.com/Legit-Labs/legitify/internal/common/namespace"
	"github.com/Legit-Labs/legitify/internal/common/utils"
	"github.com/google/go-github/v53/github"
	"github.com/shurcooL/githubv4"
)

type repositoryCollector struct {
	collectors.BaseCollector
	Client           *ghclient.Client
	Context          context.Context
	scorecardEnabled bool
}

func NewRepositoryCollector(ctx context.Context, client *ghclient.Client) collectors.Collector {
	c := &repositoryCollector{
		BaseCollector:    collectors.NewBaseCollector(namespace.Repository),
		Client:           client,
		Context:          ctx,
		scorecardEnabled: context_utils.GetScorecardEnabled(ctx),
	}
	return c
}

type totalCountRepoQuery struct {
	Organization struct {
		Repositories struct {
			TotalCount githubv4.Int
		} `graphql:"repositories(first: 1, isArchived: false)"`
	} `graphql:"organization(login: $login)"`
}

func (rc *repositoryCollector) CollectTotalEntities() int {
	repositories, exist := context_utils.GetRepositories(rc.Context)
	if exist {
		return len(repositories)
	}

	gw := group_waiter.New()
	orgs, err := rc.Client.CollectOrganizations()

	if err != nil {
		log.Printf("failed to collect organization %s", err)
		return 0
	}

	var totalCount int32 = 0
	for _, org := range orgs {
		org := org
		gw.Do(func() {
			variables := map[string]interface{}{
				"login": githubv4.String(org.Name()),
			}

			totalCountQuery := totalCountRepoQuery{}

			e := rc.Client.GraphQLClient().Query(rc.Context, &totalCountQuery, variables)

			if e != nil {
				return
			}

			totalCount += int32(totalCountQuery.Organization.Repositories.TotalCount)
		})
	}
	gw.Wait()

	return int(totalCount)
}

func (rc *repositoryCollector) Collect() collectors.SubCollectorChannels {
	repositories, exist := context_utils.GetRepositories(rc.Context)

	if exist {
		return rc.collectSpecific(repositories)
	}

	return rc.collectAll()
}

func (rc *repositoryCollector) collectSpecific(repositories []types.RepositoryWithOwner) collectors.SubCollectorChannels {
	type specificRepoQuery struct {
		RepositoryOwner struct {
			Organization struct {
				ViewerCanAdminister *bool
			} `graphql:"... on Organization"`

			Login      githubv4.String
			Repository ghcollected.GitHubQLRepository `graphql:"repository(name: $name)"`
		} `graphql:"repositoryOwner(login: $login)"`
	}

	return rc.WrappedCollection(func() {
		gw := group_waiter.New()
		for _, r := range repositories {
			repo := r
			gw.Do(func() {
				variables := map[string]interface{}{
					"login": githubv4.String(repo.Owner),
					"name":  githubv4.String(repo.Name),
				}

				query := specificRepoQuery{}
				err := rc.Client.GraphQLClient().Query(rc.Context, &query, variables)
				if err != nil {
					log.Println(err.Error())
					return
				}

				var collectionContext *repositoryContext

				if query.RepositoryOwner.Organization.ViewerCanAdminister != nil {

					org, err := rc.Client.Organization(repo.Owner)
					if err != nil {
						log.Println(err.Error())
						return
					}

					hasBp := hasBranchProtection(org, query.RepositoryOwner.Repository.IsPrivate)
					collectionContext = newRepositoryContext([]permissions.Role{org.Role, query.RepositoryOwner.Repository.ViewerPermission},
						hasBp, org.IsEnterprise(), false, false)
				} else {
					hasBp := rc.hasBranchProtectionForUser(repo.Owner, query.RepositoryOwner.Repository.IsPrivate)
					collectionContext = newRepositoryContext([]permissions.Role{query.RepositoryOwner.Repository.ViewerPermission},
						hasBp, false, false, false)
				}

				rc.collectRepository(&query.RepositoryOwner.Repository, repo.Owner, collectionContext)
			})
		}

		gw.Wait()
	})
}

func (rc *repositoryCollector) collectAll() collectors.SubCollectorChannels {
	return rc.WrappedCollection(func() {
		orgs, err := rc.Client.CollectOrganizations()

		if err != nil {
			log.Printf("failed to collect organizations %s", err)
			return
		}

		gw := group_waiter.New()
		for _, org := range orgs {
			localOrg := org
			gw.Do(func() {
				_ = utils.Retry(func() (bool, error) {
					err := rc.collectRepositories(&localOrg)
					return true, err
				}, 5, fmt.Sprintf("collect repositories for %s", *localOrg.Login))
			})
		}
		gw.Wait()
	})
}

type repoQuery struct {
	Organization struct {
		Repositories struct {
			PageInfo ghcollected.GitHubQLPageInfo
			Nodes    []ghcollected.GitHubQLRepository
		} `graphql:"repositories(first: 50, after: $repositoryCursor, isArchived: false)"`
	} `graphql:"organization(login: $login)"`
}

func (rc *repositoryCollector) collectRepositories(org *ghcollected.ExtendedOrg) error {
	variables := map[string]interface{}{
		"login":            githubv4.String(org.Name()),
		"repositoryCursor": (*githubv4.String)(nil),
	}

	gw := group_waiter.New()
	defer gw.Wait()
	for {
		query := repoQuery{}
		err := rc.Client.GraphQLClient().Query(rc.Context, &query, variables)

		if err != nil {
			return err
		}

		gw.Do(func() {
			nodes := query.Organization.Repositories.Nodes
			extraGw := group_waiter.New()
			for i := range nodes {
				node := &(nodes[i])
				extraGw.Do(func() {
					collectionContext := newRepositoryContext([]permissions.Role{org.Role, node.ViewerPermission},
						hasBranchProtection(org, node.IsPrivate), org.IsEnterprise(), false, false)
					rc.collectRepository(node, org.Name(), collectionContext)
				})
			}
			extraGw.Wait()
		})

		if !query.Organization.Repositories.PageInfo.HasNextPage {
			break
		}

		variables["repositoryCursor"] = query.Organization.Repositories.PageInfo.EndCursor
	}

	return nil
}

func (rc *repositoryCollector) collectRepository(repository *ghcollected.GitHubQLRepository, login string, collectionContext *repositoryContext) {
	repo := rc.collectExtraData(login, repository, collectionContext.isBranchProtectionSupported)
	entityName := collectors.FullRepoName(login, repo.Repository.Name)
	missingPermissions := rc.checkMissingPermissions(repo, entityName, collectionContext)
	rc.IssueMissingPermissions(missingPermissions...)
	collectionContext.SetHasBranchProtectionPermission(!repo.NoBranchProtectionPermission)
	collectionContext.SetHasGithubAdvancedSecurity(repo.SecurityAndAnalysis != nil)
	rc.CollectDataWithContext(repo, repo.Repository.Url, collectionContext)
	rc.CollectionChangeByOne()
}

func (rc *repositoryCollector) collectExtraData(login string,
	repository *ghcollected.GitHubQLRepository,
	isBranchProtectionSupported bool) ghcollected.Repository {
	var err error
	repo := ghcollected.Repository{
		Repository: repository,
	}

	repo = rc.withVulnerabilityAlerts(repo, login)
	repo = rc.withRepositoryHooks(repo, login)
	repo = rc.withRepoCollaborators(repo, login)
	repo = rc.withActionsSettings(repo, login)
	repo, err = rc.withSecrets(repo, login)
	if err != nil {
		log.Printf("failed to collect repository secrets for %s: %s", repo.Repository.Name, err)
	}

	repo, err = rc.withDependencyGraphManifestsCount(repo, login)
	if err != nil {
		log.Printf("error getting repository dependency manifests for %s: %s", collectors.FullRepoName(login, repo.Repository.Name), err)
	}

	repo, err = rc.withSecurityAndAnalysis(repo, login)
	if err != nil {
		log.Printf("failed to collect repository Security and Analysis settings for %s: %s", repo.Repository.Name, err)
	}

	if isBranchProtectionSupported {
		repo, err = rc.fixBranchProtectionInfo(repo, login)
		if err != nil {
			// If we can't get branch protection info, rego will ignore it (as nil)
			log.Printf("error getting branch protection info for %s: %s", repository.Name, err)
		}
		repo, err = rc.withRulesSet(repo, login)
		if err != nil {
			log.Printf("error getting rules set for %s: %s", repository.Name, err)
		}
	} else {
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, collectors.FullRepoName(login, repo.Repository.Name), orgIsFreeEffect, namespace.Repository)
		rc.IssueMissingPermissions(perm)
	}

	if rc.scorecardEnabled {
		scResult, err := scorecard.Calculate(rc.Context, repository.Url, repo.Repository.IsPrivate)
		if err != nil {
			scResult = nil
			log.Printf("error getting scorecard result for %s: %s", repository.Name, err)
		}
		repo.Scorecard = scResult
	}

	return repo
}

func hasBranchProtection(org *ghcollected.ExtendedOrg, isPrivateRepository bool) bool {
	return org.IsEnterprise() || !isPrivateRepository
}

func (rc *repositoryCollector) hasBranchProtectionForUser(userLogin string, isPrivateRepository bool) bool {
	if isPrivateRepository {
		return true
	}

	user, _, err := rc.Client.Client().Users.Get(rc.Context, userLogin)
	if err != nil {
		return false
	}

	return user.Plan != nil && *user.Plan.Name != "free"
}

func (rc *repositoryCollector) withDependencyGraphManifestsCount(repo ghcollected.Repository, org string) (ghcollected.Repository, error) {
	var dependencyGraphQuery struct {
		RepositoryOwner struct {
			Repository struct {
				DependencyGraphManifests *ghcollected.GitHubQLDependencyGraphManifests `json:"dependency_graph_manifests" graphql:"dependencyGraphManifests(first: 1)"`
			} `graphql:"repository(name: $name)"`
		} `graphql:"repositoryOwner(login: $login)"`
	}

	variables := map[string]interface{}{
		"login": githubv4.String(org),
		"name":  githubv4.String(repo.Name()),
	}

	err := rc.Client.GraphQLClient().Query(rc.Context, &dependencyGraphQuery, variables)
	if err != nil {
		return repo, err
	}

	repo.DependencyGraphManifests = dependencyGraphQuery.RepositoryOwner.Repository.DependencyGraphManifests
	return repo, nil
}

func (rc *repositoryCollector) withActionsSettings(repo ghcollected.Repository, org string) ghcollected.Repository {
	settings, err := rc.Client.GetActionsTokenPermissionsForRepository(org, repo.Name())
	if err != nil {
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, collectors.FullRepoName(org, repo.Repository.Name),
			"Cannot read repository actions settings", namespace.Repository)
		rc.IssueMissingPermissions(perm)
		return repo
	}
	repo.ActionsTokenPermissions = settings
	return repo
}

func (rc *repositoryCollector) withRepositoryHooks(repo ghcollected.Repository, org string) ghcollected.Repository {
	res, err := pagination.New[*github.Hook](rc.Client.Client().Repositories.ListHooks, nil).Sync(rc.Context, org, repo.Repository.Name)
	if err != nil {
		if res.Resp.Response.StatusCode == http.StatusNotFound {
			perm := collectors.NewMissingPermission(permissions.RepoHookRead, collectors.FullRepoName(org, repo.Repository.Name),
				"Cannot read repository webhooks", namespace.Repository)
			rc.IssueMissingPermissions(perm)
		}
		return repo
	}

	repo.Hooks = res.Collected
	return repo
}

func (rc *repositoryCollector) withVulnerabilityAlerts(repo ghcollected.Repository, org string) ghcollected.Repository {
	enabled, _, err := rc.Client.Client().Repositories.GetVulnerabilityAlerts(rc.Context, org, repo.Repository.Name)
	if err != nil {
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, collectors.FullRepoName(org, repo.Repository.Name),
			"Cannot read repository vulnerability alerts", namespace.Repository)
		rc.IssueMissingPermissions(perm)
		return repo
	}

	repo.VulnerabilityAlertsEnabled = &enabled
	return repo
}

func (rc *repositoryCollector) withRepoCollaborators(repo ghcollected.Repository, org string) ghcollected.Repository {
	users, err := pagination.New[*github.User](rc.Client.Client().Repositories.ListCollaborators, &github.ListCollaboratorsOptions{}).Sync(rc.Context, org, repo.Repository.Name)
	if err != nil {
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, collectors.FullRepoName(org, repo.Repository.Name),
			"Cannot read repository collaborators", namespace.Repository)
		rc.IssueMissingPermissions(perm)
		return repo
	}

	repo.Collaborators = users.Collected
	return repo
}

func (rc *repositoryCollector) withRulesSet(repository ghcollected.Repository, org string) (ghcollected.Repository, error) {
	if repository.Repository.DefaultBranchRef == nil {
		return repository, nil // no branches
	}

	rules, err := rc.Client.GetRulesForBranch(org, repository.Name(),
		*repository.Repository.DefaultBranchRef.Name)

	if err != nil {
		return repository, err
	}

	repository.RulesSet = rules
	return repository, nil
}

func (rc *repositoryCollector) withSecrets(repository ghcollected.Repository, login string) (ghcollected.Repository, error) {
	secrets, err := rc.Client.GetRepositorySecrets(repository.Name(), login)
	if err != nil {
		return repository, err
	}
	var repoSecrets []*ghcollected.RepositorySecret
	for i := 0; i < len(secrets.Secrets); i++ {
		repoSecrets = append(repoSecrets, &ghcollected.RepositorySecret{
			Name:      secrets.Secrets[i].Name,
			UpdatedAt: int(secrets.Secrets[i].UpdatedAt.Time.UnixNano()),
		})
	}
	repository.RepoSecrets = repoSecrets
	return repository, nil
}

func (rc *repositoryCollector) withSecurityAndAnalysis(repo ghcollected.Repository, login string) (ghcollected.Repository, error) {
	
	securityAndAnalysis, err := rc.Client.GetSecurityAndAnalysisForRepository(repo.Name(), login)
	if err != nil {
		return repo, err
	}

	repo.SecurityAndAnalysis = securityAndAnalysis
	return repo, nil
}

// fixBranchProtectionInfo fixes the branch protection info for the repository,
// to reflect whether there is no branch protection, or just no permission to fetch the info.
func (rc *repositoryCollector) fixBranchProtectionInfo(repository ghcollected.Repository, org string) (ghcollected.Repository, error) {
	if repository.Repository.DefaultBranchRef == nil {
		return repository, nil // no branches
	}
	if repository.Repository.DefaultBranchRef.BranchProtectionRule != nil {
		return repository, nil // branch protection info already available
	}

	repoName := repository.Repository.Name
	branchName := *repository.Repository.DefaultBranchRef.Name
	_, _, err := rc.Client.Client().Repositories.GetBranchProtection(rc.Context, org, repoName, branchName)
	if err == nil {
		log.Printf("inconsistent permissions (GitHub bug): graphQL query failed, but branch protection info is available. Ignoring\n")
		return repository, nil
	}

	isNoPermErr := func(err error) bool {
		// Inspired by gitHub.isBranchNotProtected()
		const noPermMessage = "Not Found"
		errorResponse, ok := err.(*github.ErrorResponse)
		return ok && errorResponse.Message == noPermMessage
	}

	switch {
	case isNoPermErr(err):
		repository.NoBranchProtectionPermission = true
	case err == github.ErrBranchNotProtected:
		// Already the default value for the NoBranchProtectionPerm & BranchProtectionRule fields
	default: // Any other error is an operational error
		return repository, err
	}

	return repository, nil
}

func (rc *repositoryCollector) checkMissingPermissions(repo ghcollected.Repository, entityName string, repoContext *repositoryContext) []collectors.MissingPermission {
	var missingPermissions []collectors.MissingPermission
	if repo.NoBranchProtectionPermission {
		effect := "Cannot read repository branch protection information"
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, entityName, effect, namespace.Repository)
		missingPermissions = append(missingPermissions, perm)
	}
	if repo.SecurityAndAnalysis == nil {
		var effect string
		if !checkRepoAdminPermission(repoContext.roles) {
			effect = "Cannot read repository Security and Analysis settings"
		} else if repo.Repository.IsPrivate {
			effect = "Your GitHub plan does not include a secret scanning feature."
		}
		perm := collectors.NewMissingPermission(permissions.RepoAdmin, entityName, effect, namespace.Repository)
		missingPermissions = append(missingPermissions, perm)
	}

	return missingPermissions
}

func checkRepoAdminPermission(roles []permissions.RepositoryRole) bool{
	for _, role := range roles {
		if (permissions.IsRepositoryRole(role) && role == permissions.RepoRoleAdmin) ||
			(permissions.IsOrgRole(role) && role == permissions.OrgRoleOwner) {
			return true
		}
	}
	return false
}

const (
	orgIsFreeEffect = "Branch protection cannot be collected because the organization is in free plan"
)
