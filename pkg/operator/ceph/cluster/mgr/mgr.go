/*
Copyright 2016 The Rook Authors. All rights reserved.

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

// Package mgr for the Ceph manager.
package mgr

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/coreos/pkg/capnslog"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha2"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	cephconfig "github.com/rook/rook/pkg/daemon/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	"github.com/rook/rook/pkg/operator/ceph/config"
	opspec "github.com/rook/rook/pkg/operator/ceph/spec"
	cephver "github.com/rook/rook/pkg/operator/ceph/version"
	"github.com/rook/rook/pkg/operator/k8sutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "op-mgr")

var prometheusRuleName = "prometheus-ceph-vVERSION-rules"

const (
	appName              = "rook-ceph-mgr"
	serviceAccountName   = "rook-ceph-mgr"
	prometheusModuleName = "prometheus"
	metricsPort          = 9283
	monitoringPath       = "/etc/ceph-monitoring/"
	serviceMonitorFile   = "service-monitor.yaml"
	// minimum amount of memory in MB to run the pod
	cephMgrPodMinimumMemory uint64 = 512
)

// Cluster represents the Rook and environment configuration settings needed to set up Ceph mgrs.
type Cluster struct {
	clusterInfo     *cephconfig.ClusterInfo
	Namespace       string
	Replicas        int
	placement       rookalpha.Placement
	annotations     rookalpha.Annotations
	context         *clusterd.Context
	dataDir         string
	Network         cephv1.NetworkSpec
	resources       v1.ResourceRequirements
	ownerRef        metav1.OwnerReference
	dashboard       cephv1.DashboardSpec
	monitoringSpec  cephv1.MonitoringSpec
	cephVersion     cephv1.CephVersionSpec
	rookVersion     string
	exitCode        func(err error) (int, bool)
	dataDirHostPath string
	isUpgrade       bool
}

// New creates an instance of the mgr
func New(
	clusterInfo *cephconfig.ClusterInfo,
	context *clusterd.Context,
	namespace, rookVersion string,
	cephVersion cephv1.CephVersionSpec,
	placement rookalpha.Placement,
	annotations rookalpha.Annotations,
	network cephv1.NetworkSpec,
	dashboard cephv1.DashboardSpec,
	monitoringSpec cephv1.MonitoringSpec,
	resources v1.ResourceRequirements,
	ownerRef metav1.OwnerReference,
	dataDirHostPath string,
	isUpgrade bool,
) *Cluster {
	return &Cluster{
		clusterInfo:     clusterInfo,
		context:         context,
		Namespace:       namespace,
		placement:       placement,
		rookVersion:     rookVersion,
		cephVersion:     cephVersion,
		Replicas:        1,
		dataDir:         k8sutil.DataDir,
		dashboard:       dashboard,
		monitoringSpec:  monitoringSpec,
		Network:         network,
		resources:       resources,
		ownerRef:        ownerRef,
		exitCode:        getExitCode,
		dataDirHostPath: dataDirHostPath,
		isUpgrade:       isUpgrade,
	}
}

var updateDeploymentAndWait = mon.UpdateCephDeploymentAndWait

// Start begins the process of running a cluster of Ceph mgrs.
func (c *Cluster) Start() error {
	// Validate pod's memory if specified
	err := opspec.CheckPodMemory(c.resources, cephMgrPodMinimumMemory)
	if err != nil {
		return fmt.Errorf("%v", err)
	}

	logger.Infof("start running mgr")

	for i := 0; i < c.Replicas; i++ {
		if i >= 2 {
			logger.Errorf("cannot have more than 2 mgrs")
			break
		}

		daemonID := k8sutil.IndexToName(i)
		resourceName := fmt.Sprintf("%s-%s", appName, daemonID)
		mgrConfig := &mgrConfig{
			DaemonID:      daemonID,
			ResourceName:  resourceName,
			DashboardPort: c.dashboardPort(),
			DataPathMap:   config.NewStatelessDaemonDataPathMap(config.MgrType, daemonID, c.Namespace, c.dataDirHostPath),
		}

		// generate keyring specific to this mgr daemon saved to k8s secret
		if err := c.generateKeyring(mgrConfig); err != nil {
			return fmt.Errorf("failed to generate keyring for %s. %+v", resourceName, err)
		}

		if !c.needHttpBindFix() {
			c.clearHttpBindFix(mgrConfig)
		}

		// start the deployment
		d := c.makeDeployment(mgrConfig)
		logger.Debugf("starting mgr deployment: %+v", d)
		_, err := c.context.Clientset.AppsV1().Deployments(c.Namespace).Create(d)
		if err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create mgr deployment %s. %+v", resourceName, err)
			}
			logger.Infof("deployment for mgr %s already exists. updating if needed", resourceName)
			// Always invoke ceph version before an upgrade so we are sure to be up-to-date
			daemon := string(config.MgrType)
			var cephVersionToUse cephver.CephVersion

			// If this is not an upgrade there is no need to check the ceph version
			if c.isUpgrade {
				currentCephVersion, err := client.LeastUptodateDaemonVersion(c.context, c.clusterInfo.Name, daemon)
				if err != nil {
					logger.Warningf("failed to retrieve current ceph %s version. %+v", daemon, err)
					logger.Debug("could not detect ceph version during update, this is likely an initial bootstrap, proceeding with c.clusterInfo.CephVersion")
					cephVersionToUse = c.clusterInfo.CephVersion
				} else {
					logger.Debugf("current cluster version for mgrs before upgrading is: %+v", currentCephVersion)
					cephVersionToUse = currentCephVersion
				}
			}

			if err := updateDeploymentAndWait(c.context, d, c.Namespace, daemon, mgrConfig.DaemonID, cephVersionToUse, c.isUpgrade); err != nil {
				return fmt.Errorf("failed to update mgr deployment %s. %+v", resourceName, err)
			}
		}

		if err := c.configureOrchestratorModules(); err != nil {
			logger.Errorf("failed to enable orchestrator modules. %+v", err)
		}

		if err := c.enablePrometheusModule(c.Namespace); err != nil {
			logger.Errorf("failed to enable mgr prometheus module. %+v", err)
		}

		if err := c.configureDashboard(mgrConfig); err != nil {
			logger.Errorf("failed to enable mgr dashboard. %+v", err)
		}

	}

	// create the metrics service
	service := c.makeMetricsService(appName)
	if _, err := c.context.Clientset.CoreV1().Services(c.Namespace).Create(service); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create mgr service. %+v", err)
		}
		logger.Infof("mgr metrics service already exists")
	} else {
		logger.Infof("mgr metrics service started")
	}

	// enable monitoring if `monitoring: enabled: true`
	if c.monitoringSpec.Enabled {
		if c.clusterInfo.CephVersion.IsAtLeastNautilus() {
			logger.Infof("starting monitoring deployment")
			// servicemonitor takes some metadata from the service for easy mapping
			if err := c.enableServiceMonitor(service); err != nil {
				logger.Errorf("failed to enable service monitor. %+v", err)
			} else {
				logger.Infof("servicemonitor enabled")
			}
			// namespace in which the prometheusRule should be deployed
			// if left empty, it will be deployed in current namespace
			namespace := c.monitoringSpec.RulesNamespace
			if namespace == "" {
				namespace = c.Namespace
			}
			if err := c.deployPrometheusRule(prometheusRuleName, namespace); err != nil {
				logger.Errorf("failed to deploy prometheus rule. %+v", err)
			} else {
				logger.Infof("prometheusRule deployed")
			}
			logger.Debugf("ended monitoring deployment")
		} else {
			logger.Debugf("monitoring not supported for ceph versions <v%v", c.clusterInfo.CephVersion.Major)
		}
	}
	return nil
}

// Ceph docs about the prometheus module: http://docs.ceph.com/docs/master/mgr/prometheus/
func (c *Cluster) enablePrometheusModule(clusterName string) error {
	if err := client.MgrEnableModule(c.context, clusterName, prometheusModuleName, true); err != nil {
		return fmt.Errorf("failed to enable mgr prometheus module. %+v", err)
	}
	return nil
}

// add a servicemonitor that allows prometheus to scrape from the monitoring endpoint of the cluster
func (c *Cluster) enableServiceMonitor(service *v1.Service) error {
	name := service.GetName()
	namespace := service.GetNamespace()
	serviceMonitor, err := k8sutil.GetServiceMonitor(path.Join(monitoringPath, serviceMonitorFile))
	if err != nil {
		return fmt.Errorf("service monitor could not be enabled. %+v", err)
	}
	serviceMonitor.SetName(name)
	serviceMonitor.SetNamespace(namespace)
	k8sutil.SetOwnerRef(&serviceMonitor.ObjectMeta, &c.ownerRef)
	serviceMonitor.Spec.NamespaceSelector.MatchNames = []string{namespace}
	serviceMonitor.Spec.Selector.MatchLabels = service.GetLabels()
	if _, err := k8sutil.CreateOrUpdateServiceMonitor(serviceMonitor); err != nil {
		return fmt.Errorf("service monitor could not be enabled. %+v", err)
	}
	return nil
}

// deploy prometheusRule that adds alerting and/or recording rules to the cluster
func (c *Cluster) deployPrometheusRule(name, namespace string) error {
	version := strconv.Itoa(c.clusterInfo.CephVersion.Major)
	name = strings.Replace(name, "VERSION", version, 1)
	prometheusRuleFile := name + ".yaml"
	prometheusRuleFile = path.Join(monitoringPath, prometheusRuleFile)
	prometheusRule, err := k8sutil.GetPrometheusRule(prometheusRuleFile)
	if err != nil {
		return fmt.Errorf("prometheus rule could not be deployed. %+v", err)
	}
	prometheusRule.SetName(name)
	prometheusRule.SetNamespace(namespace)
	owners := append(prometheusRule.GetOwnerReferences(), c.ownerRef)
	k8sutil.SetOwnerRefs(&prometheusRule.ObjectMeta, owners)
	if _, err := k8sutil.CreateOrUpdatePrometheusRule(prometheusRule); err != nil {
		return fmt.Errorf("prometheus rule could not be deployed. %+v", err)
	}
	return nil
}
