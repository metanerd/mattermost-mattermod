package server

import (
	"context"
	"strconv"
	"strings"
	"time"

	jenkins "github.com/cpanato/golang-jenkins"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/mattermost/mattermost-mattermod/model"
	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/pkg/errors"
)

// Builds implements buildsInterface for working with external CI/CD systems.
type Builds struct{}

type buildsInterface interface {
	getInstallationVersion(pr *model.PullRequest) string
	waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest) (*model.PullRequest, error)
	buildJenkinsClient(s *Server, pr *model.PullRequest) (*Repository, *jenkins.Jenkins, error)
	waitForBuild(ctx context.Context, s *Server, client *jenkins.Jenkins, pr *model.PullRequest) (*model.PullRequest, error)
	checkBuildLink(ctx context.Context, s *Server, pr *model.PullRequest) (string, error)
}

func (b *Builds) getInstallationVersion(pr *model.PullRequest) string {
	return pr.Sha[0:7]
}

func (b *Builds) buildJenkinsClient(s *Server, pr *model.PullRequest) (*Repository, *jenkins.Jenkins, error) {
	repo, ok := GetRepository(s.Config.Repositories, pr.RepoOwner, pr.RepoName)
	if !ok || repo.JenkinsServer == "" {
		return repo, nil, errors.New("jenkins server is not configured")
	}
	credentials, ok := s.Config.JenkinsCredentials[repo.JenkinsServer]
	if !ok {
		return repo, nil, errors.New("jenkins server credentials are not configured")
	}

	client := jenkins.NewJenkins(&jenkins.Auth{
		Username: credentials.Username,
		ApiToken: credentials.APIToken,
	}, credentials.URL)

	return repo, client, nil
}

func (b *Builds) waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest) (*model.PullRequest, error) {
	for {
		select {
		case <-ctx.Done():
			return pr, errors.New("timed out waiting for image to publish")
		case <-time.After(10 * time.Second):
			var err error
			pr, err = s.Store.PullRequest().Get(pr.RepoOwner, pr.RepoName, pr.Number)
			if err != nil {
				return nil, errors.Wrap(err, "unable to get updated PR from Mattermod database")
			}

			// Update the PR in case the build link has changed because of a new commit
			desiredTag := b.getInstallationVersion(pr)
			image := "mattermost/mattermost-enterprise-edition"

			_, err = reg.ManifestDigest(image, desiredTag)
			if err != nil && !strings.Contains(err.Error(), "status=404") {
				return pr, errors.Wrap(err, "unable to fetch tag from docker registry")
			}

			if err == nil {
				mlog.Info("docker tag found, image was uploaded", mlog.String("image", image), mlog.String("tag", desiredTag))
				return pr, nil
			}

			mlog.Info("docker tag for the build not found. waiting a bit more...", mlog.String("image", image), mlog.String("tag", desiredTag), mlog.String("repo", pr.RepoName), mlog.Int("number", pr.Number))
		}
	}
}

func (b *Builds) waitForBuild(ctx context.Context, s *Server, client *jenkins.Jenkins, pr *model.PullRequest) (*model.PullRequest, error) {
	for {
		select {
		case <-ctx.Done():
			return pr, errors.New("timed out waiting for build to finish")
		case <-time.After(30 * time.Second):
			var err error
			pr, err = s.Store.PullRequest().Get(pr.RepoOwner, pr.RepoName, pr.Number)
			if err != nil {
				return nil, errors.Wrap(err, "unable to get updated PR from Mattermod database")
			}

			// Update the PR in case the build link has changed because of a new commit
			pr, err = s.GetUpdateChecks(ctx, pr.RepoOwner, pr.RepoName, pr.Number)
			if err != nil {
				return pr, errors.Wrap(err, "unable to get updated PR from GitHub")
			}
			mlog.Info("Current PR Status", mlog.String("repo_name", pr.RepoName), mlog.String("build_status", pr.BuildStatus), mlog.String("build_conclusion", pr.BuildConclusion))

			if pr.RepoName == "mattermost-webapp" {
				switch pr.BuildStatus {
				case "in_progress":
					mlog.Info("Build in CircleCI is still in progress")
				case "completed":
					if pr.BuildConclusion == "success" {
						mlog.Info("Build in CircleCI succeed")
						return pr, nil
					}
					return pr, errors.New("build failed")
				default:
					return pr, errors.Errorf("unknown build status %s", pr.BuildStatus)
				}
			} else {
				if pr.BuildLink == "" {
					mlog.Info("No build link found; skipping...")
				} else {
					mlog.Info("BuildLink for PR", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName), mlog.String("buildlink", pr.BuildLink))
					// Doing this because the lib we are using does not support folders :(
					var jobNumber int64
					var jobName string

					parts := strings.Split(pr.BuildLink, "/")
					// Doing this because the lib we are using does not support folders :(
					switch pr.RepoName {
					case serverRepoName:
						jobNumber, _ = strconv.ParseInt(parts[len(parts)-3], 10, 32)
						jobName = parts[len(parts)-6]     //mattermost-server
						subJobName := parts[len(parts)-4] //PR-XXXX
						jobName = "mp/job/" + jobName + "/job/" + subJobName
					default:
						return pr, errors.Errorf("unsupported repository %s", pr.RepoName)
					}

					job, err := client.GetJob(jobName)
					if err != nil {
						return pr, errors.Wrapf(err, "failed to get Jenkins job %s", jobName)
					}

					// Doing this because the lib we are using does not support folders :(
					// This time is in the Jenkins job Name because it returns just the name
					job.Name = jobName

					build, err := client.GetBuild(job, int(jobNumber))
					if err != nil {
						return pr, errors.Wrapf(err, "failed to get Jenkins build %d", build.Number)
					}

					switch {
					case !build.Building && build.Result == "SUCCESS":
						mlog.Info("build for PR succeeded!", mlog.Int("build_number", build.Number), mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))
						return pr, nil
					case build.Result == "FAILURE" || build.Result == "ABORTED":
						return pr, errors.Errorf("build %d failed with status %q", build.Number, build.Result)
					default:
						mlog.Info("Build is running", mlog.Int("build", build.Number), mlog.Bool("building", build.Building))
					}
				}
			}

			mlog.Info("Build is still in progress; sleeping...")
		}
	}
}

func (b *Builds) checkBuildLink(ctx context.Context, s *Server, pr *model.PullRequest) (string, error) {
	repo, _ := GetRepository(s.Config.Repositories, pr.RepoOwner, pr.RepoName)
	for {
		combined, _, err := s.GithubClient.Repositories.GetCombinedStatus(ctx, pr.RepoOwner, pr.RepoName, pr.Sha, nil)
		if err != nil {
			return "", err
		}
		for _, status := range combined.Statuses {
			if *status.Context == repo.BuildStatusContext {
				if *status.TargetURL != "" {
					return *status.TargetURL, nil
				}
			}
		}

		// for the repos using circleci we have the checks now
		checks, _, err := s.GithubClient.Checks.ListCheckRunsForRef(ctx, pr.RepoOwner, pr.RepoName, pr.Sha, nil)
		if err != nil {
			return "", err
		}
		for _, status := range checks.CheckRuns {
			if *status.Name == repo.BuildStatusContext {
				return status.GetHTMLURL(), nil
			}
		}

		select {
		case <-ctx.Done():
			if err = s.sendGitHubComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, "Timed out waiting for build link. Please check the logs."); err != nil {
				mlog.Warn("Error while commenting", mlog.Err(err))
			}
			return "", errors.New("timed out waiting the build link")
		case <-time.After(10 * time.Second):
		}
	}
}
