// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"time"

	jenkins "github.com/cpanato/golang-jenkins"
	"github.com/mattermost/mattermost-mattermod/model"
	"github.com/mattermost/mattermost-server/mlog"
	mattermostModel "github.com/mattermost/mattermost-server/model"
)

// The following structs are copied from the mattermost-cloud repo to allow
// mattermod to interact with provisioning servers.
//
// TODO: consider moving the structs in mattermost-cloud for these models out
// of the /internal directory so that they can be vendored and imported here.
// When doing this, we should start using semver in the mattermost-cloud repo.

// CreateClusterRequest specifies the parameters for a new cluster.
type CreateClusterRequest struct {
	Provider string
	Size     string
	Zones    []string
}

// Cluster represents a Kubernetes cluster.
type Cluster struct {
	ID                  string
	Provider            string
	Provisioner         string
	ProviderMetadata    []byte `json:",omitempty"`
	ProvisionerMetadata []byte `json:",omitempty"`
	AllowInstallations  bool
	Size                string
	State               string
	CreateAt            int64
	DeleteAt            int64
	LockAcquiredBy      *string
	LockAcquiredAt      int64
}

// CreateInstallationRequest specifies the parameters for a new installation.
type CreateInstallationRequest struct {
	OwnerID  string
	Version  string
	DNS      string
	Size     string
	Affinity string
}

// Installation represents a Mattermost installation.
type Installation struct {
	ID             string
	OwnerID        string
	Version        string
	DNS            string
	Size           string
	Affinity       string
	GroupID        *string
	State          string
	CreateAt       int64
	DeleteAt       int64
	LockAcquiredBy *string
	LockAcquiredAt int64
}

func waitForBuildAndSetupSpinWick(pr *model.PullRequest, size string) {
	err := waitForBuildComplete(pr)
	if err != nil {
		return
	}

	if result := <-Srv.Store.Spinmint().Get(pr.Number); result.Err != nil {
		mlog.Error("Unable to get the SpinWick information. Will not build the SpinWick", mlog.String("pr_error", result.Err.Error()))
	} else if result.Data == nil {
		mlog.Info("No SpinWick for this PR in the Database. Will create a new one.")
		installationID, err := createSpinWick(pr, size)
		if err != nil {
			LogErrorToMattermost("Unable to set up SpinWick for PR %v in %v/%v: %v", pr.Number, pr.RepoOwner, pr.RepoName, err.Error())
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
			return
		}

		spinmint := &model.Spinmint{
			InstanceId: installationID,
			RepoOwner:  pr.RepoOwner,
			RepoName:   pr.RepoName,
			Number:     pr.Number,
			CreatedAt:  time.Now().UTC().Unix(),
		}
		storeSpinmintInfo(spinmint)
	}
}

func waitForBuildComplete(pr *model.PullRequest) error {
	repo, ok := Config.GetRepository(pr.RepoOwner, pr.RepoName)
	if !ok || repo.JenkinsServer == "" {
		mlog.Error("Unable to set up spintmint for PR without Jenkins configured for server", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return fmt.Errorf("Unable to set up spintmint for PR without Jenkins configured for server")
	}

	credentials, ok := Config.JenkinsCredentials[repo.JenkinsServer]
	if !ok {
		mlog.Error("No Jenkins credentials for server required for PR", mlog.String("jenkins", repo.JenkinsServer), mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return fmt.Errorf("No Jenkins credentials for server required for PR")
	}

	client := jenkins.NewJenkins(&jenkins.Auth{
		Username: credentials.Username,
		ApiToken: credentials.ApiToken,
	}, credentials.URL)

	mlog.Info("Waiting for Jenkins to build to set up SpinWick for PR", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName), mlog.String("build_link", pr.BuildLink))

	pr, errr := waitForBuild(client, pr)
	if errr == false || pr == nil {
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return fmt.Errorf("error in the build. aborting")
	}

	return nil
}

func createSpinWick(pr *model.PullRequest, size string) (string, error) {
	mlog.Info("Provisioner Server - Installation request")

	prID := makePullRequestID(pr.RepoName, pr.Number)
	installationRequest := CreateInstallationRequest{
		OwnerID:  prID,
		Version:  pr.Sha[0:7],
		DNS:      fmt.Sprintf("%s.%s", prID, Config.DNSNameTestServer),
		Size:     size,
		Affinity: "multitenant",
	}

	b, err := json.Marshal(installationRequest)
	if err != nil {
		mlog.Error("Error trying to marshal the installation request", mlog.Err(err))
		return "", err
	}

	url := fmt.Sprintf("%s/api/installations", Config.ProvisionerServer)
	respReqInstallation, err := makeRequest("POST", url, bytes.NewBuffer(b))
	if err != nil {
		mlog.Error("Error making the post request to create the mattermost installation", mlog.Err(err))
		return "", err
	}
	defer respReqInstallation.Body.Close()

	var installation Installation
	err = json.NewDecoder(respReqInstallation.Body).Decode(&installation)
	if err != nil && err != io.EOF {
		mlog.Error("Error decoding installation response", mlog.Err(err))
		return "", err
	}
	installationID := installation.ID
	mlog.Info("Provisioner Server - installation request", mlog.String("InstallationID", installationID))

	time.Sleep(3 * time.Second)
	// Get the installaion to check if the state is creation-no-compatible-clusters
	// if is that state we need to requst a new k8s cluster
	// TODO:
	// There is no garauntee that the installation has been worked on yet. We
	// may have to wait longer for it to enter the creation-no-compatible-clusters
	// state.
	url = fmt.Sprintf("%s/api/installation/%s", Config.ProvisionerServer, installationID)
	resp, err := makeRequest("GET", url, nil)
	if err != nil {
		mlog.Error("Error making the post request to create the mm installation", mlog.Err(err))
		return "", err
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&installation)
	if err != nil && err != io.EOF {
		mlog.Error("Error decoding installation", mlog.Err(err))
		return "", fmt.Errorf("Error decoding installation")
	}
	if installation.State == "creation-no-compatible-clusters" {
		err = requestK8sClusterCreation(pr)
		if err != nil {
			return "", err
		}
	}

	wait := 480
	mlog.Info("Waiting up to 480 seconds for the mattermost installation to complete...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	err = waitMattermostInstallation(ctx, pr, installationID, false)
	if err != nil {
		return "", err
	}

	return installationID, nil
}

func updateSpinWick(pr *model.PullRequest) {
	foundLabel := false
	for _, label := range pr.Labels {
		if label == Config.SetupSpinWick {
			mlog.Info("PR has the SpinWick label; will check the upgrade", mlog.Int("pr", pr.Number))
			foundLabel = true
			break
		}
		if label == Config.SetupSpinWickHA {
			mlog.Info("PR has the SpinWick HA label; will check the upgrade", mlog.Int("pr", pr.Number))
			foundLabel = true
			break
		}
	}

	if !foundLabel {
		mlog.Info("PR does not have a SpinWick label", mlog.Int("pr", pr.Number))
		return
	}

	// TODO: add a new column in the db to get the previous job and wait for the new one start
	// for now will sleep some time
	mlog.Info("Sleeping a bit to wait for the build process start", mlog.Int("pr", pr.Number), mlog.String("sha", pr.Sha))
	time.Sleep(60 * time.Second)

	wait := 480
	mlog.Info("Waiting up to 480 seconds to get the up-to-date build link...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	// need to do this workaroud here because when push a new commit the build link
	// is not updated and can be blank for some time
	buildLink, err := checkBuildLink(ctx, pr)
	if err != nil || buildLink == "" {
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}
	mlog.Info("Build Link updated", mlog.String("buildLink", buildLink), mlog.String("OldBuildLink", pr.BuildLink))
	// update the build link
	pr.BuildLink = buildLink
	if result := <-Srv.Store.PullRequest().Save(pr); result.Err != nil {
		mlog.Error(result.Err.Error())
	}
	mlog.Info("New build", mlog.String("New", pr.BuildLink))

	var installation string
	result := <-Srv.Store.Spinmint().Get(pr.Number)
	if result.Err != nil {
		mlog.Error("Unable to get the SpinWick information; skipping...", mlog.String("pr_error", result.Err.Error()))
		return
	} else if result.Data == nil {
		mlog.Error("No SpinWick for this PR in the database; skipping...")
		return
	} else {
		spinmint := result.Data.(*model.Spinmint)
		installation = spinmint.InstanceId
	}

	commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick upgrade will occur after the build is successful.")
	err = waitForBuildComplete(pr)
	if err != nil {
		mlog.Error(fmt.Sprintf("Error waiting for build to complete: %s", err))
		return
	}
	// TODO: remove this when we starting building the docker image in the sam build pipeline
	time.Sleep(60 * time.Second)

	mlog.Info("Provisioner Server - Upgrade request", mlog.String("SHA", pr.Sha))
	shortCommit := pr.Sha[0:7]
	payload := fmt.Sprintf("{\n\"version\": \"%s\"}", shortCommit)
	var mmStr = []byte(payload)
	url := fmt.Sprintf("%s/api/installation/%s/mattermost", Config.ProvisionerServer, installation)
	resp, err := makeRequest("PUT", url, bytes.NewBuffer(mmStr))
	if err != nil {
		mlog.Error("Error making the put request to upgrade the mm cluster", mlog.Err(err))
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Error during the request to upgrade. Please remove the label and try again.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		mlog.Error("Error request was not accepted", mlog.Int("StatusCode", resp.StatusCode))
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Error doing the upgrade process. Please remove the label and try again.")
		return
	}

	wait = 480
	mlog.Info("Waiting up to 480 seconds for the mattermost installation to complete...")
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	err = waitMattermostInstallation(ctx, pr, installation, true)
	if err != nil {
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}
}

func destroySpinWick(pr *model.PullRequest, instanceClusterID string) {
	mlog.Info("Destroying SpinWick experimental for PR", mlog.String("instance", instanceClusterID), mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))

	destroyMMInstallation(instanceClusterID)
	// Remove from the local db
	removeSpinmintInfo(instanceClusterID)
}

func destroyMMInstallation(instanceClusterID string) {
	url := fmt.Sprintf("%s/api/installation/%s", Config.ProvisionerServer, instanceClusterID)
	resp, err := makeRequest("DELETE", url, nil)
	if err != nil {
		mlog.Error("Error deleting the installation", mlog.Err(err))
	}
	defer resp.Body.Close()
}

func checkBuildLink(ctx context.Context, pr *model.PullRequest) (string, error) {
	client := NewGithubClient()
	repo, _ := Config.GetRepository(pr.RepoOwner, pr.RepoName)
	for {
		combined, _, err := client.Repositories.GetCombinedStatus(context.Background(), pr.RepoOwner, pr.RepoName, pr.Sha, nil)
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
		checks, _, err := client.Checks.ListCheckRunsForRef(context.Background(), pr.RepoOwner, pr.RepoName, pr.Sha, nil)
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
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Timed out waiting for build link. Please check the logs.")
			return "", fmt.Errorf("timed out waiting the build link")
		case <-time.After(10 * time.Second):
		}
	}
}

func waitMattermostInstallation(ctx context.Context, pr *model.PullRequest, installationRequestID string, upgrade bool) error {
	for {
		url := fmt.Sprintf("%s/api/installation/%s", Config.ProvisionerServer, installationRequestID)
		resp, err := makeRequest("GET", url, nil)
		if err != nil {
			mlog.Error("Error making the post request to create the mm installation", mlog.Err(err))
			return err
		}
		defer resp.Body.Close()
		var installationRequest Installation
		err = json.NewDecoder(resp.Body).Decode(&installationRequest)
		if err != nil && err != io.EOF {
			mlog.Error("Error decoding installation", mlog.Err(err))
			return fmt.Errorf("Error decoding installation")
		}
		if installationRequest.State == "stable" {
			mmURL := fmt.Sprintf("https://%s.%s", makePullRequestID(pr.RepoName, pr.Number), Config.DNSNameTestServer)
			if !upgrade {
				userErr := initializeMattermostTestServer(mmURL, pr.Number)
				if userErr != nil {
					commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to create mattermost installation.")
					destroyMMInstallation(installationRequest.ID)
					return nil
				}
				userTable := "| Account Type | Username | Password |\n|---|---|---|\n| Admin | sysadmin | Sys@dmin123 |\n| User | user-1 | User-1@123 |"
				msg := fmt.Sprintf("Mattermost test server created! :tada:\n\nAccess here: %s\n\n%s", mmURL, userTable)
				commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, msg)
			} else {
				msg := fmt.Sprintf("Mattermost test server updated!\n\nAccess here: %s", mmURL)
				commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, msg)
			}
			return nil
		} else if installationRequest.State == "creation-failed" {
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to create mattermost installation.")
			destroyMMInstallation(installationRequest.ID)
			return fmt.Errorf("error creating mattermost installation")
		}
		mlog.Info("Provisioner Server - installation request creating... sleep", mlog.String("InstallationID", installationRequest.ID), mlog.String("State", installationRequest.State))
		select {
		case <-ctx.Done():
			destroyMMInstallation(installationRequest.ID)
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Timed out waiting for the mattermost installation. Please check the logs.")
			return fmt.Errorf("timed out waiting for the mattermost installation complete. requesting the deletion")
		case <-time.After(10 * time.Second):
		}
	}
}

func waitK8sCluster(ctx context.Context, pr *model.PullRequest, clusterRequestID string) error {
	for {
		url := fmt.Sprintf("%s/api/cluster/%s", Config.ProvisionerServer, clusterRequestID)
		resp, err := makeRequest("GET", url, nil)
		if err != nil {
			mlog.Error("Error making the post request to create the k8s cluster", mlog.Err(err))
			return err
		}
		defer resp.Body.Close()

		var clusterRequest Cluster
		err = json.NewDecoder(resp.Body).Decode(&clusterRequest)
		if err != nil && err != io.EOF {
			mlog.Error("Error decoding cluster response", mlog.Err(err))
		}
		if clusterRequest.State == "stable" {
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Kubernetes cluster created. Now will deploy Mattermost... Hang on!")
			return nil
		} else if clusterRequest.State == "creation-failed" {
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to create the k8s cluster.")
			return fmt.Errorf("error creating k8s cluster")
		}
		mlog.Info("Provisioner Server - cluster request creating... sleep", mlog.String("ClusterID", clusterRequest.ID), mlog.String("State", clusterRequest.State))
		time.Sleep(20 * time.Second)
		select {
		case <-ctx.Done():
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Timed out waiting for the kubernetes cluster. Please check the logs.")
			return fmt.Errorf("timed out waiting for the cluster installation complete")
		case <-time.After(10 * time.Second):
		}
	}
}

func makeRequest(method, url string, payload io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func initializeMattermostTestServer(mmURL string, prNumber int) error {
	mlog.Info("Will check if can ping the new DNS otherwise will wait for the DNS propagation for 5 minutes")
	wait := 300
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	mmHost, _ := url.Parse(mmURL)
	err := checkDNS(ctx, fmt.Sprintf("%s:443", mmHost.Host))
	if err != nil {
		mlog.Info("URL not accessible")
		return err
	}

	mlog.Info("Will create the initial user")
	client := mattermostModel.NewAPIv4Client(mmURL)

	//check if Mattermost is available
	wait = 300
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	err = checkMMPing(client, ctx)
	if err != nil {
		return err
	}

	user := &mattermostModel.User{
		Username: "sysadmin",
		Email:    "sysadmin@example.mattermost.com",
		Password: "Sys@dmin123",
	}
	_, response := client.CreateUser(user)
	if response.StatusCode != 201 {
		mlog.Error("Error creating the initial user", mlog.Int("StatusCode", response.StatusCode), mlog.String("Message", response.Error.Message))
		return fmt.Errorf(response.Error.Message)
	}
	mlog.Info("Done the creation of the initial user")

	mlog.Info("Logging into MM")
	client.Logout()
	userLogged, response := client.Login("sysadmin", "Sys@dmin123")
	if response.StatusCode != 200 {
		mlog.Error("Error logging with the initial user", mlog.Int("StatusCode", response.StatusCode), mlog.String("Message", response.Error.Message))
		return fmt.Errorf(response.Error.Message)
	}
	mlog.Info("Done logging into MM")

	mlog.Info("Creating new Team")
	teamName := fmt.Sprintf("pr%d", prNumber)
	team := &mattermostModel.Team{
		Name:        teamName,
		DisplayName: teamName,
		Type:        "O",
	}
	firstTeam, response := client.CreateTeam(team)
	if response.StatusCode != 201 {
		mlog.Error("Error creating the initial team", mlog.Int("StatusCode", response.StatusCode))
	}
	mlog.Info("Done creating new Team and will update the config")

	_, response = client.AddTeamMember(firstTeam.Id, userLogged.Id)
	if response.StatusCode != 201 {
		mlog.Error("Error adding sysadmin to the initial team", mlog.Int("StatusCode", response.StatusCode))
	}

	// Create test user-1
	testUser := &mattermostModel.User{
		Username: "user-1",
		Email:    "user-1@example.mattermost.com",
		Password: "User-1@123",
	}
	testUser, response = client.CreateUser(testUser)
	if response.StatusCode != 201 {
		mlog.Error("Error creating the initial test user", mlog.Int("StatusCode", response.StatusCode), mlog.String("Message", response.Error.Message))
	}
	_, response = client.AddTeamMember(firstTeam.Id, testUser.Id)
	if response.StatusCode != 201 {
		mlog.Error("Error adding test user to the initial team", mlog.Int("StatusCode", response.StatusCode))
	}

	config, response := client.GetConfig()
	if response.StatusCode != 200 {
		mlog.Error("Error getting the config ", mlog.Int("StatusCode", response.StatusCode), mlog.String("Message", response.Error.Message))
		return fmt.Errorf(response.Error.Message)
	}

	config.TeamSettings.EnableOpenServer = NewBool(true)
	config.TeamSettings.ExperimentalViewArchivedChannels = NewBool(true)
	config.PluginSettings.EnableUploads = NewBool(true)
	config.ServiceSettings.EnableTesting = NewBool(true)
	config.ServiceSettings.ExperimentalLdapGroupSync = NewBool(true)
	config.ServiceSettings.EnableDeveloper = NewBool(true)
	config.LogSettings.FileLevel = NewString("INFO")
	config.EmailSettings.FeedbackName = NewString("SpinWick Feedback")
	config.EmailSettings.FeedbackEmail = NewString("feedback@mattermost.com")
	config.EmailSettings.ReplyToAddress = NewString("feedback@mattermost.com")
	config.EmailSettings.SMTPUsername = NewString(Config.AWSEmailAccessKey)
	config.EmailSettings.SMTPPassword = NewString(Config.AWSEmailSecretKey)
	config.EmailSettings.SMTPServer = NewString(Config.AWSEmailEndpoint)
	config.EmailSettings.SMTPPort = NewString("465")
	config.EmailSettings.EnableSMTPAuth = NewBool(true)
	config.EmailSettings.ConnectionSecurity = NewString("TLS")
	config.EmailSettings.SendEmailNotifications = NewBool(true)
	config.LdapSettings.Enable = NewBool(true)
	config.LdapSettings.EnableSync = NewBool(true)
	config.LdapSettings.LdapServer = NewString("ldap.forumsys.com")
	config.LdapSettings.BaseDN = NewString("dc=example,dc=com")
	config.LdapSettings.BindUsername = NewString("cn=read-only-admin,dc=example,dc=com")
	config.LdapSettings.BindPassword = NewString("password")
	config.LdapSettings.GroupDisplayNameAttribute = NewString("cn")
	config.LdapSettings.GroupIdAttribute = NewString("entryUUID")
	config.LdapSettings.EmailAttribute = NewString("mail")
	config.LdapSettings.UsernameAttribute = NewString("uid")
	config.LdapSettings.IdAttribute = NewString("uid")
	config.LdapSettings.LoginIdAttribute = NewString("uid")

	// UpdateConfig
	_, response = client.UpdateConfig(config)
	if response.StatusCode != 200 {
		mlog.Error("Error setting the config ", mlog.Int("StatusCode", response.StatusCode), mlog.String("Message", response.Error.Message))
		return fmt.Errorf(response.Error.Message)
	}

	mlog.Info("Done update the config. All good.")

	return nil
}

func requestK8sClusterCreation(pr *model.PullRequest) error {
	mlog.Info("Building new kubernetes cluster")

	url := fmt.Sprintf("%s/api/clusters", Config.ProvisionerServer)
	commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Please wait while a new kubernetes cluster is created for your SpinWick")

	clusterRequest := CreateClusterRequest{
		Size: "SizeAlef1000",
	}
	b, err := json.Marshal(clusterRequest)
	if err != nil {
		mlog.Error("Error trying to marshal the cluster request", mlog.Err(err))
		return err
	}

	respReqCluster, err := makeRequest("POST", url, bytes.NewBuffer(b))
	if err != nil {
		mlog.Error("Error trying to send the k8s-cluster-creation request", mlog.Err(err))
		return err
	}
	defer respReqCluster.Body.Close()

	var cluster Cluster
	err = json.NewDecoder(respReqCluster.Body).Decode(&cluster)
	if err != nil && err != io.EOF {
		mlog.Error("Error decoding", mlog.Err(err))
		return err
	}
	mlog.Info("Provisioner Server - cluster request", mlog.String("ClusterID", cluster.ID))

	wait := 900
	mlog.Info("Waiting up to 900 seconds for the k8s cluster creation to complete...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	return waitK8sCluster(ctx, pr, cluster.ID)
}

func checkDNS(ctx context.Context, url string) error {
	for {
		timeout := time.Duration(2 * time.Second)
		_, err := net.DialTimeout("tcp", url, timeout)
		if err == nil {
			mlog.Debug("URL reachable", mlog.String("URL", url))
			return nil
		}
		select {
		case <-ctx.Done():
			mlog.Error("Timeout while checking the URL. URL not reachabled", mlog.String("URL", url))
			return fmt.Errorf("Timeout while checking the URL. URL not reachabled")
		case <-time.After(10 * time.Second):
			mlog.Debug("not reachabled, will sleep 10 seconds", mlog.String("URL", url))
		}
	}
}

func checkMMPing(client *mattermostModel.Client4, ctx context.Context) error {
	for {
		status, response := client.GetPing()
		if response.StatusCode == 200 && status == "OK" {
			return nil
		}
		select {
		case <-ctx.Done():
			mlog.Error("Timeout while checking mattermost")
			return fmt.Errorf("Timeout while checking mattermost")
		case <-time.After(10 * time.Second):
			mlog.Debug("cannot get the mattermost ping, waiting a bit more")
		}
	}
}

func makePullRequestID(repoName string, prNumber int) string {
	return strings.ToLower(fmt.Sprintf("%s-pr-%d", repoName, prNumber))
}

func NewBool(b bool) *bool       { return &b }
func NewInt(n int) *int          { return &n }
func NewInt64(n int64) *int64    { return &n }
func NewInt32(n int32) *int32    { return &n }
func NewString(s string) *string { return &s }