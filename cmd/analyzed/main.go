package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/supergiant/robot"
	"github.com/supergiant/robot/builtin/plugins/underutilizednodes"
	"github.com/supergiant/robot/builtin/plugins/requestslimitscheck"
	"github.com/supergiant/robot/pkg/api"
	"github.com/supergiant/robot/pkg/api/handlers"
	"github.com/supergiant/robot/pkg/api/operations"
	"github.com/supergiant/robot/pkg/config"
	"github.com/supergiant/robot/pkg/logger"
	"github.com/supergiant/robot/pkg/models"
	"github.com/supergiant/robot/pkg/plugin"
	"github.com/supergiant/robot/pkg/plugin/proto"
	"github.com/supergiant/robot/pkg/scheduler"
	"github.com/supergiant/robot/pkg/storage"
	"github.com/supergiant/robot/pkg/storage/etcd"

	"github.com/go-openapi/loads"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {

	command := &cobra.Command{
		Use:          "analyzed",
		Short:        "analyze service checks K8s cluster by means of installed plugins and gives recommendations",
		RunE:         runCommand,
		SilenceUsage: true,
	}

	command.PersistentFlags().StringArrayP(
		"config",
		"c",
		[]string{"./config.yaml", "/etc/analyzed/config.yaml", "$HOME/.analyzed/config.yaml"},
		"config file path")

	if err := command.Execute(); err != nil {
		log.Fatalf("\n%v\n", err)
	}
}

func runCommand(cmd *cobra.Command, _ []string) error {
	configFilePaths, err := cmd.Flags().GetStringArray("config")
	if err != nil {
		return errors.Wrap(err, "unable to get config flag value")
	}

	cfg := &robot.Config{}

	// configFileReadError is not critical due to possibility that configuration is done by environment variables
	configFileReadError := config.ReadFromFiles(cfg, configFilePaths)

	if err = config.MergeEnv("AZ", cfg); err != nil {
		return errors.Wrap(err, "unable to merge env variables")
	}

	//TODO: try to unify APIs discovery which are hosted in k8s
	//TODO: and rewrite config population logic
	if etcdEndpoint := discoverETCDEndpoint(); etcdEndpoint != "" {
		cfg.ETCD.Endpoints = append(cfg.ETCD.Endpoints, discoverETCDEndpoint())
	}
	if strings.TrimSpace(discoverKubeAPIServerURI()) != "" {
		cfg.K8sAPIServerURI = discoverKubeAPIServerURI()
	}

	log := logger.NewLogger(cfg.Logging).WithField("app", "robot")
	mainLogger := log.WithField("component", "main")

	logEnvs(mainLogger)
	mainLogger.Infof("config: %+v", cfg)
	mainLogger.Infof("config file name: %s", config.UsedFileName())
	if configFileReadError != nil {
		mainLogger.Warnf("unable to read config file, %v", configFileReadError)
	}

	if err := cfg.Validate(); err != nil {
		return errors.Wrap(err, "config validation error")
	}

	etcdStorage, err := etcd.NewETCDStorage(cfg.ETCD)
	if err != nil {
		return errors.Wrap(err, "unable to create ETCD client")
	}

	defer etcdStorage.Close()

	plugins := make(plugin.PluginsSet)
	plugins.Load(underutilizednodes.NewPlugin())
	plugins.Load(requestslimitscheck.NewPlugin())

	//TODO: refactor and move this logic from to the plugin loading subsystem
	for pluginName, plugin := range plugins {
		ctx, _ := context.WithTimeout(context.Background(), cfg.Plugin.CheckTimeout)
		pluginInfo, err := plugin.Info(ctx, &proto.Empty{})
		if err != nil {
			mainLogger.Errorf("unable to load plugin, name: %v, error %v", pluginName, err)
		}

		b, err := (&models.RecommendationPlugin{
			Description: pluginInfo.Description,
			ID:          pluginInfo.Id,
			InstalledAt: time.Now().String(),
			Name:        pluginInfo.Name,
			Status:      "OK", // TODO: add status to proto, than implement plugins state which will reflect it's status
			Version:     pluginInfo.Version,
		}).MarshalBinary()
		if err != nil {
			mainLogger.Errorf("unable to load plugin, name: %v, error %v", pluginName, err)
		}

		err = etcdStorage.Put(ctx, models.PluginPrefix, pluginName, b)
		if err != nil {
			mainLogger.Errorf("unable to load plugin, name: %v, error %v", pluginName, err)
		}
	}

	check := func(p plugin.PluginsSet, stor storage.Interface, logger logrus.FieldLogger) func() {
		return func() {
			for pluginID, pluginClient := range p {
				ctx, cancel := context.WithTimeout(context.Background(), cfg.Plugin.CheckTimeout)
				checkResponse, err := pluginClient.Check(ctx, &proto.CheckRequest{})
				if err != nil {
					logger.Errorf("unable to execute check for plugin: %s, error: %v", pluginID, err)
					cancel()
					continue
				}
				if checkResponse.Error != "" {
					logger.Errorf("plugin: %s, returned error: %s", pluginID, checkResponse.Error)
					cancel()
					continue
				}

				if checkResponse.Result == nil {
					logger.Errorf("plugin: %s, returned nil Result", pluginID)
					cancel()
					continue
				}

				r := checkResponse.Result

				var actions = []*models.PluginAction{}
				for _, action := range r.Actions {
					actions = append(actions, &models.PluginAction{
						Description: action.Description,
						ID:          action.ActionId,
					})
				}
				currentTime := time.Now()
				checkResult := models.CheckResult{
					CheckStatus:     r.GetStatus().String(),
					CompletedAt:     currentTime.String(),
					Description:     r.GetDescription(),
					ExecutionStatus: r.GetExecutionStatus(),
					ID:              r.GetName(),
					Name:            r.GetName(),
					PossibleActions: actions,
				}

				bytes, err := checkResult.MarshalBinary()
				if err != nil {
					logger.Errorf("unable to marshal check result, plugin: %s, returned error: %s", pluginID, err)
					cancel()
					continue
				}

				err = stor.Put(ctx, models.CheckResultPrefix, pluginID, bytes)
				if err != nil {
					logger.Errorf("unable to store check result, plugin: %s, returned error: %s", pluginID, err)
					cancel()
				}

				cancel()
			}
		}
	}(plugins, etcdStorage, log.WithField("component", "pluginsChecks"))

	scheduler := scheduler.NewScheduler(cfg.Plugin.CheckInterval, check)
	defer scheduler.Stop()

	swaggerSpec, err := loads.Analyzed(api.SwaggerJSON, "2.0")
	if err != nil {
		return errors.Wrap(err, "unable to create spec analyzed document")
	}

	//TODO: add request logging middleware
	//TODO: add metrics middleware
	analyzeAPI := operations.NewAnalyzeAPI(swaggerSpec)
	analyzeAPI.GetRecommendationPluginsHandler = handlers.NewRecommendationPluginsHandler(
		etcdStorage,
		log.WithField("handler", "RecommendationPluginsHandler"),
	)
	analyzeAPI.GetCheckResultsHandler = handlers.NewCheckResultsHandler(
		etcdStorage,
		log.WithField("handler", "CheckResultsHandler"),
	)

	server := api.NewServer(analyzeAPI)
	server.Port = cfg.API.ServerPort
	server.Host = cfg.API.ServerHost
	server.ConfigureAPI()

	defer server.Shutdown()

	if servingError := server.Serve(); servingError != nil {
		return errors.Wrap(servingError, "unable to serve HTTP API")
	}

	return nil
}

func logEnvs(logger logrus.FieldLogger) {
	for _, pair := range os.Environ() {
		logger.Warnf("%s", pair)
	}
}

func discoverETCDEndpoint() string {
	etcdHost, hostExists := os.LookupEnv("ETCD_SERVICE_HOST")
	etcdPort, portExists := os.LookupEnv("ETCD_SERVICE_PORT")
	if !hostExists || !portExists {
		return ""
	}
	return etcdHost + ":" + etcdPort
}

func discoverKubeAPIServerURI() string {
	kubeAPIServerHost, hostExists := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	kubeAPIServerPort, portExists := os.LookupEnv("KUBERNETES_SERVICE_PORT")
	if !hostExists || !portExists {
		return ""
	}
	return kubeAPIServerHost + ":" + kubeAPIServerPort
}