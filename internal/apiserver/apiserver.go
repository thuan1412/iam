// Copyright 2020 Lingfei Kong <colin404@foxmail.com>. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

// Package apiserver does all of the work necessary to create a iam APIServer.
package apiserver

import (
	"context"
	"fmt"

	pb "github.com/marmotedu/api/proto/apiserver/v1"
	cliflag "github.com/marmotedu/component-base/pkg/cli/flag"
	"github.com/marmotedu/component-base/pkg/cli/globalflag"
	"github.com/marmotedu/component-base/pkg/term"
	"github.com/marmotedu/component-base/pkg/util/idutil"
	"github.com/marmotedu/component-base/pkg/version"
	"github.com/marmotedu/component-base/pkg/version/verflag"
	"github.com/marmotedu/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	cachev1 "github.com/marmotedu/iam/internal/apiserver/api/v1/cache"
	"github.com/marmotedu/iam/internal/apiserver/options"
	"github.com/marmotedu/iam/internal/apiserver/store/mysql"
	genericoptions "github.com/marmotedu/iam/internal/pkg/options"
	genericapiserver "github.com/marmotedu/iam/internal/pkg/server"
	"github.com/marmotedu/iam/pkg/log"
	"github.com/marmotedu/iam/pkg/shutdown"
	"github.com/marmotedu/iam/pkg/shutdown/shutdownmanagers/posixsignal"
	"github.com/marmotedu/iam/pkg/storage"
)

const (
	// recommendedFileName defines the configuration used by iam-apiserver.
	// the configuration file is different from other iam service.
	recommendedFileName = "iam-apiserver.yaml"

	// appName defines the executable binary filename for iam-apiserver component.
	appName = "iam-apiserver"
)

// NewAPIServerCommand creates a *cobra.Command object with default parameters.
func NewAPIServerCommand() *cobra.Command {
	cliflag.InitFlags()

	s := options.NewServerRunOptions()

	cmd := &cobra.Command{
		Use:   appName,
		Short: "The IAM API server to validates and configures data for the api objects",
		Long: `The IAM API server validates and configures data
for the api objects which include users, policies, secrets, and
others. The API Server services REST operations to do the api objects management.

Find more iam-apiserver information at:
    https://github.com/marmotedu/iam/blob/master/docs/guide/en-US/cmd/iam-apiserver.md`,

		// stop printing usage when the command errors
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())

			if err := viper.BindPFlags(cmd.Flags()); err != nil {
				return err
			}

			// set default options
			completedOptions, err := complete(s)
			if err != nil {
				return err
			}

			// validate options
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return errors.NewAggregate(errs)
			}

			log.InitWithOptions(completedOptions.Log)
			defer log.Flush()

			return completedOptions.Run()
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name())
	fs := cmd.Flags()
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})

	return cmd
}

// Run runs the specified APIServer. This should never exit.
func (completedOptions *completedServerRunOptions) Run() error {
	// To help debugging, immediately log config and version
	log.Debugf("config: `%s`", completedOptions.String())
	log.Debugf("version: %+v", version.Get().ToJSON())

	// initialize graceful shutdown
	gs := shutdown.New()
	gs.AddShutdownManager(posixsignal.NewPosixSignalManager())

	if err := completedOptions.Init(gs); err != nil {
		return err
	}

	serverConfig, err := createAPIServerConfig(completedOptions.ServerRunOptions)
	if err != nil {
		return err
	}

	server, err := serverConfig.Complete().New()
	if err != nil {
		return err
	}

	return server.Run(gs)
}

// ExtraConfig defines extra configuration for the iam-apiserver.
type ExtraConfig struct {
	Addr       string
	MaxMsgSize int
	ServerCert genericoptions.GeneratableKeyCert
}

type completedExtraConfig struct {
	*ExtraConfig
}

// apiServerConfig defines configuration for the iam-apiserver.
type apiServerConfig struct {
	GenericConfig *genericapiserver.Config
	ExtraConfig   ExtraConfig
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   completedExtraConfig
}

// APIServer is only responsible for serving the APIs for iam-apiserver.
type APIServer struct {
	GRPCAPIServer    *grpcAPIServer
	GenericAPIServer *genericapiserver.GenericAPIServer
}

// Complete fills in any fields not set that are required to have valid data and can be derived from other fields.
func (c *ExtraConfig) Complete() completedExtraConfig {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8081"
	}

	return completedExtraConfig{c}
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *apiServerConfig) Complete() completedConfig {
	return completedConfig{
		c.GenericConfig.Complete(),
		c.ExtraConfig.Complete(),
	}
}

// New returns a new instance of APIServer from the given config.
// Certain config fields will be set to a default value if unset.
func (c completedConfig) New() (*APIServer, error) {
	genericServer, err := c.GenericConfig.New()
	if err != nil {
		return nil, err
	}
	initRouter(genericServer.Engine)

	grpcServer := c.ExtraConfig.New()

	s := &APIServer{
		GenericAPIServer: genericServer,
		GRPCAPIServer:    grpcServer,
	}

	return s, nil
}

// New create a grpcAPIServer instance.
func (c *ExtraConfig) New() *grpcAPIServer {
	creds, err := credentials.NewServerTLSFromFile(c.ServerCert.CertKey.CertFile, c.ServerCert.CertKey.KeyFile)
	if err != nil {
		log.Fatalf("Failed to generate credentials %s", err.Error())
	}
	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(c.MaxMsgSize), grpc.Creds(creds)}
	grpcServer := grpc.NewServer(opts...)

	storeIns, _ := mysql.GetMySQLFactoryOr(nil)
	cacheIns, err := cachev1.GetCacheInsOr(storeIns)
	if err != nil {
		log.Fatalf("Failed to get cache instance: %s", err.Error())
	}

	pb.RegisterCacheServer(grpcServer, cacheIns)

	reflection.Register(grpcServer)

	return &grpcAPIServer{grpcServer, c.Addr}
}

// Run start the APIServer.
func (s *APIServer) Run(gs *shutdown.GracefulShutdown) error {
	// run grpc server
	go s.GRPCAPIServer.Run()

	gs.AddShutdownCallback(shutdown.ShutdownFunc(func(string) error {
		s.GRPCAPIServer.Close()
		s.GenericAPIServer.Close()

		return nil
	}))

	// start shutdown managers
	if err := gs.Start(); err != nil {
		log.Fatalf("start shutdown manager failed: %s", err.Error())
	}

	return s.GenericAPIServer.Run()
}

func buildGenericConfig(s *options.ServerRunOptions) (genericConfig *genericapiserver.Config, lastErr error) {
	genericConfig = genericapiserver.NewConfig()
	if lastErr = s.GenericServerRunOptions.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	if lastErr = s.FeatureOptions.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	if lastErr = s.SecureServing.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	if lastErr = s.InsecureServing.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	return
}

// createAPIServerConfig create apiserver config from all options.
func createAPIServerConfig(s *options.ServerRunOptions) (*apiServerConfig, error) {
	genericConfig, err := buildGenericConfig(s)
	if err != nil {
		return nil, err
	}

	config := &apiServerConfig{
		GenericConfig: genericConfig,
		ExtraConfig: ExtraConfig{
			Addr:       fmt.Sprintf("%s:%d", s.GRPCOptions.BindAddress, s.GRPCOptions.BindPort),
			MaxMsgSize: s.GRPCOptions.MaxMsgSize,
			ServerCert: s.SecureServing.ServerCert,
		},
	}

	return config, nil
}

// completedServerRunOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedServerRunOptions struct {
	*options.ServerRunOptions
}

// complete set default ServerRunOptions.
// Should be called after iam-apiserver flags parsed.
func complete(s *options.ServerRunOptions) (completedServerRunOptions, error) {
	var options completedServerRunOptions

	genericapiserver.LoadConfig(s.APIConfig, recommendedFileName)

	if err := viper.Unmarshal(s); err != nil {
		return options, err
	}

	if s.JwtOptions.Key == "" {
		s.JwtOptions.Key = idutil.NewSecretKey()
	}

	if err := s.SecureServing.Complete(); err != nil {
		return options, err
	}

	options.ServerRunOptions = s

	return options, nil
}

func (completedOptions completedServerRunOptions) Init(gs *shutdown.GracefulShutdown) error {
	if err := completedOptions.InitDataStore(); err != nil {
		log.Warnf("init datastore: %s", err)
	}

	gs.AddShutdownCallback(shutdown.ShutdownFunc(func(string) error {
		mysqlStore, _ := mysql.GetMySQLFactoryOr(nil)
		if mysqlStore != nil {
			return mysqlStore.Close()
		}

		return nil
	}))

	return nil
}

func (completedOptions completedServerRunOptions) InitDataStore() error {
	completedOptions.InitRedisStore()

	_, err := mysql.GetMySQLFactoryOr(completedOptions.MySQLOptions)
	if err != nil {
		return err
	}

	// uncomment the following lines if you want to switch to etcd storage.
	/*
		if _, err := etcd.GetEtcdFactoryOr(completedOptions.EtcdOptions, nil); err != nil {
			return err
		}
	*/
	// store.SetClient(mysqlStore)
	return nil
}

func (completedOptions completedServerRunOptions) InitRedisStore() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	config := &storage.Config{
		Host:                  completedOptions.RedisOptions.Host,
		Port:                  completedOptions.RedisOptions.Port,
		Addrs:                 completedOptions.RedisOptions.Addrs,
		MasterName:            completedOptions.RedisOptions.MasterName,
		Username:              completedOptions.RedisOptions.Username,
		Password:              completedOptions.RedisOptions.Password,
		Database:              completedOptions.RedisOptions.Database,
		MaxIdle:               completedOptions.RedisOptions.MaxIdle,
		MaxActive:             completedOptions.RedisOptions.MaxActive,
		Timeout:               completedOptions.RedisOptions.Timeout,
		EnableCluster:         completedOptions.RedisOptions.EnableCluster,
		UseSSL:                completedOptions.RedisOptions.UseSSL,
		SSLInsecureSkipVerify: completedOptions.RedisOptions.SSLInsecureSkipVerify,
	}

	// try to connect to redis
	go storage.ConnectToRedis(ctx, config)
}
