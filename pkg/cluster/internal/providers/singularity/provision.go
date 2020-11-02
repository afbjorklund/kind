/*
Copyright 2020 The Kubernetes Authors.

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

package singularity

import (
	"fmt"
	"net"
	//"path/filepath"
	"strings"

	"sigs.k8s.io/kind/pkg/cluster/constants"
	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/exec"

	"sigs.k8s.io/kind/pkg/cluster/internal/loadbalancer"
	"sigs.k8s.io/kind/pkg/cluster/internal/providers/common"
	"sigs.k8s.io/kind/pkg/internal/apis/config"
)

// planCreation creates a slice of funcs that will create the containers
func planCreation(cfg *config.Cluster) (createContainerFuncs []func() error, err error) {
	// these apply to all container creation
	nodeNamer := common.MakeNodeNamer(cfg.Name)
	genericArgs, err := commonArgs(cfg)
	if err != nil {
		return nil, err
	}

	// only the external LB should reflect the port if we have multiple control planes
	apiServerPort := cfg.Networking.APIServerPort
	apiServerAddress := cfg.Networking.APIServerAddress
	if clusterHasImplicitLoadBalancer(cfg) {
		apiServerPort = 0              // replaced with random ports
		apiServerAddress = "127.0.0.1" // only the LB needs to be non-local
		if clusterIsIPv6(cfg) {
			apiServerAddress = "::1" // only the LB needs to be non-local
		}
		// plan loadbalancer node
		name := nodeNamer(constants.ExternalLoadBalancerNodeRoleValue)
		createContainerFuncs = append(createContainerFuncs, func() error {
			args, err := runArgsForLoadBalancer(cfg, name, genericArgs)
			if err != nil {
				return err
			}
			return createContainer(args)
		})
	}

	// plan normal nodes
	for _, node := range cfg.Nodes {
		node := node.DeepCopy()              // copy so we can modify
		name := nodeNamer(string(node.Role)) // name the node

		// plan actual creation based on role
		switch node.Role {
		case config.ControlPlaneRole:
			createContainerFuncs = append(createContainerFuncs, func() error {
				node.ExtraPortMappings = append(node.ExtraPortMappings,
					config.PortMapping{
						ListenAddress: apiServerAddress,
						HostPort:      apiServerPort,
						ContainerPort: common.APIServerInternalPort,
					},
				)
				args, err := runArgsForNode(node, cfg.Networking.IPFamily, name, genericArgs)
				if err != nil {
					return err
				}
				return createContainer(args)
			})
		case config.WorkerRole:
			createContainerFuncs = append(createContainerFuncs, func() error {
				args, err := runArgsForNode(node, cfg.Networking.IPFamily, name, genericArgs)
				if err != nil {
					return err
				}
				return createContainer(args)
			})
		default:
			return nil, errors.Errorf("unknown node role: %q", node.Role)
		}
	}
	return createContainerFuncs, nil
}

func createContainer(args []string) error {
	if err := exec.Command("singularity", args...).Run(); err != nil {
		return errors.Wrap(err, "singularity run error")
	}
	return nil
}

func clusterIsIPv6(cfg *config.Cluster) bool {
	return cfg.Networking.IPFamily == "ipv6"
}

func clusterHasImplicitLoadBalancer(cfg *config.Cluster) bool {
	controlPlanes := 0
	for _, configNode := range cfg.Nodes {
		role := string(configNode.Role)
		if role == constants.ControlPlaneNodeRoleValue {
			controlPlanes++
		}
	}
	return controlPlanes > 1
}

// commonArgs computes static arguments that apply to all containers
func commonArgs(cfg *config.Cluster) ([]string, error) {
	// standard arguments all nodes containers need, computed once
	args := []string{
/*
		// label the node with the cluster ID
		"--label", fmt.Sprintf("%s=%s", clusterLabelKey, cfg.Name),
*/
	}

	// enable IPv6 if necessary
	if clusterIsIPv6(cfg) {
		args = append(args, "--sysctl=net.ipv6.conf.all.disable_ipv6=0", "--sysctl=net.ipv6.conf.all.forwarding=1")
	}

	// pass proxy environment variables
	proxyEnv, err := getProxyEnv(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "proxy setup error")
	}
	for key, val := range proxyEnv {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, val))
	}

	return args, nil
}

func runArgsForNode(node *config.Node, clusterIPFamily config.ClusterIPFamily, name string, args []string) ([]string, error) {
/*
	// Pre-create anonymous volumes to enable specifying mount options
	// during container run time
	varVolume, err := createAnonymousVolume(name)
	if err != nil {
		return nil, err
	}
*/
	args = append([]string{
		"instance",
		"start",
		"--hostname", name, // make hostname match container name
/*
		// label the node with the role ID
		"--label", fmt.Sprintf("%s=%s", nodeRoleLabelKey, node.Role),
*/
		// running containers in a container requires privileged
		"--keep-privs",
/*
		// runtime persistent storage
		// this ensures that E.G. pods, logs etc. are not on the container
		// filesystem, which is not only better for performance, but allows
		// running kind in kind for "party tricks"
		// (please don't depend on doing this though!)
		// also enable default docker volume options
		// suid: SUID applications on the volume will be able to change their privilege
		// exec: executables on the volume will be able to executed within the container
		// dev: devices on the volume will be able to be used by processes within the container
		"--volume", fmt.Sprintf("%s:/var:suid,exec,dev", varVolume),
*/
		// some k8s things want to read /lib/modules
		"-B", "/lib/modules:/lib/modules:ro",
	},
		args...,
	)

	// convert mounts and port mappings to container run args
	args = append(args, generateMountBindings(node.ExtraMounts...)...)
	mappingArgs, err := generatePortMappings(clusterIPFamily, node.ExtraPortMappings...)
	if err != nil {
		return nil, err
	}
	args = append(args, mappingArgs...)

	_, image := sanitizeImage(node.Image)
	args = append(args, image)

	// finally, specify the name
	return append(args, name), nil
}

func runArgsForLoadBalancer(cfg *config.Cluster, name string, args []string) ([]string, error) {
	args = append([]string{
		"run",
		"--hostname", name, // make hostname match container name
		"--name", name, // ... and set the container name
		// label the node with the role ID
		"--label", fmt.Sprintf("%s=%s", nodeRoleLabelKey, constants.ExternalLoadBalancerNodeRoleValue),
	},
		args...,
	)

	// load balancer port mapping
	mappingArgs, err := generatePortMappings(cfg.Networking.IPFamily,
		config.PortMapping{
			ListenAddress: cfg.Networking.APIServerAddress,
			HostPort:      cfg.Networking.APIServerPort,
			ContainerPort: common.APIServerInternalPort,
		},
	)
	if err != nil {
		return nil, err
	}
	args = append(args, mappingArgs...)

	// finally, specify the image to run
	_, image := sanitizeImage(loadbalancer.Image)
	return append(args, image), nil
}

func getProxyEnv(cfg *config.Cluster) (map[string]string, error) {
	envs := common.GetProxyEnvs(cfg)
	// Specifically add the singularity network subnets to NO_PROXY if we are using a proxy
	if len(envs) > 0 {
		subnets, err := getSubnets("bridge")
		if err != nil {
			return nil, err
		}
		noProxyList := append(subnets, envs[common.NOProxy])
		// Add pod and service dns names to no_proxy to allow in cluster
		// Note: this is best effort based on the default CoreDNS spec
		// https://github.com/kubernetes/dns/blob/master/docs/specification.md
		// Any user created pod/service hostnames, namespaces, custom DNS services
		// are expected to be no-proxied by the user explicitly.
		noProxyList = append(noProxyList, ".svc", ".svc.cluster", ".svc.cluster.local")
		noProxyJoined := strings.Join(noProxyList, ",")
		envs[common.NOProxy] = noProxyJoined
		envs[strings.ToLower(common.NOProxy)] = noProxyJoined
	}
	return envs, nil
}

func getSubnets(networkName string) ([]string, error) {
	return nil, fmt.Errorf("failed to get subnets")
}

// generateMountBindings converts the mount list to a list of args for singularity
// '<HostPath>:<ContainerPath>[:options]', where 'options'
// is a comma-separated list of the following strings:
// 'ro', if the path is read only
// 'Z', if the volume requires SELinux relabeling
func generateMountBindings(mounts ...config.Mount) []string {
	args := make([]string, 0, len(mounts))
	for _, m := range mounts {
		bind := fmt.Sprintf("%s:%s", m.HostPath, m.ContainerPath)
		var attrs []string
		if m.Readonly {
			attrs = append(attrs, "ro")
		}
		// Only request relabeling if the pod provides an SELinux context. If the pod
		// does not provide an SELinux context relabeling will label the volume with
		// the container's randomly allocated MCS label. This would restrict access
		// to the volume to the container which mounts it first.
		if m.SelinuxRelabel {
			attrs = append(attrs, "Z")
		}
		switch m.Propagation {
		case config.MountPropagationNone:
			// noop, private is default
		case config.MountPropagationBidirectional:
			attrs = append(attrs, "rshared")
		case config.MountPropagationHostToContainer:
			attrs = append(attrs, "rslave")
		default: // Falls back to "private"
		}
		if len(attrs) > 0 {
			bind = fmt.Sprintf("%s:%s", bind, strings.Join(attrs, ","))
		}
		args = append(args, fmt.Sprintf("--volume=%s", bind))
	}
	return args
}

// generatePortMappings converts the portMappings list to a list of args for singularity
func generatePortMappings(clusterIPFamily config.ClusterIPFamily, portMappings ...config.PortMapping) ([]string, error) {
	args := make([]string, 0, len(portMappings))
	for _, pm := range portMappings {
		// do provider internal defaulting
		// in a future API revision we will handle this at the API level and remove this
		if pm.ListenAddress == "" {
			switch clusterIPFamily {
			case config.IPv4Family:
				pm.ListenAddress = "0.0.0.0"
			case config.IPv6Family:
				pm.ListenAddress = "::"
			default:
				return nil, errors.Errorf("unknown cluster IP family: %v", clusterIPFamily)
			}
		}
		if string(pm.Protocol) == "" {
			pm.Protocol = config.PortMappingProtocolTCP // TCP is the default
		}

		// validate that the provider can handle this binding
		switch pm.Protocol {
		case config.PortMappingProtocolTCP:
		case config.PortMappingProtocolUDP:
		case config.PortMappingProtocolSCTP:
		default:
			return nil, errors.Errorf("unknown port mapping protocol: %v", pm.Protocol)
		}

		// get a random port if necessary (port = 0)
		hostPort, err := common.PortOrGetFreePort(pm.HostPort, pm.ListenAddress)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get random host port for port mapping")
		}

		// generate the actual mapping arg
		protocol := string(pm.Protocol)
		hostPortBinding := net.JoinHostPort(pm.ListenAddress, fmt.Sprintf("%d", hostPort))
		// Podman expects empty string instead of 0 to assign a random port
		// https://github.com/containers/libpod/blob/master/pkg/spec/ports.go#L68-L69
		if strings.HasSuffix(hostPortBinding, ":0") {
			hostPortBinding = strings.TrimSuffix(hostPortBinding, "0")
		}
		args = append(args, fmt.Sprintf("--publish=%s:%d/%s", hostPortBinding, pm.ContainerPort, strings.ToLower(protocol)))
	}
	return args, nil
}
