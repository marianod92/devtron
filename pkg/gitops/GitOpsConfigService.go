/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package gitops

import (
	"encoding/json"
	"fmt"
	"github.com/devtron-labs/devtron/client/argocdServer"
	"github.com/devtron-labs/devtron/internal/sql/models"
	"github.com/devtron-labs/devtron/internal/sql/repository"
	"github.com/devtron-labs/devtron/internal/util"
	"github.com/devtron-labs/devtron/pkg/cluster"
	"github.com/devtron-labs/devtron/pkg/pipeline"
	"github.com/devtron-labs/devtron/pkg/user"
	"github.com/ghodss/yaml"
	"github.com/go-pg/pg"
	dirCopy "github.com/otiai10/copy"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ChartWorkingDirs string

type GitOpsConfigService interface {
	CreateGitOpsConfig(config *GitOpsConfigDto) (*GitOpsConfigDto, error)
	UpdateGitOpsConfig(config *GitOpsConfigDto) error
	GetGitOpsConfigById(id int) (*GitOpsConfigDto, error)
	GetAllGitOpsConfig() ([]*GitOpsConfigDto, error)
	GetGitOpsConfigByProvider(provider string) (*GitOpsConfigDto, error)
	GetGitOpsConfigActive() (*GitOpsConfigDto, error)
	GitOpsValidateDryRun() *util.DetailedError
}

type GitOpsConfigDto struct {
	Id               int    `json:"id,omitempty"`
	Provider         string `json:"provider"`
	Username         string `json:"username"`
	Token            string `json:"token"`
	GitLabGroupId    string `json:"gitLabGroupId"`
	GitHubOrgId      string `json:"gitHubOrgId"`
	Host             string `json:"host"`
	Active           bool   `json:"active"`
	AzureProjectName string `json:"azureProjectName"`
	UserId           int32  `json:"-"`
}

const GitOpsSecretName = "devtron-gitops-secret"

//type ValidationResponseForOneTask struct {
//	Successful bool
//	//message = fmt.Sprintf("task name - err string")
//	Message string
//}
//
//type GitopsValidationResponse struct {
//	CreateRepo    ValidationResponseForOneTask
//	Readme        ValidationResponseForOneTask
//	Clone         ValidationResponseForOneTask
//	CommitAndPush ValidationResponseForOneTask
//	CommitOnRest  ValidationResponseForOneTask
//}
type GitOpsConfigServiceImpl struct {
	randSource           rand.Source
	logger               *zap.SugaredLogger
	gitOpsRepository     repository.GitOpsConfigRepository
	K8sUtil              *util.K8sUtil
	aCDAuthConfig        *user.ACDAuthConfig
	clusterService       cluster.ClusterService
	envService           cluster.EnvironmentService
	versionService       argocdServer.VersionService
	gitFactory           *util.GitFactory
	chartTemplateService *util.ChartTemplateServiceImpl
}

func NewGitOpsConfigServiceImpl(Logger *zap.SugaredLogger, ciHandler pipeline.CiHandler,
	gitOpsRepository repository.GitOpsConfigRepository, K8sUtil *util.K8sUtil, aCDAuthConfig *user.ACDAuthConfig,
	clusterService cluster.ClusterService, envService cluster.EnvironmentService, versionService argocdServer.VersionService,
	gitFactory *util.GitFactory, chartTemplateServiceImpl *util.ChartTemplateServiceImpl) *GitOpsConfigServiceImpl {
	return &GitOpsConfigServiceImpl{
		logger:               Logger,
		gitOpsRepository:     gitOpsRepository,
		K8sUtil:              K8sUtil,
		aCDAuthConfig:        aCDAuthConfig,
		clusterService:       clusterService,
		envService:           envService,
		versionService:       versionService,
		gitFactory:           gitFactory,
		chartTemplateService: chartTemplateServiceImpl,
	}
}
func (impl *GitOpsConfigServiceImpl) CreateGitOpsConfig(request *GitOpsConfigDto) (*GitOpsConfigDto, error) {
	impl.logger.Debugw("gitops create request", "req", request)
	dbConnection := impl.gitOpsRepository.GetConnection()
	tx, err := dbConnection.Begin()
	if err != nil {
		return nil, err
	}
	// Rollback tx on error.
	defer tx.Rollback()

	existingModel, err := impl.gitOpsRepository.GetGitOpsConfigActive()
	if err != nil && err != pg.ErrNoRows {
		impl.logger.Errorw("error in creating new gitops config", "error", err)
		return nil, err
	}
	if existingModel != nil && existingModel.Id > 0 {
		existingModel.Active = false
		existingModel.UpdatedOn = time.Now()
		existingModel.UpdatedBy = request.UserId
		err = impl.gitOpsRepository.UpdateGitOpsConfig(existingModel, tx)
		if err != nil {
			impl.logger.Errorw("error in creating new gitops config", "error", err)
			return nil, err
		}
	}
	model := &repository.GitOpsConfig{
		Provider:      strings.ToUpper(request.Provider),
		Username:      request.Username,
		Token:         request.Token,
		GitHubOrgId:   request.GitHubOrgId,
		GitLabGroupId: request.GitLabGroupId,
		Host:          request.Host,
		Active:        true,
		AzureProject:  request.AzureProjectName,
		AuditLog:      models.AuditLog{CreatedBy: request.UserId, CreatedOn: time.Now(), UpdatedOn: time.Now(), UpdatedBy: request.UserId},
	}
	model, err = impl.gitOpsRepository.CreateGitOpsConfig(model, tx)
	if err != nil {
		impl.logger.Errorw("error in saving gitops config", "data", model, "err", err)
		err = &util.ApiError{
			InternalMessage: "gitops config failed to create in db",
			UserMessage:     "gitops config failed to create in db",
		}
		return nil, err
	}

	clusterBean, err := impl.clusterService.FindOne(cluster.ClusterName)
	if err != nil {
		return nil, err
	}
	cfg, err := impl.clusterService.GetClusterConfig(clusterBean)
	if err != nil {
		return nil, err
	}

	client, err := impl.K8sUtil.GetClient(cfg)
	if err != nil {
		return nil, err
	}

	secret, err := impl.K8sUtil.GetSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
	statusError, _ := err.(*errors.StatusError)
	if err != nil && statusError.Status().Code != http.StatusNotFound {
		impl.logger.Errorw("secret not found", "err", err)
		return nil, err
	}
	data := make(map[string][]byte)
	data["username"] = []byte(request.Username)
	data["password"] = []byte(request.Token)
	if secret == nil {
		secret, err = impl.K8sUtil.CreateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, data, GitOpsSecretName, client)
		if err != nil {
			impl.logger.Errorw("err on creating secret", "err", err)
			return nil, err
		}
	} else {
		secret.Data = data
		secret, err = impl.K8sUtil.UpdateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, secret, client)
		if err != nil {
			operationComplete := false
			retryCount := 0
			for !operationComplete && retryCount < 3 {
				retryCount = retryCount + 1
				secret, err := impl.K8sUtil.GetSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
				if err != nil {
					impl.logger.Errorw("secret not found", "err", err)
					return nil, err
				}
				secret.Data = data
				secret, err = impl.K8sUtil.UpdateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, secret, client)
				if err != nil {
					continue
				}
				if err == nil {
					operationComplete = true
				}
			}

		}
	}

	operationComplete := false
	retryCount := 0
	for !operationComplete && retryCount < 3 {
		retryCount = retryCount + 1

		cm, err := impl.K8sUtil.GetConfigMap(impl.aCDAuthConfig.ACDConfigMapNamespace, impl.aCDAuthConfig.ACDConfigMapName, client)
		if err != nil {
			return nil, err
		}
		updatedData := impl.updateData(cm.Data, request, GitOpsSecretName, existingModel.Host)
		data := cm.Data
		data["repository.credentials"] = updatedData["repository.credentials"]
		cm.Data = data
		_, err = impl.K8sUtil.UpdateConfigMap(impl.aCDAuthConfig.ACDConfigMapNamespace, cm, client)
		if err != nil {
			continue
		}
		if err == nil {
			operationComplete = true
		}
	}
	if !operationComplete {
		return nil, fmt.Errorf("resouce version not matched with config map attemped 3 times")
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}
	err = impl.gitFactory.Reload()
	if err != nil {
		return nil, err
	}
	request.Id = model.Id
	return request, nil
}
func (impl *GitOpsConfigServiceImpl) UpdateGitOpsConfig(request *GitOpsConfigDto) error {
	impl.logger.Debugw("gitops config update request", "req", request)
	dbConnection := impl.gitOpsRepository.GetConnection()
	tx, err := dbConnection.Begin()
	if err != nil {
		return err
	}
	// Rollback tx on error.
	defer tx.Rollback()
	model, err := impl.gitOpsRepository.GetGitOpsConfigById(request.Id)
	if err != nil {
		impl.logger.Errorw("No matching entry found for update.", "id", request.Id)
		err = &util.ApiError{
			InternalMessage: "gitops config update failed, does not exist",
			UserMessage:     "gitops config update failed, does not exist",
		}
		return err
	}

	existingModel, err := impl.gitOpsRepository.GetGitOpsConfigActive()
	if err != nil && err != pg.ErrNoRows {
		impl.logger.Errorw("error in creating new gitops config", "error", err)
		return err
	}

	if request.Active {
		if existingModel != nil && existingModel.Id > 0 && existingModel.Id != model.Id {
			existingModel.Active = false
			existingModel.UpdatedOn = time.Now()
			existingModel.UpdatedBy = request.UserId
			err = impl.gitOpsRepository.UpdateGitOpsConfig(existingModel, tx)
			if err != nil {
				impl.logger.Errorw("error in creating new gitops config", "error", err)
				return err
			}
		}
	} else {
		if existingModel == nil || existingModel.Id == 0 {
			return fmt.Errorf("no active config found, please ensure atleast on gitops config active")
		}
	}

	model.Provider = strings.ToUpper(request.Provider)
	model.Username = request.Username
	model.Token = request.Token
	model.GitLabGroupId = request.GitLabGroupId
	model.GitHubOrgId = request.GitHubOrgId
	model.Host = request.Host
	model.Active = request.Active
	model.AzureProject = request.AzureProjectName
	err = impl.gitOpsRepository.UpdateGitOpsConfig(model, tx)
	if err != nil {
		impl.logger.Errorw("error in updating team", "data", model, "err", err)
		err = &util.ApiError{
			InternalMessage: "gitops config failed to update in db",
			UserMessage:     "gitops config failed to update in db",
		}
		return err
	}
	request.Id = model.Id

	clusterBean, err := impl.clusterService.FindOne(cluster.ClusterName)
	if err != nil {
		return err
	}
	cfg, err := impl.clusterService.GetClusterConfig(clusterBean)
	if err != nil {
		return err
	}

	client, err := impl.K8sUtil.GetClient(cfg)
	if err != nil {
		return err
	}

	secret, err := impl.K8sUtil.GetSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
	statusError, _ := err.(*errors.StatusError)
	if err != nil && statusError.Status().Code != http.StatusNotFound {
		impl.logger.Errorw("secret not found", "err", err)
		return err
	}
	data := make(map[string][]byte)
	data["username"] = []byte(request.Username)
	data["password"] = []byte(request.Token)
	if secret == nil {
		secret, err = impl.K8sUtil.CreateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, data, GitOpsSecretName, client)
		if err != nil {
			impl.logger.Errorw("err on creating secret", "err", err)
			return err
		}
	} else {
		secret.Data = data
		secret, err = impl.K8sUtil.UpdateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, secret, client)
		if err != nil {
			operationComplete := false
			retryCount := 0
			for !operationComplete && retryCount < 3 {
				retryCount = retryCount + 1
				secret, err := impl.K8sUtil.GetSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
				if err != nil {
					impl.logger.Errorw("secret not found", "err", err)
					return err
				}
				secret.Data = data
				secret, err = impl.K8sUtil.UpdateSecret(impl.aCDAuthConfig.ACDConfigMapNamespace, secret, client)
				if err != nil {
					continue
				}
				if err == nil {
					operationComplete = true
				}
			}

		}
	}

	operationComplete := false
	retryCount := 0
	for !operationComplete && retryCount < 3 {
		retryCount = retryCount + 1

		cm, err := impl.K8sUtil.GetConfigMap(impl.aCDAuthConfig.ACDConfigMapNamespace, impl.aCDAuthConfig.ACDConfigMapName, client)
		if err != nil {
			return err
		}
		updatedData := impl.updateData(cm.Data, request, GitOpsSecretName, existingModel.Host)
		data := cm.Data
		data["repository.credentials"] = updatedData["repository.credentials"]
		cm.Data = data
		_, err = impl.K8sUtil.UpdateConfigMap(impl.aCDAuthConfig.ACDConfigMapNamespace, cm, client)
		if err != nil {
			continue
		}
		if err == nil {
			operationComplete = true
		}
	}
	if !operationComplete {
		return fmt.Errorf("resouce version not matched with config map attemped 3 times")
	}

	err = tx.Commit()
	if err != nil {
		return err
	}
	err = impl.gitFactory.Reload()
	if err != nil {
		return err
	}
	return nil
}

func (impl *GitOpsConfigServiceImpl) GetGitOpsConfigById(id int) (*GitOpsConfigDto, error) {
	model, err := impl.gitOpsRepository.GetGitOpsConfigById(id)
	if err != nil {
		impl.logger.Errorw("GetGitOpsConfigById, error while get by id", "err", err, "id", id)
		return nil, err
	}
	config := &GitOpsConfigDto{
		Id:               model.Id,
		Provider:         model.Provider,
		GitHubOrgId:      model.GitHubOrgId,
		GitLabGroupId:    model.GitLabGroupId,
		Username:         model.Username,
		Token:            model.Token,
		Host:             model.Host,
		Active:           model.Active,
		UserId:           model.CreatedBy,
		AzureProjectName: model.AzureProject,
	}

	return config, err
}

func (impl *GitOpsConfigServiceImpl) GetAllGitOpsConfig() ([]*GitOpsConfigDto, error) {
	models, err := impl.gitOpsRepository.GetAllGitOpsConfig()
	if err != nil {
		impl.logger.Errorw("GetAllGitOpsConfig, error while fetch all", "err", err)
		return nil, err
	}
	configs := make([]*GitOpsConfigDto, 0)
	for _, model := range models {
		config := &GitOpsConfigDto{
			Id:               model.Id,
			Provider:         model.Provider,
			GitHubOrgId:      model.GitHubOrgId,
			GitLabGroupId:    model.GitLabGroupId,
			Username:         model.Username,
			Token:            model.Token,
			Host:             model.Host,
			Active:           model.Active,
			UserId:           model.CreatedBy,
			AzureProjectName: model.AzureProject,
		}
		configs = append(configs, config)
	}
	return configs, err
}

func (impl *GitOpsConfigServiceImpl) GetGitOpsConfigByProvider(provider string) (*GitOpsConfigDto, error) {
	model, err := impl.gitOpsRepository.GetGitOpsConfigByProvider(provider)
	if err != nil {
		impl.logger.Errorw("GetGitOpsConfigByProvider, error while get by name", "err", err, "provider", provider)
		return nil, err
	}
	config := &GitOpsConfigDto{
		Id:               model.Id,
		Provider:         model.Provider,
		GitHubOrgId:      model.GitHubOrgId,
		GitLabGroupId:    model.GitLabGroupId,
		Username:         model.Username,
		Token:            model.Token,
		Host:             model.Host,
		Active:           model.Active,
		UserId:           model.CreatedBy,
		AzureProjectName: model.AzureProject,
	}

	return config, err
}

func (impl *GitOpsConfigServiceImpl) updateData(data map[string]string, request *GitOpsConfigDto, secretName string, existingHost string) map[string]string {
	var newRepositories []*RepositoryCredentialsDto
	var existingRepositories []*RepositoryCredentialsDto
	repoStr := data["repository.credentials"]
	if len(repoStr) > 0 {
		repoByte, err := yaml.YAMLToJSON([]byte(repoStr))
		if err != nil {
			panic(err)
		}
		err = json.Unmarshal(repoByte, &existingRepositories)
		if err != nil {
			panic(err)
		}
	}

	for _, item := range existingRepositories {
		if item.Url != existingHost {
			newRepositories = append(newRepositories, item)
		}
	}
	repoData := impl.createRepoElement(secretName, request)
	newRepositories = append(newRepositories, repoData)

	rb, err := json.Marshal(newRepositories)
	if err != nil {
		panic(err)
	}
	repositoriesYamlByte, err := yaml.JSONToYAML(rb)
	if err != nil {
		panic(err)
	}
	repositoryCredentials := map[string]string{}
	if len(repositoriesYamlByte) > 0 {
		repositoryCredentials["repository.credentials"] = string(repositoriesYamlByte)
	}
	return repositoryCredentials
}

func (impl *GitOpsConfigServiceImpl) createRepoElement(secretName string, request *GitOpsConfigDto) *RepositoryCredentialsDto {
	repoData := &RepositoryCredentialsDto{}
	usernameSecret := &KeyDto{Name: secretName, Key: "username"}
	passwordSecret := &KeyDto{Name: secretName, Key: "password"}
	repoData.PasswordSecret = passwordSecret
	repoData.UsernameSecret = usernameSecret
	repoData.Url = request.Host
	return repoData
}

type RepositoryCredentialsDto struct {
	Url            string  `json:"url,omitempty"`
	UsernameSecret *KeyDto `json:"usernameSecret,omitempty"`
	PasswordSecret *KeyDto `json:"passwordSecret,omitempty"`
}

type KeyDto struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
}

func (impl *GitOpsConfigServiceImpl) GetGitOpsConfigActive() (*GitOpsConfigDto, error) {
	model, err := impl.gitOpsRepository.GetGitOpsConfigActive()
	if err != nil {
		impl.logger.Errorw("GetGitOpsConfigActive, error while getting error", "err", err)
		return nil, err
	}
	config := &GitOpsConfigDto{
		Id:               model.Id,
		Provider:         model.Provider,
		GitHubOrgId:      model.GitHubOrgId,
		GitLabGroupId:    model.GitLabGroupId,
		Active:           model.Active,
		UserId:           model.CreatedBy,
		AzureProjectName: model.AzureProject,
	}
	return config, err
}

func (impl *GitOpsConfigServiceImpl) GitOpsValidateDryRun() *util.DetailedError {

	var detailedError *util.DetailedError

	//TO ASK
	appName := "random-name"
	baseTemplateName := " "
	version := " "
	tmpChartLocation := " "

	repoUrl, _, detailedErrorCreateRepo := impl.gitFactory.Client.CreateRepository(appName, "helm chart for "+appName)

	detailedError.StageErrorMap = detailedErrorCreateRepo.StageErrorMap
	detailedError.SuccessfulStages = detailedErrorCreateRepo.SuccessfulStages

	if detailedError.StageErrorMap != nil {
		impl.logger.Errorw("error in creating git project", "name", appName, "err", detailedErrorCreateRepo.Err)
		return detailedError
	}

	chartDir := fmt.Sprintf("%s-%s", appName, impl.chartTemplateService.getDir())
	clonedDir := impl.gitFactory.GitService.GetCloneDirectory(chartDir)
	if _, err := os.Stat(clonedDir); os.IsNotExist(err) {
		clonedDir, err = impl.gitFactory.GitService.Clone(repoUrl, chartDir)
		if err != nil {
			impl.logger.Errorw("error in cloning repo", "url", repoUrl, "err", err)
			detailedError.StageErrorMap["clone"] = err
			return detailedError
		}
		detailedError.SuccessfulStages = append(detailedError.SuccessfulStages, "clone")
	}
	dir := filepath.Join(clonedDir, baseTemplateName, version)
	err := os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		impl.logger.Errorw("error in making dir", "err", err)
		// TO ASK
		return nil
	}
	err = dirCopy.Copy(tmpChartLocation, dir)
	if err != nil {
		impl.logger.Errorw("error copying dir", "err", err)
		// TO ASK
		return nil
	}
	commit, err := impl.gitFactory.GitService.CommitAndPushAllChanges(clonedDir, "first commit")
	if err != nil {
			impl.logger.Errorw("error in commit and pushing git", "err", err)
			if commit == ""{
				detailedError.StageErrorMap["commitOnRest"] = err
			} else{
				detailedError.StageErrorMap["push"] = err
			}
		}
	detailedError.SuccessfulStages = append(detailedError.SuccessfulStages,"commitOnRest")
	detailedError.SuccessfulStages = append(detailedError.SuccessfulStages,"push")
	impl.logger.Debugw("template committed", "url", repoUrl, "commit", commit)

	defer impl.chartTemplateService.CleanDir(clonedDir)
	detailedError.ValidatedOn = time.Now()
	return detailedError
}
