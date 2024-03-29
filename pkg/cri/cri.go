/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cri

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/cri/sbserver"
	"github.com/containerd/containerd/pkg/nri"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/klog/v2"

	criconfig "github.com/containerd/containerd/pkg/cri/config"
	"github.com/containerd/containerd/pkg/cri/constants"
	"github.com/containerd/containerd/pkg/cri/server"
)

// Register CRI service plugin
func init() {
	config := criconfig.DefaultConfig()
	plugin.Register(&plugin.Registration{
		Type:   plugin.GRPCPlugin,
		ID:     "cri",
		Config: &config,
		Requires: []plugin.Type{
			plugin.EventPlugin,
			plugin.ServicePlugin,
			plugin.NRIApiPlugin,
		},
		InitFn: initCRIService,
	})
}

func initCRIService(ic *plugin.InitContext) (interface{}, error) {
	ic.Meta.Platforms = []imagespec.Platform{platforms.DefaultSpec()}
	ic.Meta.Exports = map[string]string{"CRIVersion": constants.CRIVersion, "CRIVersionAlpha": constants.CRIVersionAlpha}
	ctx := ic.Context
	pluginConfig := ic.Config.(*criconfig.PluginConfig)
	if err := criconfig.ValidatePluginConfig(ctx, pluginConfig); err != nil {
		return nil, fmt.Errorf("invalid plugin config: %w", err)
	}

	c := criconfig.Config{
		PluginConfig:       *pluginConfig,
		ContainerdRootDir:  filepath.Dir(ic.Root),
		ContainerdEndpoint: ic.Address,
		RootDir:            ic.Root,
		StateDir:           ic.State,
	}
	log.G(ctx).Infof("Start cri plugin with config %+v", c)

	if err := setGLogLevel(); err != nil {
		return nil, fmt.Errorf("failed to set glog level: %w", err)
	}

	log.G(ctx).Info("Connect containerd service")
	client, err := containerd.New(
		"",
		containerd.WithDefaultNamespace(constants.K8sContainerdNamespace),
		containerd.WithDefaultPlatform(platforms.Default()),
		containerd.WithInMemoryServices(ic),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create containerd client: %w", err)
	}

	var s server.CRIService
	var nrip nri.API
	if os.Getenv("ENABLE_CRI_SANDBOXES") != "" {
		log.G(ctx).Info("using experimental CRI Sandbox server - unset ENABLE_CRI_SANDBOXES to disable")
		s, err = sbserver.NewCRIService(c, client)
	} else {
		log.G(ctx).Info("using legacy CRI server")

		nrip, err = getNRIPlugin(ic)
		if err != nil {
			log.G(ctx).Info("NRI service not found, disabling NRI support")
		}

		s, err = server.NewCRIService(c, client, nrip)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create CRI service: %w", err)
	}

	go func() {
		if err := s.Run(); err != nil {
			log.G(ctx).WithError(err).Fatal("Failed to run CRI service")
		}
		// TODO(random-liu): Whether and how we can stop containerd.
	}()

	if nrip != nil {
		log.G(ctx).Info("using experimental NRI integration - disable nri plugin to prevent this")
		if err = nrip.Start(); err != nil {
			log.G(ctx).WithError(err).Fatal("Failed to start NRI service")
		}
	}

	return s, nil
}

// Set glog level.
func setGLogLevel() error {
	l := logrus.GetLevel()
	fs := flag.NewFlagSet("klog", flag.PanicOnError)
	klog.InitFlags(fs)
	if err := fs.Set("logtostderr", "true"); err != nil {
		return err
	}
	switch l {
	case logrus.TraceLevel:
		return fs.Set("v", "5")
	case logrus.DebugLevel:
		return fs.Set("v", "4")
	case logrus.InfoLevel:
		return fs.Set("v", "2")
	// glog doesn't support following filters. Defaults to v=0.
	case logrus.WarnLevel:
	case logrus.ErrorLevel:
	case logrus.FatalLevel:
	case logrus.PanicLevel:
	}
	return nil
}

// Get the NRI plugin and verify its type.
func getNRIPlugin(ic *plugin.InitContext) (nri.API, error) {
	const (
		pluginType = plugin.NRIApiPlugin
		pluginName = "nri"
	)

	p, err := ic.GetByID(pluginType, pluginName)
	if err != nil {
		return nil, err
	}

	api, ok := p.(nri.API)
	if !ok {
		return nil, fmt.Errorf("NRI plugin (%s, %q) has incompatible type %T",
			pluginType, pluginName, api)
	}

	return api, nil
}
