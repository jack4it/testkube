package v1

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/kelseyhightower/envconfig"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	executorv1 "github.com/kubeshop/testkube-operator/apis/executor/v1"
	executorsclientv1 "github.com/kubeshop/testkube-operator/client/executors/v1"
	testsclientv2 "github.com/kubeshop/testkube-operator/client/tests/v2"
	testsuitesclientv1 "github.com/kubeshop/testkube-operator/client/testsuites/v1"
	"github.com/kubeshop/testkube/internal/pkg/api/datefilter"
	"github.com/kubeshop/testkube/internal/pkg/api/repository/result"
	"github.com/kubeshop/testkube/internal/pkg/api/repository/testresult"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/cronjob"
	"github.com/kubeshop/testkube/pkg/executor/client"
	"github.com/kubeshop/testkube/pkg/oauth"
	"github.com/kubeshop/testkube/pkg/secret"
	"github.com/kubeshop/testkube/pkg/server"
	"github.com/kubeshop/testkube/pkg/storage"
	"github.com/kubeshop/testkube/pkg/storage/minio"
	"github.com/kubeshop/testkube/pkg/webhook"
)

func NewTestkubeAPI(
	namespace string,
	executionsResults result.Repository,
	testExecutionsResults testresult.Repository,
	testsClient *testsclientv2.TestsClient,
	executorsClient *executorsclientv1.ExecutorsClient,
	testsuitesClient *testsuitesclientv1.TestSuitesClient,
	secretClient *secret.Client,
	webhookClient *executorsclientv1.WebhooksClient,
	clusterId string,
) TestkubeAPI {

	var httpConfig server.Config
	err := envconfig.Process("APISERVER", &httpConfig)
	// Do we want to panic here or just ignore the err
	if err != nil {
		panic(err)
	}

	// you can disable analytics tracking for API server
	analyticsEnabledStr := os.Getenv("TESTKUBE_ANALYTICS_ENABLED")
	analyticsEnabled, err := strconv.ParseBool(analyticsEnabledStr)
	if err != nil {
		analyticsEnabled = true
	}

	s := TestkubeAPI{
		HTTPServer:           server.NewServer(httpConfig),
		TestExecutionResults: testExecutionsResults,
		ExecutionResults:     executionsResults,
		TestsClient:          testsClient,
		ExecutorsClient:      executorsClient,
		SecretClient:         secretClient,
		TestsSuitesClient:    testsuitesClient,
		Metrics:              NewMetrics(),
		EventsEmitter:        webhook.NewEmitter(),
		WebhooksClient:       webhookClient,
		Namespace:            namespace,
		AnalyticsEnabled:     analyticsEnabled,
		ClusterID:            clusterId,
	}

	initImage, err := s.loadDefaultExecutors(s.Namespace, os.Getenv("TESTKUBE_DEFAULT_EXECUTORS"))
	if err != nil {
		s.Log.Warnf("load default executors %w", err)
	}

	if err = s.jobTemplates.decodeFromEnv(); err != nil {
		panic(err)
	}

	if s.Executor, err = client.NewJobExecutor(executionsResults, s.Namespace, initImage, s.jobTemplates.Job); err != nil {
		panic(err)
	}

	s.CronJobClient, err = cronjob.NewClient(httpConfig.Fullname, httpConfig.Port, s.jobTemplates.Cronjob, s.Namespace)
	if err != nil {
		panic(err)
	}

	s.Init()
	return s
}

type TestkubeAPI struct {
	server.HTTPServer
	ExecutionResults     result.Repository
	TestExecutionResults testresult.Repository
	Executor             client.Executor
	TestsSuitesClient    *testsuitesclientv1.TestSuitesClient
	TestsClient          *testsclientv2.TestsClient
	ExecutorsClient      *executorsclientv1.ExecutorsClient
	SecretClient         *secret.Client
	WebhooksClient       *executorsclientv1.WebhooksClient
	EventsEmitter        *webhook.Emitter
	CronJobClient        *cronjob.Client
	Metrics              Metrics
	Storage              storage.Client
	storageParams        storageParams
	jobTemplates         jobTemplates
	Namespace            string
	AnalyticsEnabled     bool
	ClusterID            string
	oauthParams          oauthParams
}

type jobTemplates struct {
	Job     string
	Cronjob string
}

func (j *jobTemplates) decodeFromEnv() error {
	err := envconfig.Process("TESTKUBE_TEMPLATE", j)
	if err != nil {
		return err
	}
	templates := []*string{&j.Job, &j.Cronjob}
	for i := range templates {
		if *templates[i] != "" {
			dataDecoded, err := base64.StdEncoding.DecodeString(*templates[i])
			if err != nil {
				return err
			}

			*templates[i] = string(dataDecoded)
		}
	}

	return nil
}

type storageParams struct {
	SSL             bool
	Endpoint        string
	AccessKeyId     string
	SecretAccessKey string
	Location        string
	Token           string
}

type oauthParams struct {
	ClientID     string
	ClientSecret string
	Provider     oauth.ProviderType
	Scopes       string
}

// Init initializes api server settings
func (s TestkubeAPI) Init() {
	if err := envconfig.Process("STORAGE", &s.storageParams); err != nil {
		s.Log.Infow("Processing STORAGE environment config", err)
	}

	if err := envconfig.Process("TESTKUBE_OAUTH", &s.oauthParams); err != nil {
		s.Log.Infow("Processing TESTKUBE_OAUTH environment config", err)
	}

	s.Storage = minio.NewClient(s.storageParams.Endpoint, s.storageParams.AccessKeyId, s.storageParams.SecretAccessKey, s.storageParams.Location, s.storageParams.Token, s.storageParams.SSL)

	s.Routes.Static("/api-docs", "./api/v1")
	s.Routes.Use(cors.New())
	s.Routes.Use(s.AuthHandler())

	if s.AnalyticsEnabled {
		// global analytics tracking send async
		s.Routes.Use(s.AnalyticsHandler())
	}

	s.Routes.Get("/info", s.InfoHandler())
	s.Routes.Get("/routes", s.RoutesHandler())

	executors := s.Routes.Group("/executors")

	executors.Post("/", s.CreateExecutorHandler())
	executors.Get("/", s.ListExecutorsHandler())
	executors.Get("/:name", s.GetExecutorHandler())
	executors.Delete("/:name", s.DeleteExecutorHandler())
	executors.Delete("/", s.DeleteExecutorsHandler())

	webhooks := s.Routes.Group("/webhooks")

	webhooks.Post("/", s.CreateWebhookHandler())
	webhooks.Get("/", s.ListWebhooksHandler())
	webhooks.Get("/:name", s.GetWebhookHandler())
	webhooks.Delete("/:name", s.DeleteWebhookHandler())
	webhooks.Delete("/", s.DeleteWebhooksHandler())

	executions := s.Routes.Group("/executions")

	executions.Get("/", s.ListExecutionsHandler())
	executions.Post("/", s.ExecuteTestsHandler())
	executions.Get("/:executionID", s.GetExecutionHandler())
	executions.Get("/:executionID/artifacts", s.ListArtifactsHandler())
	executions.Get("/:executionID/logs", s.ExecutionLogsHandler())
	executions.Get("/:executionID/artifacts/:filename", s.GetArtifactHandler())

	tests := s.Routes.Group("/tests")

	tests.Get("/", s.ListTestsHandler())
	tests.Post("/", s.CreateTestHandler())
	tests.Patch("/:id", s.UpdateTestHandler())
	tests.Delete("/", s.DeleteTestsHandler())

	tests.Get("/:id", s.GetTestHandler())
	tests.Delete("/:id", s.DeleteTestHandler())

	tests.Post("/:id/executions", s.ExecuteTestsHandler())

	tests.Get("/:id/executions", s.ListExecutionsHandler())
	tests.Get("/:id/executions/:executionID", s.GetExecutionHandler())
	tests.Delete("/:id/executions/:executionID", s.AbortExecutionHandler())

	testWithExecutions := s.Routes.Group("/test-with-executions")
	testWithExecutions.Get("/", s.ListTestWithExecutionsHandler())
	testWithExecutions.Get("/:id", s.GetTestWithExecutionHandler())

	testsuites := s.Routes.Group("/test-suites")

	testsuites.Post("/", s.CreateTestSuiteHandler())
	testsuites.Patch("/:id", s.UpdateTestSuiteHandler())
	testsuites.Get("/", s.ListTestSuitesHandler())
	testsuites.Delete("/", s.DeleteTestSuitesHandler())
	testsuites.Get("/:id", s.GetTestSuiteHandler())
	testsuites.Delete("/:id", s.DeleteTestSuiteHandler())

	testsuites.Post("/:id/executions", s.ExecuteTestSuitesHandler())
	testsuites.Get("/:id/executions", s.ListTestSuiteExecutionsHandler())
	testsuites.Get("/:id/executions/:executionID", s.GetTestSuiteExecutionHandler())

	testExecutions := s.Routes.Group("/test-suite-executions")
	testExecutions.Get("/", s.ListTestSuiteExecutionsHandler())
	testExecutions.Post("/", s.ExecuteTestSuitesHandler())
	testExecutions.Get("/:executionID", s.GetTestSuiteExecutionHandler())

	testSuiteWithExecutions := s.Routes.Group("/test-suite-with-executions")
	testSuiteWithExecutions.Get("/", s.ListTestSuiteWithExecutionsHandler())
	testSuiteWithExecutions.Get("/:id", s.GetTestSuiteWithExecutionHandler())

	labels := s.Routes.Group("/labels")
	labels.Get("/", s.ListLabelsHandler())

	slack := s.Routes.Group("/slack")
	slack.Get("/", s.OauthHandler())

	s.EventsEmitter.RunWorkers()
	s.HandleEmitterLogs()

	// mount everything on results
	// TODO it should be named /api/ + dashboard refactor
	s.Mux.Mount("/results", s.Mux)

	s.Log.Infow("Testkube API configured", "namespace", s.Namespace, "clusterId", s.ClusterID)
}

// TODO should we use single generic filter for all list based resources ?
// currently filters for e.g. tests are done "by hand"
func getFilterFromRequest(c *fiber.Ctx) result.Filter {

	filter := result.NewExecutionsFilter()

	// id for /tests/ID/executions
	testName := c.Params("id", "")
	if testName == "" {
		// query param for /executions?testName
		testName = c.Query("testName", "")
	}

	if testName != "" {
		filter = filter.WithTestName(testName)
	}

	textSearch := c.Query("textSearch", "")
	if textSearch != "" {
		filter = filter.WithTextSearch(textSearch)
	}

	page, err := strconv.Atoi(c.Query("page", ""))
	if err == nil {
		filter = filter.WithPage(page)
	}

	pageSize, err := strconv.Atoi(c.Query("pageSize", ""))
	if err == nil && pageSize != 0 {
		filter = filter.WithPageSize(pageSize)
	}

	status := c.Query("status", "")
	if status != "" {
		filter = filter.WithStatus(status)
	}

	objectType := c.Query("type", "")
	if objectType != "" {
		filter = filter.WithType(objectType)
	}

	dFilter := datefilter.NewDateFilter(c.Query("startDate", ""), c.Query("endDate", ""))
	if dFilter.IsStartValid {
		filter = filter.WithStartDate(dFilter.Start)
	}

	if dFilter.IsEndValid {
		filter = filter.WithEndDate(dFilter.End)
	}

	selector := c.Query("selector")
	if selector != "" {
		filter = filter.WithSelector(selector)
	}

	return filter
}

// loadDefaultExecutors loads default executors
func (s TestkubeAPI) loadDefaultExecutors(namespace, data string) (initImage string, err error) {
	var executors []testkube.ExecutorDetails

	if data == "" {
		return "", nil
	}

	dataDecoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", err
	}

	if err := json.Unmarshal([]byte(dataDecoded), &executors); err != nil {
		return "", err
	}

	for _, executor := range executors {
		if executor.Executor == nil {
			continue
		}

		if executor.Name == "executor-init" {
			initImage = executor.Executor.Image
			continue
		}

		obj := &executorv1.Executor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      executor.Name,
				Namespace: namespace,
			},
			Spec: executorv1.ExecutorSpec{
				Types:        executor.Executor.Types,
				ExecutorType: executor.Executor.ExecutorType,
				Image:        executor.Executor.Image,
			},
		}

		result, err := s.ExecutorsClient.Get(executor.Name)
		if err != nil && !errors.IsNotFound(err) {
			return "", err
		}

		if err != nil {
			if _, err = s.ExecutorsClient.Create(obj); err != nil {
				return "", err
			}
		} else {
			result.Spec = obj.Spec
			if _, err = s.ExecutorsClient.Update(result); err != nil {
				return "", err
			}
		}
	}

	return initImage, nil
}
