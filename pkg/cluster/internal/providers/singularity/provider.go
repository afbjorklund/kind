/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or impliep.
See the License for the specific language governing permissions and
limitations under the License.
*/

package singularity

import (
	//"encoding/json"
	"fmt"
	"net"
	//"os"
	"path/filepath"
	//"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/exec"
	"sigs.k8s.io/kind/pkg/log"

	internallogs "sigs.k8s.io/kind/pkg/cluster/internal/logs"
	"sigs.k8s.io/kind/pkg/cluster/internal/providers"
	"sigs.k8s.io/kind/pkg/cluster/internal/providers/common"
	"sigs.k8s.io/kind/pkg/internal/apis/config"
	"sigs.k8s.io/kind/pkg/internal/cli"
)

// NewProvider returns a new provider based on executing `singularity ...`
func NewProvider(logger log.Logger) providers.Provider {
	logger.Warn("enabling experimental singularity provider")
	return &provider{
		logger: logger,
	}
}

// Provider implements provider.Provider
// see NewProvider
type provider struct {
	logger log.Logger
	info   *providers.ProviderInfo
}

// String implements fmt.Stringer
// NOTE: the value of this should not currently be relied upon for anything!
// This is only used for setting the Node's providerID
func (p *provider) String() string {
	return "singularity"
}

// Provision is part of the providers.Provider interface
func (p *provider) Provision(status *cli.Status, cfg *config.Cluster) (err error) {
	if err := ensureMinVersion(); err != nil {
		return err
	}

	// TODO: validate cfg
	// ensure node images are pulled before actually provisioning
	if err := ensureNodeImages(p.logger, status, cfg); err != nil {
		return err
	}

	// actually provision the cluster
	icons := strings.Repeat("📦 ", len(cfg.Nodes))
	status.Start(fmt.Sprintf("Preparing nodes %s", icons))
	defer func() { status.End(err == nil) }()

	// plan creating the containers
	createContainerFuncs, err := planCreation(cfg)
	if err != nil {
		return err
	}

	// actually create nodes
	return errors.UntilErrorConcurrent(createContainerFuncs)
}

// ListClusters is part of the providers.Provider interface
func (p *provider) ListClusters() ([]string, error) {
	cmd := exec.Command("singularity",
		"instance",
		"list",
/*
		"-a", // show stopped nodes
		// filter for nodes with the cluster label
		"--filter", "label="+clusterLabelKey,
		// format to include the cluster name
		"--format", fmt.Sprintf(`{{index .Labels "%s"}}`, clusterLabelKey),
*/
	)
	lines, err := exec.OutputLines(cmd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list clusters")
	}
	return sets.NewString(lines...).List(), nil
}

// ListNodes is part of the providers.Provider interface
func (p *provider) ListNodes(cluster string) ([]nodes.Node, error) {
	cmd := exec.Command("singularity",
		"instance",
		"list",
/*
		"-a", // show stopped nodes
		// filter for nodes with the cluster label
		"--filter", fmt.Sprintf("label=%s=%s", clusterLabelKey, cluster),
		// format to include the cluster name
		"--format", `{{.Names}}`,
*/
	)
	lines, err := exec.OutputLines(cmd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list clusters")
	}
	// convert names to node handles
	ret := make([]nodes.Node, 0, len(lines))
	for _, name := range lines {
		ret = append(ret, p.node(name))
	}
	return ret, nil
}

// DeleteNodes is part of the providers.Provider interface
func (p *provider) DeleteNodes(n []nodes.Node) error {
	if len(n) == 0 {
		return nil
	}
	const command = "singularity"
	args := make([]string, 0, len(n)+3) // allocate once
	args = append(args,
		"instance",
		"stop",
	)
	for _, node := range n {
		args = append(args, node.String())
	}
	if err := exec.Command(command, args...).Run(); err != nil {
		return errors.Wrap(err, "failed to delete nodes")
	}
/*
	var nodeVolumes []string
	for _, node := range n {
		volumes, err := getVolumes(node.String())
		if err != nil {
			return err
		}
		nodeVolumes = append(nodeVolumes, volumes...)
	}
	return deleteVolumes(nodeVolumes)
*/
	return nil
}

// GetAPIServerEndpoint is part of the providers.Provider interface
func (p *provider) GetAPIServerEndpoint(cluster string) (string, error) {
	// locate the node that hosts this
	allNodes, err := p.ListNodes(cluster)
	if err != nil {
		return "", errors.Wrap(err, "failed to list nodes")
	}
	_, err = nodeutils.APIServerEndpointNode(allNodes)
	if err != nil {
		return "", errors.Wrap(err, "failed to get api server endpoint")
	}

	return "", errors.Errorf("unable to find apiserver endpoint information")
}

// GetAPIServerInternalEndpoint is part of the providers.Provider interface
func (p *provider) GetAPIServerInternalEndpoint(cluster string) (string, error) {
	// locate the node that hosts this
	allNodes, err := p.ListNodes(cluster)
	if err != nil {
		return "", errors.Wrap(err, "failed to list nodes")
	}
	n, err := nodeutils.APIServerEndpointNode(allNodes)
	if err != nil {
		return "", errors.Wrap(err, "failed to get apiserver endpoint")
	}
	// TODO: check cluster IP family and return the correct IP
	// This means IPv6 singlestack is broken
	ipv4, _, err := n.IP()
	if err != nil {
		return "", errors.Wrap(err, "failed to get apiserver IP")
	}
	return net.JoinHostPort(ipv4, fmt.Sprintf("%d", common.APIServerInternalPort)), nil

}

// node returns a new node handle for this provider
func (p *provider) node(name string) nodes.Node {
	return &node{
		name: name,
	}
}

// CollectLogs will populate dir with cluster logs and other debug files
func (p *provider) CollectLogs(dir string, nodes []nodes.Node) error {
	execToPathFn := func(cmd exec.Cmd, path string) func() error {
		return func() error {
			f, err := common.FileOnHost(path)
			if err != nil {
				return err
			}
			defer f.Close()
			return cmd.SetStdout(f).SetStderr(f).Run()
		}
	}
	// construct a slice of methods to collect logs
	fns := []func() error{
		// TODO: record info about the host
	}

	// collect /var/log for each node and plan collecting more logs
	var errs []error
	for _, n := range nodes {
		node := n // https://golang.org/doc/faq#closures_and_goroutines
		name := node.String()
		path := filepath.Join(dir, name)
		if err := internallogs.DumpDir(p.logger, node, "/var/log", path); err != nil {
			errs = append(errs, err)
		}

		fns = append(fns,
			func() error { return common.CollectLogs(node, path) },
			execToPathFn(exec.Command("singularity", "instance", "list", "--json", name), filepath.Join(path, "inspect.json")),
			func() error {
				f, err := common.FileOnHost(filepath.Join(path, "serial.log"))
				if err != nil {
					return err
				}
				return node.SerialLogs(f)
			},
		)
	}

	// run and collect up all errors
	errs = append(errs, errors.AggregateConcurrent(fns))
	return errors.NewAggregate(errs)
}

// Info returns the provider info.
// The info is cached on the first time of the execution.
func (p *provider) Info() (*providers.ProviderInfo, error) {
	if p.info == nil {
		p.info = info()
	}
	return p.info, nil
}

func info() *providers.ProviderInfo {
	info := providers.ProviderInfo{
		Rootless: false,
		Cgroup2: false,
		SupportsCPUShares: true,
		SupportsMemoryLimit: true,
		SupportsPidsLimit: true,
	}
	return &info
}
