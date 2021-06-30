/*
Copyright 2019 The Kubernetes Authors.

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

// Package deployer implements the kubetest2 GKE deployer
package deployer

import (
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/octago/sflags/gen/gpflag"
	"github.com/spf13/pflag"
	"k8s.io/klog"
	"sigs.k8s.io/boskos/client"

	"sigs.k8s.io/kubetest2/kubetest2-gke/deployer/options"
	"sigs.k8s.io/kubetest2/pkg/build"
	"sigs.k8s.io/kubetest2/pkg/types"
)

// Name is the name of the deployer
const Name = "gke"

const (
	e2eAllow            = "tcp:22,tcp:80,tcp:8080,tcp:30000-32767,udp:30000-32767"
	defaultImage        = "cos"
	defaultWindowsImage = WindowsImageTypeLTSC
)

const (
	gceStockoutErrorPattern = ".*does not have enough resources available to fulfill.*"
)

type privateClusterAccessLevel string

const (
	no           privateClusterAccessLevel = "no"
	limited      privateClusterAccessLevel = "limited"
	unrestricted privateClusterAccessLevel = "unrestricted"
)

var (
	// poolRe matches instance group URLs of the form `https://www.googleapis.com/compute/v1/projects/some-project/zones/a-zone/instanceGroupManagers/gke-some-cluster-some-pool-90fcb815-grp`
	// or `https://www.googleapis.com/compute/v1/projects/some-project/zones/a-zone/instanceGroupManagers/gk3-some-cluster-some-pool-90fcb815-grp` for GKE in Autopilot mode.
	// Match meaning:
	// m[0]: path starting with zones/
	// m[1]: zone
	// m[2]: pool name (passed to e2es)
	// m[3]: unique hash (used as nonce for firewall rules)
	poolRe = regexp.MustCompile(`zones/([^/]+)/instanceGroupManagers/(gk[e|3]-.*-([0-9a-f]{8})-grp)$`)

	urlRe = regexp.MustCompile(`https://.*/`)

	defaultNodePool = gkeNodePool{
		Nodes:       3,
		MachineType: "n1-standard-2",
	}

	defaultWindowsNodePool = gkeNodePool{
		Nodes:       1,
		MachineType: "n1-standard-2",
	}
)

type gkeNodePool struct {
	Nodes       int
	MachineType string
}

type ig struct {
	path string
	zone string
	name string
	uniq string
}

type cluster struct {
	// index is the index of the cluster in the list provided via the --cluster-name flag
	index int
	name  string
}

type Deployer struct {
	// generic parts
	kubetest2CommonOptions types.Options

	*options.BuildOptions
	*options.CommonOptions
	*options.UpOptions

	// doInit helps to make sure the initialization is performed only once
	doInit sync.Once
	// only used for multi-project multi-cluster profile to save the project-clusters mapping
	projectClustersLayout map[string][]cluster
	// project -> cluster -> instance groups
	instanceGroups map[string]map[string][]*ig

	kubecfgPath  string
	testPrepared bool

	localLogsDir string
	gcsLogsDir   string

	// gke specific details for retrying
	totalTryCount                        int
	retryCount                           int
	retryableErrorPatternsCompiled       []*regexp.Regexp
	subnetworkRangesInternal             [][]string
	privateClusterMasterIPRangesInternal [][]string

	// boskos struct field will be non-nil when the deployer is
	// using boskos to acquire a GCP project
	boskos *client.Client

	// this channel serves as a signal channel for the hearbeat goroutine
	// so that it can be explicitly closed
	boskosHeartbeatClose chan struct{}
}

// assert that New implements types.NewDeployer
var _ types.NewDeployer = New

// assert that deployer implements types.Deployer
var _ types.Deployer = &Deployer{}

func (d *Deployer) Provider() string {
	return Name
}

// New implements deployer.New for gke
func New(opts types.Options) (types.Deployer, *pflag.FlagSet) {
	// create a deployer object and set fields that are not flag controlled
	d := &Deployer{
		kubetest2CommonOptions: opts,
		BuildOptions: &options.BuildOptions{
			CommonBuildOptions: &build.Options{
				Builder:  &build.NoopBuilder{},
				Stager:   &build.NoopStager{},
				Strategy: "make",
			},
		},
		CommonOptions: &options.CommonOptions{
			Network:     "default",
			Environment: "prod",
		},
		UpOptions: &options.UpOptions{
			NumClusters:        1,
			NumNodes:           defaultNodePool.Nodes,
			MachineType:        defaultNodePool.MachineType,
			ImageType:          defaultImage,
			WindowsNumNodes:    defaultWindowsNodePool.Nodes,
			WindowsMachineType: defaultWindowsNodePool.MachineType,
			WindowsImageType:   defaultWindowsImage,
			// Leave Version as empty to use the default cluster version.
			Version:          "",
			GCPSSHKeyIgnored: true,

			BoskosLocation:                 defaultBoskosLocation,
			BoskosResourceType:             defaultGKEProjectResourceType,
			BoskosAcquireTimeoutSeconds:    defaultBoskosAcquireTimeoutSeconds,
			BoskosHeartbeatIntervalSeconds: defaultBoskosHeartbeatIntervalSeconds,
			BoskosProjectsRequested:        1,

			RetryableErrorPatterns: []string{gceStockoutErrorPattern},
		},
		localLogsDir: filepath.Join(opts.RunDir(), "logs"),
	}

	// register flags
	fs := bindFlags(d)

	// register flags for klog
	klog.InitFlags(nil)
	fs.AddGoFlagSet(flag.CommandLine)
	return d, fs
}

func (d *Deployer) VerifyLocationFlags() error {
	if len(d.Zones) == 0 && len(d.Regions) == 0 {
		return fmt.Errorf("--zone or --region must be set for GKE deployment")
	} else if len(d.Zones) != 0 && len(d.Regions) != 0 {
		return fmt.Errorf("--zone and --region cannot both be set")
	}
	return nil
}

// locationFlag builds the zone/region flag from the provided zone/region
// used by gcloud commands.
func locationFlag(regions, zones []string, retryCount int) string {
	if len(zones) != 0 {
		return "--zone=" + zones[retryCount]
	}
	return "--region=" + regions[retryCount]
}

// regionFromLocation computes the region from the specified zone/region
// used by some commands (such as subnets), which do not support zones.
func regionFromLocation(regions, zones []string, retryCount int) string {
	if len(zones) != 0 {
		zone := zones[retryCount]
		return zone[0:strings.LastIndex(zone, "-")]
	}
	return regions[retryCount]
}

func bindFlags(d *Deployer) *pflag.FlagSet {
	flags, err := gpflag.Parse(d)
	if err != nil {
		klog.Fatalf("unable to generate flags from deployer")
		return nil
	}

	return flags
}
